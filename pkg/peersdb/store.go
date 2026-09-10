package peersdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UpsertPeer 记录一次「见到某个节点」：不存在则插入，存在则更新
// name/last_seen/last_ip，并在提供地址时合并进地址簿。
// 不会改变 trusted/approved/manual 状态（审批状态只由审批流程变更）。
func (d *DB) UpsertPeer(ctx context.Context, p Peer, addrs []string) error {
	now := time.Now()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("peersdb: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO peers (peer_id, name, first_seen, last_seen, last_ip, manual)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET
	name      = CASE WHEN excluded.name != '' THEN excluded.name ELSE peers.name END,
	last_seen = excluded.last_seen,
	last_ip   = CASE WHEN excluded.last_ip != '' THEN excluded.last_ip ELSE peers.last_ip END,
	manual    = peers.manual OR excluded.manual`,
		p.PeerID, p.Name, now, now, p.LastIP, boolInt(p.Manually)); err != nil {
		return fmt.Errorf("peersdb: upsert peer %s: %w", p.PeerID, err)
	}
	for _, a := range NormalizeAddrs(addrs) {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO peer_addrs (peer_id, addr) VALUES (?, ?)`, p.PeerID, a); err != nil {
			return fmt.Errorf("peersdb: upsert addr: %w", err)
		}
	}
	return tx.Commit()
}

