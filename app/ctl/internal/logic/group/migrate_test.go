package group

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// =================================================================================
// 版本化迁移
// =================================================================================

// TestMigrateVersionProgress 新库跑迁移后版本应到达最新，账本记录正确。
func TestMigrateVersionProgress(t *testing.T) {
	ctx := context.Background()
	st, err := openStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()

	want := migrations[len(migrations)-1].version
	got, err := st.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, want).Scan(&name); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if name != migrations[len(migrations)-1].name {
		t.Fatalf("ledger name = %q, want %q", name, migrations[len(migrations)-1].name)
	}
}

// TestMigrateIdempotent 重复打开同一库，迁移必须幂等无副作用。
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	first, err := openStore(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first.Close()

	second, err := openStore(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()

	got, err := second.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	want := migrations[len(migrations)-1].version
	if got != want {
		t.Fatalf("version after reopen = %d, want %d", got, want)
	}
}

// TestLegacyDBCompat 旧版裸 migrate() 建的库（无 schema_migrations 账本、
// 有业务表和数据）打开后应自动补账本并到最新版本，数据完好。
func TestLegacyDBCompat(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	// 模拟旧版库：直接建旧 schema + 插入数据，不建账本。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	legacySchema := `
CREATE TABLE groups (
	id TEXT PRIMARY KEY, name TEXT NOT NULL, creator_peer_id TEXT NOT NULL,
	invite_code TEXT NOT NULL UNIQUE, invite_expires_at DATETIME,
	cidr TEXT NOT NULL, subnet_index INTEGER NOT NULL UNIQUE,
	version INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE members (
	group_id TEXT NOT NULL REFERENCES groups(id), peer_id TEXT PRIMARY KEY,
	name TEXT NOT NULL, os TEXT NOT NULL DEFAULT '', virtual_ip TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	enrolled_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_members_group ON members(group_id);
CREATE TABLE announced_addrs (
	peer_id TEXT NOT NULL REFERENCES members(peer_id) ON DELETE CASCADE,
	addr TEXT NOT NULL, PRIMARY KEY (peer_id, addr)
);`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO groups (id, name, creator_peer_id, invite_code, cidr, subnet_index)
		VALUES ('g0','legacy','peer-a','CODE123456','10.7.0.0/24',0)`); err != nil {
		t.Fatalf("insert legacy group: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO members (group_id, peer_id, name, virtual_ip, role)
		VALUES ('g0','peer-a','owner-a','10.7.0.2','owner')`); err != nil {
		t.Fatalf("insert legacy member: %v", err)
	}
	raw.Close()

	// 用新代码打开：应补账本、跑迁移、保数据。
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("open legacy with new code: %v", err)
	}
	defer st.Close()

	got, err := st.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != migrations[len(migrations)-1].version {
		t.Fatalf("version = %d, want %d", got, migrations[len(migrations)-1].version)
	}
	groups, members, addrs, skipped, err := st.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != "g0" {
		t.Fatalf("groups = %+v, want 1 legacy group", groups)
	}
	if len(members) != 1 || members[0].PeerID != "peer-a" {
		t.Fatalf("members = %+v, want 1 legacy member", members)
	}
	if len(addrs) != 0 || skipped != 0 {
		t.Fatalf("addrs=%d skipped=%d, want 0/0", len(addrs), skipped)
	}
}

// =================================================================================
// 备份
// =================================================================================

// TestSnapshotBackup 备份文件应可独立打开且数据一致。
func TestSnapshotBackup(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer st.Close()

	// 写入数据（不经 Registry，直接 SQL 保持测试聚焦迁移模块）。
	if _, err := st.db.Exec(`INSERT INTO groups (id, name, creator_peer_id, invite_code, cidr, subnet_index)
		VALUES ('g0','bak','peer-a','CODE123456','10.7.0.0/24',0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "sub", "bak.db")
	if err := st.SnapshotBackup(ctx, backupPath); err != nil {
		t.Fatalf("SnapshotBackup: %v", err)
	}
	if fi, err := os.Stat(backupPath); err != nil || fi.Size() == 0 {
		t.Fatalf("backup missing or empty: %v", err)
	}

	bak, err := openStoreRaw(backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bak.Close()
	groups, _, _, _, err := bak.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot backup: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != "g0" {
		t.Fatalf("backup groups = %+v, want g0", groups)
	}
}

// =================================================================================
// 修复
// =================================================================================

// TestRepairHealthyDB 健康库跑修复应是 no-op（直接迁移成功，不重建）。
func TestRepairHealthyDB(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	st.Close()

	result, err := RepairDatabase(ctx, path)
	if err != nil {
		t.Fatalf("RepairDatabase: %v", err)
	}
	if result.Rebuilt {
		t.Fatalf("healthy db should not be rebuilt")
	}
	if result.Groups != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Fatalf("backup not created: %v", err)
	}
}

// TestRepairRebuildWithData 旧库带数据 → 修复后数据应完整迁回且可正常打开。
func TestRepairRebuildWithData(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	// 准备一个带完整数据的库。
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO groups (id, name, creator_peer_id, invite_code, cidr, subnet_index, version)
		VALUES ('g0','fixme','peer-a','CODE123456','10.7.0.0/24',0,5)`); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO members (group_id, peer_id, name, os, virtual_ip, role)
		VALUES ('g0','peer-a','owner-a','linux','10.7.0.2','owner'),
		       ('g0','peer-b','member-b','windows','10.7.0.3','member')`); err != nil {
		t.Fatalf("seed members: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO announced_addrs (peer_id, addr) VALUES
		('peer-b','/ip4/10.0.0.1/tcp/1'), ('peer-b','/ip4/10.0.0.1/udp/2/quic-v1')`); err != nil {
		t.Fatalf("seed addrs: %v", err)
	}
	st.Close()

	result, err := RepairDatabase(ctx, path)
	if err != nil {
		t.Fatalf("RepairDatabase: %v", err)
	}
	if result.Groups != 1 || result.Members != 2 || result.Addrs != 2 {
		t.Fatalf("repair moved %d/%d/%d, want 1/2/2", result.Groups, result.Members, result.Addrs)
	}
	if result.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", result.Skipped)
	}

	// 修复后的库应能正常打开且数据可用（走完整 openStore 链路）。
	fresh, err := openStore(path)
	if err != nil {
		t.Fatalf("open repaired db: %v", err)
	}
	defer fresh.Close()
	groups, members, addrs, _, err := fresh.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot repaired: %v", err)
	}
	if len(groups) != 1 || groups[0].Version != 5 {
		t.Fatalf("groups = %+v, want version=5", groups)
	}
	if len(members) != 2 || len(addrs) != 2 {
		t.Fatalf("members=%d addrs=%d, want 2/2", len(members), len(addrs))
	}
}

// TestRepairCorruptDB 主库损坏（头部被破坏）时修复应：文件级保底备份 → 重建空库可打开。
func TestRepairCorruptDB(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	// 先造一个合法库，再破坏文件头。
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	st.Close()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for corrupt: %v", err)
	}
	// SQLite 文件头前 16 字节是 "SQLite format 3\000"，写坏它。
	if _, err := f.WriteAt([]byte("GARBAGE----------"), 0); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	f.Close()

	result, err := RepairDatabase(ctx, path)
	if err != nil {
		t.Fatalf("RepairDatabase: %v", err)
	}
	if !result.Rebuilt {
		t.Fatalf("corrupt db should be rebuilt")
	}
	// 修复后应能正常打开并使用。
	fresh, err := openStore(path)
	if err != nil {
		t.Fatalf("open repaired: %v", err)
	}
	defer fresh.Close()
	if _, err := fresh.Version(ctx); err != nil {
		t.Fatalf("Version on repaired: %v", err)
	}
}

// TestRepairMissingDB 数据库文件不存在时：SQLite 打开会自动创建新库，
// 修复应等价于初始化（走迁移成功路径，Rebuilt=false 但结果健康）。
func TestRepairMissingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "not-exist.db")

	result, err := RepairDatabase(ctx, path)
	if err != nil {
		t.Fatalf("RepairDatabase: %v", err)
	}
	// SQLite 对不存在的文件 open 即创建，走"健康库直接迁移"分支属预期行为。
	if result.Rebuilt {
		t.Fatalf("missing db opens as fresh empty db, no rebuild expected")
	}
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Fatalf("backup not created: %v", err)
	}
	fresh, err := openStore(path)
	if err != nil {
		t.Fatalf("open after repair: %v", err)
	}
	defer fresh.Close()
}
