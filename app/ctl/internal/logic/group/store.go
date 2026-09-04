package group

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// store 负责 SQLite 持久化：群组、成员、通告地址三类数据。
// 写路径先落库再更新内存索引，保证服务重启后数据完整恢复。

type store struct {
	db *sql.DB
}

type groupRow struct {
	ID            string
	Name          string
	CreatorPeerID string
	InviteCode    string
	InviteExpires *time.Time // nil 表示邀请码永久有效
	CIDR          string
	SubnetIndex   int
	Version       uint64
}

type memberRow struct {
	PeerID    string
	Name      string
	OS        string
	VirtualIP string
	Role      string // "owner" 或 "member"
}

func openStore(path string) (*store, error) {
	// WAL 提升并发读写；busy_timeout 缓解多写者锁竞争。
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc/sqlite 多连接写同一文件容易 SQLITE_BUSY，收敛为单写连接。
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	// 版本化迁移（账本表 schema_migrations 记录进度，逐版本推进）。
	if _, err := s.Migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 保留为兼容入口：旧调用点（测试等）走版本化迁移。
func (s *store) migrate() error {
	_, err := s.Migrate(context.Background())
	return err
}

func (s *store) Close() error {
	return s.db.Close()
}

// --- 群组 ---

func (s *store) insertGroup(ctx context.Context, row groupRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, creator_peer_id, invite_code, invite_expires_at, cidr, subnet_index, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.Name, row.CreatorPeerID, row.InviteCode, row.InviteExpires, row.CIDR, row.SubnetIndex, row.Version)
	if err != nil {
		return fmt.Errorf("insert group %s: %w", row.ID, err)
	}
	return nil
}

func (s *store) bumpGroupVersion(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET version = version + 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("bump group %s version: %w", id, err)
	}
	return nil
}

// updateInvite 落库新的邀请码与过期时间（expires 可为 nil 表示永久有效）。
func (s *store) updateInvite(ctx context.Context, groupID, inviteCode string, expires *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE groups SET invite_code = ?, invite_expires_at = ? WHERE id = ?`,
		inviteCode, expires, groupID)
	if err != nil {
		return fmt.Errorf("update invite of %s: %w", groupID, err)
	}
	return nil
}

func (s *store) loadGroups(ctx context.Context) ([]groupRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, creator_peer_id, invite_code, invite_expires_at, cidr, subnet_index, version FROM groups`)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	defer rows.Close()
	var items []groupRow
	for rows.Next() {
		var row groupRow
		if err := rows.Scan(&row.ID, &row.Name, &row.CreatorPeerID, &row.InviteCode, &row.InviteExpires, &row.CIDR, &row.SubnetIndex, &row.Version); err != nil {
			return nil, fmt.Errorf("scan group row: %w", err)
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

func (s *store) maxSubnetIndex(ctx context.Context) (int, error) {
	var maxIdx sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(subnet_index) FROM groups`).Scan(&maxIdx); err != nil {
		return 0, fmt.Errorf("load max subnet_index: %w", err)
	}
	if !maxIdx.Valid {
		return -1, nil
	}
	return int(maxIdx.Int64), nil
}

// --- 成员 ---

func (s *store) insertMember(ctx context.Context, groupID string, row memberRow) error {
	role := row.Role
	if role == "" {
		role = "member"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO members (group_id, peer_id, name, os, virtual_ip, role)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		groupID, row.PeerID, row.Name, row.OS, row.VirtualIP, role)
	if err != nil {
		return fmt.Errorf("insert member %s: %w", row.PeerID, err)
	}
	return nil
}

// deleteMember 移除成员；announced_addrs 由外键级联删除。
func (s *store) deleteMember(ctx context.Context, groupID, peerID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM members WHERE group_id = ? AND peer_id = ?`, groupID, peerID)
	if err != nil {
		return fmt.Errorf("delete member %s of %s: %w", peerID, groupID, err)
	}
	return nil
}

func (s *store) loadMembersByGroup(ctx context.Context, groupID string) ([]memberRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT peer_id, name, os, virtual_ip, role FROM members WHERE group_id = ? ORDER BY virtual_ip`, groupID)
	if err != nil {
		return nil, fmt.Errorf("load members of %s: %w", groupID, err)
	}
	defer rows.Close()
	var items []memberRow
	for rows.Next() {
		var row memberRow
		if err := rows.Scan(&row.PeerID, &row.Name, &row.OS, &row.VirtualIP, &row.Role); err != nil {
			return nil, fmt.Errorf("scan member row: %w", err)
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

// --- 通告地址 ---

func (s *store) replaceAnnouncedAddrs(ctx context.Context, peerID string, addrs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin addr tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM announced_addrs WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("clear addrs of %s: %w", peerID, err)
	}
	for _, addr := range addrs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO announced_addrs (peer_id, addr) VALUES (?, ?)`, peerID, addr); err != nil {
			return fmt.Errorf("insert addr of %s: %w", peerID, err)
		}
	}
	return tx.Commit()
}

func (s *store) loadAnnouncedAddrs(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer_id, addr FROM announced_addrs`)
	if err != nil {
		return nil, fmt.Errorf("load announced addrs: %w", err)
	}
	defer rows.Close()
	result := make(map[string][]string)
	for rows.Next() {
		var peerID, addr string
		if err := rows.Scan(&peerID, &addr); err != nil {
			return nil, fmt.Errorf("scan addr row: %w", err)
		}
		result[peerID] = append(result[peerID], addr)
	}
	return result, rows.Err()
}

// parseDSNPath 从 DSN 中提取纯文件路径，便于测试用临时目录。
func parseDSNPath(path string) string {
	return strings.TrimPrefix(path, "file:")
}
