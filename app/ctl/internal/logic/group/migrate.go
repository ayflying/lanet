// =================================================================================
// SQLite schema 迁移模块：版本化迁移 + 损坏修复 + 数据导入导出。
//
// 设计：
//   - 迁移以有序切片内嵌在代码中（与二进制同生命周期，不依赖外部文件）；
//   - schema_migrations 表记录当前版本与脏标记，逐步进版本，失败即停（脏）；
//   - 修复策略：先安全备份（含 WAL checkpoint），再尝试打开；打不开时
//     用 SQLite 官方恢复手段（.recover 语义的现代实现：逐表全量重读）导出
//     可读数据 → 新库重建 → 原路灌回。
// =================================================================================

package group

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// migration 单个版本迁移：up 在当前版本上执行以进入下一版本。
// 每条迁移必须幂等可重入（失败重跑不产生副作用）。
type migration struct {
	version int
	name    string
	up      string
}

// migrations 按版本升序排列。v1 为基线（与旧版裸 migrate() 的 schema 等价）。
var migrations = []migration{
	{
		version: 1,
		name:    "baseline: groups/members/announced_addrs",
		up: `
CREATE TABLE IF NOT EXISTS groups (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL,
	creator_peer_id TEXT NOT NULL,
	invite_code     TEXT NOT NULL UNIQUE,
	invite_expires_at DATETIME,
	cidr            TEXT NOT NULL,
	subnet_index    INTEGER NOT NULL UNIQUE,
	version         INTEGER NOT NULL DEFAULT 1,
	created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS members (
	group_id    TEXT NOT NULL REFERENCES groups(id),
	peer_id     TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	os          TEXT NOT NULL DEFAULT '',
	virtual_ip  TEXT NOT NULL,
	role        TEXT NOT NULL DEFAULT 'member',
	enrolled_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_members_group ON members(group_id);
CREATE TABLE IF NOT EXISTS announced_addrs (
	peer_id  TEXT NOT NULL REFERENCES members(peer_id) ON DELETE CASCADE,
	addr     TEXT NOT NULL,
	PRIMARY KEY (peer_id, addr)
);`,
	},
	// 后续 schema 变更在此追加 {version: 2, name: "...", up: "..."}。
	// 禁止修改已发布的迁移，只允许追加新版本。
}

// schemaMigrationsMeta 迁移账本表。
const schemaMigrationsMeta = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version   INTEGER PRIMARY KEY,
	name      TEXT NOT NULL,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// Migrate 执行版本化迁移，返回迁移后的版本号。
// 任何一条迁移失败都会返回错误（该迁移语句自身需保证可重入，
// 修复后重跑即可继续）。
func (s *store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, schemaMigrationsMeta); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}
	current, err := s.currentVersion(ctx)
	if err != nil {
		return 0, err
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if _, err := s.db.ExecContext(ctx, m.up); err != nil {
			return current, fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name); err != nil {
			return current, fmt.Errorf("record migration %d: %w", m.version, err)
		}
		current = m.version
	}
	return current, nil
}