// SetTrusted 设置审批状态。trusted=true 表示永久信任（免再审）。
// 无论批准还是撤销，都会清掉该节点的待审批记录：
//   - 批准后不该再出现在「待审批」列表里；
//   - 撤销后也不该残留旧的请求。
func (d *DB) SetTrusted(ctx context.Context, peerID string, trusted bool) error {
	res, err := d.db.ExecContext(ctx, `
UPDATE peers SET trusted = ?, approved = ? WHERE peer_id = ?`,
		boolInt(trusted), boolInt(true), peerID)
	if err != nil {
		return fmt.Errorf("peersdb: set trusted %s: %w", peerID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 库中还没有该节点：先建一条再置信任（手动添加的可信节点）。
		if _, err := d.db.ExecContext(ctx, `
INSERT INTO peers (peer_id, trusted, approved, manual, first_seen, last_seen)
VALUES (?, ?, 1, 1, ?, ?)`, peerID, boolInt(trusted), time.Now(), time.Now()); err != nil {
			return fmt.Errorf("peersdb: insert trusted %s: %w", peerID, err)
		}
	}
	// 审批动作完成后，该节点不再属于「待审批」。
	if _, err := d.db.ExecContext(ctx, `DELETE FROM pending_requests WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("peersdb: clear pending %s: %w", peerID, err)
	}
	return nil
}

// IsTrusted 查询某节点是否已审批信任。
func (d *DB) IsTrusted(ctx context.Context, peerID string) (bool, error) {
	var t int
	err := d.db.QueryRowContext(ctx, `SELECT trusted FROM peers WHERE peer_id = ?`, peerID).Scan(&t)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("peersdb: is trusted %s: %w", peerID, err)
	}
	return t != 0, nil
}

// GetPeer 读取单个节点（含地址），不存在返回 (nil, nil)。
func (d *DB) GetPeer(ctx context.Context, peerID string) (*Peer, error) {
	row := d.db.QueryRowContext(ctx, `
SELECT peer_id, name, trusted, approved, manual, first_seen, last_seen, last_ip, notes
FROM peers WHERE peer_id = ?`, peerID)
	p, err := scanPeer(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peersdb: get peer %s: %w", peerID, err)
	}
	addrs, err := d.Addrs(ctx, peerID)
	if err != nil {
		return nil, err
	}
	p.Addrs = addrs
	return p, nil
}

// KnownAddrs 返回某节点的已知地址，按「拨通成功次数多、最近成功优先」排序，
// 供「按 ID 连接」时优先尝试（命中即零 DHT 查询秒连）。
func (d *DB) KnownAddrs(ctx context.Context, peerID string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT addr FROM peer_addrs WHERE peer_id = ?
ORDER BY ok_count DESC, (last_ok IS NULL), last_ok DESC`, peerID)
	if err != nil {
		return nil, fmt.Errorf("peersdb: known addrs %s: %w", peerID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Addrs 返回某节点全部已知地址（不过滤排序，展示用）。
func (d *DB) Addrs(ctx context.Context, peerID string) ([]string, error) {
	return d.KnownAddrs(ctx, peerID)
}

// NoteDialResult 记录一次拨号结果：成功时累加 ok_count 并更新 last_ok，
// 让下次连接优先走这条可用地址。
func (d *DB) NoteDialResult(ctx context.Context, peerID, addr string, ok bool) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if !ok {
		// 失败不改动计数，仅确保记录存在（保留历史便于排查）。
		_, err := d.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO peer_addrs (peer_id, addr) VALUES (?, ?)`, peerID, addr)
		return err
	}
	_, err := d.db.ExecContext(ctx, `
INSERT INTO peer_addrs (peer_id, addr, last_ok, ok_count) VALUES (?, ?, ?, 1)
ON CONFLICT(peer_id, addr) DO UPDATE SET
	last_ok  = excluded.last_ok,
	ok_count = peer_addrs.ok_count + 1`, peerID, addr, time.Now())
	if err != nil {
		return fmt.Errorf("peersdb: note dial %s: %w", peerID, err)
	}
	return nil
}

// ListPeers 列出节点（trustedOnly=true 时只列已审批的）。
func (d *DB) ListPeers(ctx context.Context, trustedOnly bool) ([]Peer, error) {
	q := `SELECT peer_id, name, trusted, approved, manual, first_seen, last_seen, last_ip, notes FROM peers`
	if trustedOnly {
		q += ` WHERE trusted = 1`
	}
	q += ` ORDER BY last_seen DESC`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("peersdb: list peers: %w", err)
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 填充地址（数量通常很小，逐条查可读性更好）。
	for i := range out {
		addrs, err := d.KnownAddrs(ctx, out[i].PeerID)
		if err != nil {
			return nil, err
		}
		out[i].Addrs = addrs
	}
	return out, nil
}

// TrustedIDs 返回全部已审批节点的 ID 集合（供运行时快速判断）。
func (d *DB) TrustedIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT peer_id FROM peers WHERE trusted = 1`)
	if err != nil {
		return nil, fmt.Errorf("peersdb: trusted ids: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DeletePeer 删除节点及其地址（用于「忘记该设备」）。
func (d *DB) DeletePeer(ctx context.Context, peerID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM peers WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("peersdb: delete peer %s: %w", peerID, err)
	}
	return nil
}

// =================================================================================
// 待审批请求
// =================================================================================

// PendingRequest 一条待审批的连接请求。
type PendingRequest struct {
	PeerID      string
	Name        string
	Addrs       []string
	RequestedAt time.Time
	Reason      string
}

// AddPending 记录一条待审批请求（重复请求刷新时间与地址）。
func (d *DB) AddPending(ctx context.Context, r PendingRequest) error {
	at := r.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := d.db.ExecContext(ctx, `
INSERT INTO pending_requests (peer_id, name, addrs, requested_at, reason)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET
	name         = CASE WHEN excluded.name != '' THEN excluded.name ELSE pending_requests.name END,
	addrs        = excluded.addrs,
	requested_at = excluded.requested_at,
	reason       = excluded.reason`,
		r.PeerID, r.Name, strings.Join(NormalizeAddrs(r.Addrs), ","), at, r.Reason)
	if err != nil {
		return fmt.Errorf("peersdb: add pending %s: %w", r.PeerID, err)
	}
	return nil
}

// ListPending 列出待审批请求（按请求时间升序，先来先审）。
func (d *DB) ListPending(ctx context.Context) ([]PendingRequest, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT peer_id, name, addrs, requested_at, reason FROM pending_requests ORDER BY requested_at`)
	if err != nil {
		return nil, fmt.Errorf("peersdb: list pending: %w", err)
	}
	defer rows.Close()
	var out []PendingRequest
	for rows.Next() {
		var r PendingRequest
		var addrs string
		if err := rows.Scan(&r.PeerID, &r.Name, &addrs, &r.RequestedAt, &r.Reason); err != nil {
			return nil, err
		}
		r.Addrs = NormalizeAddrs(strings.Split(addrs, ","))
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeletePending 移除一条待审批请求（同意或拒绝后调用）。
func (d *DB) DeletePending(ctx context.Context, peerID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM pending_requests WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("peersdb: delete pending %s: %w", peerID, err)
	}
	return nil
}

// IsPending 查询是否为待审批状态。
func (d *DB) IsPending(ctx context.Context, peerID string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_requests WHERE peer_id = ?`, peerID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// =================================================================================
// 辅助
// =================================================================================

// rowScanner 兼容 *sql.Row 与 *sql.Rows。
type rowScanner interface{ Scan(dest ...any) error }

func scanPeer(s rowScanner) (*Peer, error) {
	var p Peer
	var trusted, approved, manual int
	var firstSeen, lastSeen sql.NullTime
	if err := s.Scan(&p.PeerID, &p.Name, &trusted, &approved, &manual,
		&firstSeen, &lastSeen, &p.LastIP, &p.Notes); err != nil {
		return nil, err
	}
	p.Trusted, p.Approved, p.Manually = trusted != 0, approved != 0, manual != 0
	p.FirstSeen, p.LastSeen = firstSeen.Time, lastSeen.Time
	return &p, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NormalizeAddrs 清洗地址列表：去空白、去空项、去重、保持顺序。
// 不含 "/p2p/" 的裸地址也可（拨号时由调用方补全节点 ID）。
func NormalizeAddrs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}