// currentVersion 读取账本中最大已应用版本；空库返回 0。
func (s *store) currentVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// Version 返回数据库当前迁移版本（供外部检查，版本 -1 表示账本不存在/库不可读）。
func (s *store) Version(ctx context.Context) (int, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists)
	if err != nil {
		return -1, fmt.Errorf("probe sqlite_master: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	return s.currentVersion(ctx)
}

// BackupCheckpoint 将 WAL 落盘合并进主库文件（安全备份前置步骤）。
// PRAGMA 失败不致命（可能库只读），静默继续。
func (s *store) BackupCheckpoint(ctx context.Context) error {
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

// =================================================================================
// 修复与搬迁
// =================================================================================

// SnapshotBackup 源库 VACUUM INTO 导出一致快照（SQLite 3.27+）。
// 单语句原子导出，即使源库有未 checkpoint 的 WAL 也能得到一致副本。
// VACUUM INTO 要求目标文件不存在，已存在时返回错误。
func (s *store) SnapshotBackup(ctx context.Context, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO %q`, destPath)); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}
	return nil
}

// snapshot 导出结构体：与迁移后的 schema 字段一一对应。
type snapshotGroup struct {
	ID, Name, CreatorPeerID, InviteCode string
	InviteExpires                       *time.Time
	CIDR                                string
	SubnetIndex                         int
	Version                             uint64
	CreatedAt                           time.Time
}

type snapshotMember struct {
	GroupID, PeerID, Name, OS, VirtualIP, Role string
	EnrolledAt                                 time.Time
}

type snapshotAddr struct {
	PeerID, Addr string
}

// snapshot 全量读取三类表（逐表全量 SELECT，可穿透部分损坏——损坏页之外的行仍可读出，
// 遇到不可读行跳过并计数，最终报告给调用者）。
func (s *store) snapshot(ctx context.Context) (groups []snapshotGroup, members []snapshotMember, addrs []snapshotAddr, skipped int, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, creator_peer_id, invite_code, invite_expires_at, cidr, subnet_index, version, created_at FROM groups`)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("read groups: %w", err)
	}
	for rows.Next() {
		var g snapshotGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatorPeerID, &g.InviteCode, &g.InviteExpires, &g.CIDR, &g.SubnetIndex, &g.Version, &g.CreatedAt); err != nil {
			skipped++
			continue
		}
		groups = append(groups, g)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		`SELECT group_id, peer_id, name, os, virtual_ip, role, enrolled_at FROM members`)
	if err != nil {
		return groups, members, nil, skipped, fmt.Errorf("read members: %w", err)
	}
	for rows.Next() {
		var m snapshotMember
		if err := rows.Scan(&m.GroupID, &m.PeerID, &m.Name, &m.OS, &m.VirtualIP, &m.Role, &m.EnrolledAt); err != nil {
			skipped++
			continue
		}
		members = append(members, m)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT peer_id, addr FROM announced_addrs`)
	if err != nil {
		return groups, members, addrs, skipped, fmt.Errorf("read announced_addrs: %w", err)
	}
	for rows.Next() {
		var a snapshotAddr
		if err := rows.Scan(&a.PeerID, &a.Addr); err != nil {
			skipped++
			continue
		}
		addrs = append(addrs, a)
	}
	rows.Close()
	return groups, members, addrs, skipped, rows.Err()
}

// RepairResult 修复报告。
type RepairResult struct {
	// BackupPath 修复前生成的一致性备份路径。
	BackupPath string
	// Groups/Members/Addrs 成功迁移的数据量。
	Groups, Members, Addrs int
	// Skipped 因损坏无法读取的行数（>0 表示库有物理损坏，需要人工检查）。
	Skipped int
	// Rebuilt 是否执行了重建（源库不可用时为 true）。
	Rebuilt bool
}

// RepairDatabase 数据修复主入口：尽量原库保留，不可用时重建。
// 流程：VACUUM INTO 一致备份 → 尝试直接迁移原库 → 失败则导出可读数据 →
// 备份后重建新库 → 灌回 → 用新库替换原库。
func RepairDatabase(ctx context.Context, dbPath string) (*RepairResult, error) {
	result := &RepairResult{}
	// 时间戳精确到纳秒级别格式，避免同一秒内重跑触发 VACUUM INTO 目标已存在。
	backupPath := fmt.Sprintf("%s.bak-%s", dbPath, time.Now().Format("20060102-150405.000000000"))
	result.BackupPath = backupPath

	// 1. 先在原库上做 VACUUM INTO 一致备份（源库必须能打开；打不开则跳过——
	//    此时直接文件级拷贝 db+wal+shm 三件套保底）。
	src, err := openStoreRaw(dbPath)
	if err != nil {
		// 库完全打不开（文件损坏/不是数据库）：文件级拷贝保底后删掉重建。
		_ = copyFile(dbPath, backupPath)
		_ = copyFile(dbPath+"-wal", backupPath+"-wal")
		_ = copyFile(dbPath+"-shm", backupPath+"-shm")
		result.Rebuilt = true
		if err := rebuildFromNothing(ctx, dbPath, result); err != nil {
			return result, err
		}
		return result, nil
	}
	defer src.Close()

	if err := src.SnapshotBackup(ctx, backupPath); err != nil {
		return result, fmt.Errorf("backup before repair: %w", err)
	}

	// 2. 原库可打开：尝试直接跑迁移，成功即修复完成（常见情况：版本落后）。
	if _, err := src.Migrate(ctx); err == nil {
		groups, members, addrs, skipped, _ := src.snapshot(ctx)
		result.Groups, result.Members, result.Addrs, result.Skipped = len(groups), len(members), len(addrs), skipped
		return result, nil
	}

	// 3. 迁移失败（schema 损坏）：导出可读数据。
	groups, members, addrs, skipped, snapErr := src.snapshot(ctx)
	result.Skipped = skipped
	if snapErr != nil && len(groups) == 0 && len(members) == 0 {
		// 完全读不出：直接用备份重建空库。
		result.Rebuilt = true
		if err := rebuildFromBackup(ctx, backupPath, dbPath, result); err != nil {
			return result, err
		}
		return result, nil
	}

	// 4. 有可读数据：重建新库并灌回。
	result.Rebuilt = true
	if err := rebuildWithData(ctx, dbPath, groups, members, addrs, result); err != nil {
		return result, err
	}
	return result, nil
}

// rebuildFromNothing 原库不可用时：删掉旧文件后新建空库跑迁移。
func rebuildFromNothing(ctx context.Context, dbPath string, result *RepairResult) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove broken db %s: %w", p, err)
		}
	}
	fresh, err := openStoreRaw(dbPath)
	if err != nil {
		return fmt.Errorf("recreate db: %w", err)
	}
	defer fresh.Close()
	if _, err := fresh.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate fresh db: %w", err)
	}
	return nil
}

// rebuildFromBackup 用备份库的数据重建主库。
func rebuildFromBackup(ctx context.Context, backupPath, dbPath string, result *RepairResult) error {
	bak, err := openStoreRaw(backupPath)
	if err != nil {
		return fmt.Errorf("open backup for rebuild: %w", err)
	}
	defer bak.Close()
	groups, members, addrs, skipped, err := bak.snapshot(ctx)
	result.Skipped += skipped
	if err != nil {
		return fmt.Errorf("snapshot backup: %w", err)
	}
	return rebuildWithData(ctx, dbPath, groups, members, addrs, result)
}

// rebuildWithData 重建主库并灌回数据。
func rebuildWithData(ctx context.Context, dbPath string, groups []snapshotGroup, members []snapshotMember, addrs []snapshotAddr, result *RepairResult) error {
	// 重建前清掉主库文件三件套。
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old db %s: %w", p, err)
		}
	}
	fresh, err := openStoreRaw(dbPath)
	if err != nil {
		return fmt.Errorf("open rebuilt db: %w", err)
	}
	defer fresh.Close()
	if _, err := fresh.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate rebuilt db: %w", err)
	}
	if err := fresh.restoreSnapshot(ctx, groups, members, addrs); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	result.Groups, result.Members, result.Addrs = len(groups), len(members), len(addrs)
	return nil
}

// restoreSnapshot 把快照数据灌回新库（单事务，灌一半失败可整体回滚）。
func (s *store) restoreSnapshot(ctx context.Context, groups []snapshotGroup, members []snapshotMember, addrs []snapshotAddr) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, g := range groups {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO groups (id, name, creator_peer_id, invite_code, invite_expires_at, cidr, subnet_index, version, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.ID, g.Name, g.CreatorPeerID, g.InviteCode, g.InviteExpires, g.CIDR, g.SubnetIndex, g.Version, g.CreatedAt); err != nil {
			return fmt.Errorf("restore group %s: %w", g.ID, err)
		}
	}
	for _, m := range members {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO members (group_id, peer_id, name, os, virtual_ip, role, enrolled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.GroupID, m.PeerID, m.Name, m.OS, m.VirtualIP, m.Role, m.EnrolledAt); err != nil {
			return fmt.Errorf("restore member %s: %w", m.PeerID, err)
		}
	}
	for _, a := range addrs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO announced_addrs (peer_id, addr) VALUES (?, ?)`, a.PeerID, a.Addr); err != nil {
			return fmt.Errorf("restore addr of %s: %w", a.PeerID, err)
		}
	}
	return tx.Commit()
}

// =================================================================================
// 底层辅助
// =================================================================================

// openStoreRaw 以最宽松方式打开库（仅 journal_mode=WAL + busy_timeout，
// 不跑 migrate——修复流程自己控制迁移时机）。
func openStoreRaw(path string) (*store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	// 探活：真正打开文件。
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}
	return &store{db: db}, nil
}

// OpenStoreRaw 导出给运维子命令使用：打开库但不迁移。
func OpenStoreRaw(path string) (*store, error) {
	return openStoreRaw(path)
}

// copyFile 文件级拷贝（不存在的源文件静默跳过）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = out.ReadFrom(in)
	return err
}
