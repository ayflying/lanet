package peersdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
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
	added := 0
	for _, a := range FilterDialableAddrs(NormalizeAddrs(addrs)) {
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO peer_addrs (peer_id, addr) VALUES (?, ?)`, p.PeerID, a)
		if err != nil {
			return fmt.Errorf("peersdb: upsert addr: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	// 有新增才修剪：地址簿是只增不减的，运行期也必须收敛，否则单个节点
	// 会一直涨（实测某节点攒到 110 条，每次连接都要逐条试）。
	if added > 0 {
		if err := prunePeerAddrsTx(ctx, tx, p.PeerID, prunePeerAddrsKeep); err != nil {
			return err
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
	// 授予信任后该节点也不再属于「附近」：nearby 的语义是「同网络密钥内
	// 可发现、但尚未成为好友」。历史缺陷——这里只清了 pending 没清 nearby，
	// 于是「先被被动发现写进 nearby、紧接着审批通过」的节点会永久滞留在
	// 附近列表（用户看到一堆其实已经是好友的节点）。
	// 注意只在 trusted=true 时清：撤销信任/删除好友后，该节点**应该**重新
	// 出现在附近（下一轮发现会重新写回），这是「申请连接」的恢复入口。
	if trusted {
		if _, err := d.db.ExecContext(ctx, `DELETE FROM nearby WHERE peer_id = ?`, peerID); err != nil {
			return fmt.Errorf("peersdb: clear nearby %s: %w", peerID, err)
		}
		for scope := range seedTables {
			if err := d.DeleteSeed(ctx, scope, peerID); err != nil {
				return err
			}
		}
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
		// 失败不改动计数，仅确保记录存在（保留历史便于排查）。但结构性不可达
		// 的地址连记录都不该留：它们永远拨不通，留着只会让每次「按 ID 连接」
		// 多试一条。此处是与 UpsertPeer 对齐的写入侧闸门。
		if !isStructurallyDialable(addr) {
			return nil
		}
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

// HasPeers 判断地址簿是否至少存在一个节点。仅执行 EXISTS，避免为流控加载全部地址和记录。
func (d *DB) HasPeers(ctx context.Context, trustedOnly bool) (bool, error) {
	q := `SELECT EXISTS(SELECT 1 FROM peers`
	if trustedOnly {
		q += ` WHERE trusted = 1`
	}
	q += `)`
	var exists bool
	if err := d.db.QueryRowContext(ctx, q).Scan(&exists); err != nil {
		return false, fmt.Errorf("peersdb: has peers: %w", err)
	}
	return exists, nil
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
// 附近节点（发现到但尚未成为好友的同网络节点）
// =================================================================================

// Nearby 一条「附近」观察：同网络密钥内可见、但本机未信任的节点。
type Nearby struct {
	PeerID string
	Name   string
	Addrs  []string
	Source string
	// Notes 本机手写的备注（来自 peers 表，**不是** nearby 表的列）。
	// 仅用于展示：用户给「曾经是好友、后来删掉」的节点写过备注时，它在附近
	// 列表里仍应显示出来，否则用户认不出这是谁而不敢重新申请。
	Notes     string
	FirstSeen time.Time
	LastSeen  time.Time
}

// UpsertNearby 记录一次「发现某个非好友节点」：不存在则插入，存在则刷新
// 地址/来源/最近时间。已信任节点直接忽略（好友不进附近）。被删除过的好友
// 同样会回到附近（墓碑只改握手拒绝语义，不屏蔽可见性）——用户正是靠这条
// 记录点「申请连接」重新加回对方。
// 表容量封顶 200 条（按最近发现时间保留），防止公共大网下无限膨胀。
func (d *DB) UpsertNearby(ctx context.Context, n Nearby) error {
	if n.PeerID == "" {
		return nil
	}
	trusted, err := d.IsTrusted(ctx, n.PeerID)
	if err != nil {
		return err
	}
	if trusted {
		// 已信任节点不该出现在附近：顺手清掉可能残留的历史观察。这同时
		// 覆盖「发现时读到未信任 → 审批并发完成 → 本次 INSERT 才落地」的
		// 竞态窗口（若只 return 而不删，这条记录会永久滞留）。
		_, _ = d.db.ExecContext(ctx, `DELETE FROM nearby WHERE peer_id = ?`, n.PeerID)
		return nil
	}
	now := time.Now()
	if n.Name == "" {
		// 被动发现拿不到对端身份（审批隔离），但本机可能早就认识它——曾经是
		// 好友、或手动添加过。把本地已知名字一起写进观察表，展示时不必每次
		// 回查；对端改名后，下一次「对方主动申请」会以新名字覆盖。
		var known sql.NullString
		if err := d.db.QueryRowContext(ctx,
			`SELECT name FROM peers WHERE peer_id = ?`, n.PeerID).Scan(&known); err == nil && known.Valid {
			n.Name = known.String
		}
	}
	res, err := d.db.ExecContext(ctx, `
INSERT INTO nearby (peer_id, name, addrs, source, first_seen, last_seen)
SELECT ?, ?, ?, ?, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM peers WHERE peer_id = ? AND trusted = 1)
ON CONFLICT(peer_id) DO UPDATE SET
	name      = CASE WHEN excluded.name != '' THEN excluded.name ELSE nearby.name END,
	addrs     = CASE WHEN excluded.addrs != '' THEN excluded.addrs ELSE nearby.addrs END,
	source    = CASE WHEN excluded.source != '' THEN excluded.source ELSE nearby.source END,
	last_seen = excluded.last_seen
WHERE (excluded.name != '' AND excluded.name != nearby.name)
   OR (excluded.addrs != '' AND excluded.addrs != nearby.addrs)
   OR (excluded.source != '' AND excluded.source != nearby.source)
   OR nearby.last_seen <= ?`,
		n.PeerID, n.Name, strings.Join(FilterDialableAddrs(NormalizeAddrs(n.Addrs)), ","), n.Source, now, now, n.PeerID, now.Add(-2*time.Minute))
	if err != nil {
		return fmt.Errorf("peersdb: upsert nearby %s: %w", n.PeerID, err)
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return nil
	}
	// 封顶修剪：只保留最近出现的 200 条。
	if _, err = d.db.ExecContext(ctx, `
DELETE FROM nearby WHERE peer_id NOT IN (
	SELECT peer_id FROM nearby ORDER BY last_seen DESC LIMIT 200)`); err != nil {
		return fmt.Errorf("peersdb: prune nearby: %w", err)
	}
	return nil
}

// ListNearby 列出附近节点（按最近发现时间倒序）。
func (d *DB) ListNearby(ctx context.Context) ([]Nearby, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT peer_id, name, addrs, source, first_seen, last_seen
FROM nearby ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("peersdb: list nearby: %w", err)
	}
	defer rows.Close()
	var out []Nearby
	for rows.Next() {
		var n Nearby
		var addrs string
		var firstSeen, lastSeen sql.NullTime
		if err := rows.Scan(&n.PeerID, &n.Name, &addrs, &n.Source, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		n.Addrs = NormalizeAddrs(strings.Split(addrs, ","))
		n.FirstSeen, n.LastSeen = firstSeen.Time, lastSeen.Time
		out = append(out, n)
	}
	return out, rows.Err()
}

// RemoveNearby 从附近列表移除一条观察（成为好友后清理）。
func (d *DB) RemoveNearby(ctx context.Context, peerID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM nearby WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("peersdb: remove nearby %s: %w", peerID, err)
	}
	return nil
}

// PruneTrustedNearby 清理 nearby 表里「其实已经是好友」的残留行，返回删除条数。
//
// 用于修复存量脏数据：旧版本 SetTrusted 只清 pending_requests、不清 nearby，
// 于是「被动发现先写进附近 → 同一秒审批通过」的节点会永久留在附近列表；
// 又因为 nearby 表不含网络密钥字段（ListNearby 全表返回），用户切换
// network_key 之后这些陈旧行依然会显示，看起来像「换了密钥还能看到对方」。
// 幂等，可在每次打开库时调用。
func (d *DB) PruneTrustedNearby(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
DELETE FROM nearby WHERE peer_id IN (
	SELECT peer_id FROM peers WHERE trusted = 1)`)
	if err != nil {
		return 0, fmt.Errorf("peersdb: prune trusted nearby: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneSelf 清理地址簿里「指向本机自己」的残留行，返回删除条数。
//
// 为什么会出现指向自己的行：nearby / pending_requests 是**持久化观察表**，
// 与「当前身份」没有任何绑定关系。当 node.key 发生变化时（历史版本在
// Windows 服务模式下把身份文件路径解析到了 CWD，于是 SCM 在 System32 里
// 建了一个全新身份，PeerID 与虚拟 IP 双双漂移），身份 A 运行期间会把身份 B
// 写进同一张表；身份切回 B 之后，那几行就变成了「自己」。控制台据此把它当成
// 陌生节点展示，用户点「申请连接」又会被自我保护拦下，报一句看不懂的
// 「这就是本机节点 ID，无需连接」。
//
// 与 PruneTrustedNearby 同理：每次打开库做一次幂等清理，无需追加迁移即可
// 修好存量脏数据。peer_addrs 由 peers 的外键 ON DELETE CASCADE 级联清理；
// unfriended（墓碑）指向自己同样有害——本机会拒绝自己的握手。
func (d *DB) PruneSelf(ctx context.Context, selfPeerID string) (int64, error) {
	if selfPeerID == "" {
		return 0, nil
	}
	var total int64
	for _, q := range []string{
		`DELETE FROM nearby WHERE peer_id = ?`,
		`DELETE FROM pending_requests WHERE peer_id = ?`,
		`DELETE FROM unfriended WHERE peer_id = ?`,
		`DELETE FROM peers WHERE peer_id = ?`,
		// 种子表同样会因身份漂移残留「自己」的行：控制台会把它显示成一个
		// 可用的公网入口，本机却永远拨不通（其实是自己）。
		`DELETE FROM group_seeds WHERE peer_id = ?`,
		`DELETE FROM global_seeds WHERE peer_id = ?`,
	} {
		res, err := d.db.ExecContext(ctx, q, selfPeerID)
		if err != nil {
			return total, fmt.Errorf("peersdb: prune self: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// =================================================================================
// 删除好友墓碑（unfriended）
// =================================================================================

// AddUnfriended 记录「本机已主动删除该节点」的墓碑。附近列表记录**保留**：
// 被删除的好友仍应出现在「附近」里，用户点「申请连接」即可重新加回
// （那会清除墓碑）——如果顺手删了，删除过的节点就再也刷不出来了。
func (d *DB) AddUnfriended(ctx context.Context, peerID, name string) error {
	if peerID == "" {
		return nil
	}
	if name == "" {
		// 尽量保留已知名称，墓碑信息更友好。
		if p, err := d.GetPeer(ctx, peerID); err == nil && p != nil {
			name = p.Name
		}
	}
	if _, err := d.db.ExecContext(ctx, `
INSERT INTO unfriended (peer_id, name, at) VALUES (?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET
	name = CASE WHEN excluded.name != '' THEN excluded.name ELSE unfriended.name END,
	at   = excluded.at`, peerID, name, time.Now()); err != nil {
		return fmt.Errorf("peersdb: add unfriended %s: %w", peerID, err)
	}
	return nil
}

// IsUnfriended 查询本机是否主动删除过该节点。
func (d *DB) IsUnfriended(ctx context.Context, peerID string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM unfriended WHERE peer_id = ?`, peerID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("peersdb: is unfriended %s: %w", peerID, err)
	}
	return n > 0, nil
}

// ClearUnfriended 清除墓碑（重新加好友 / 收到对方的 unfriend 通知时）。
func (d *DB) ClearUnfriended(ctx context.Context, peerID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM unfriended WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("peersdb: clear unfriended %s: %w", peerID, err)
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

// =================================================================================
// 地址清洗：结构过滤（与 NormalizeAddrs 分层，语义不同）
// =================================================================================

// lanetOverlayPrefixes 是 lanet 自身的隧道地址段。与 p2pkit 侧保持一致，
// 只包含项目规划地址，避免误过滤其他 ULA 网络。
var lanetOverlayPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.7.0.0/16"),
	netip.MustParsePrefix("fd00:6c61:6e65::/48"),
}

func isLanetOverlayIP(ip netip.Addr) bool {
	for _, prefix := range lanetOverlayPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// prunePeerAddrsKeep 每个节点在地址簿里保留的地址条数上限（见 PrunePeerAddrs）。
const prunePeerAddrsKeep = 16

// FilterDialableAddrs 剔除「结构上不可能拨通」的地址，保留其余顺序不变。
//
// 被判掉的四类（都只依赖地址形态，不需要任何对端信息）：
//   - 回环（127.0.0.0/8、::1）：只有对端自己可达；
//   - 链路本地（169.254/16、fe80::/10）：只在同一物理链路上有意义，跨网
//     必然失败；
//   - 未指定（0.0.0.0、::）：那是监听通配地址，不是可拨地址；
//   - lanet overlay（10.7.0.0/16）：本网自己的隧道段，拨它成环。
//
// 为什么必须清理而不是仅排序：拨号是「逐个尝试、共享一份时间预算」的。
// 真机实测某好友的地址簿累积 52 条、ok_count 全为 0，其中绝大部分是
// 127.0.0.1 / 169.254.x / WSL / ZeroTier 网段——每次「按 ID 连接」都要在
// 这些注定失败的地址上耗掉预算，真正能通的那条反而没机会试，表现为
// 「列表里看得到、连不上、过一会儿又自己通了」。
//
// 保底：若过滤后一条不剩（例如某节点只在链路本地可见），则退回原列表首条——
// 同链路的 mDNS 场景仍应保留连接能力，不能因为清洗过狠而彻底断掉。
func FilterDialableAddrs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if !isStructurallyDialable(a) {
			continue
		}
		out = append(out, a)
	}
	if len(out) == 0 && len(in) > 0 {
		return []string{in[0]}
	}
	return out
}

// isStructurallyDialable 判断单个地址文本是否值得尝试拨号（无 IP 段者一律保留，
// 交给拨号侧解析，如 /dns4/…）。
func isStructurallyDialable(addr string) bool {
	// circuit 是中继路径，临时且随中继预约变化，会被对端每轮重新通告——
	// 持久化它只会让地址簿越攒越多（实测 404 条里有 130 条是 circuit 变体）。
	if strings.Contains(addr, "/p2p-circuit") {
		return false
	}
	ip, ok := addrIP(addr)
	if !ok {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	return !isLanetOverlayIP(ip)
}

// addrIP 从 multiaddr 文本里取出 IP，兼容带 zone 的 IPv6（fe80::1%12）与
// IPv4-mapped IPv6（::ffff:1.2.3.4 → 归一为 IPv4）。
//
// 走字符串切分而不引 multiaddr 依赖：地址簿里存的形态固定是
// `/ip4/<v>/…` 或 `/ip6/<v>/…`，这里只需要判断可达性，不需要完整解析。
func addrIP(addr string) (netip.Addr, bool) {
	parts := strings.Split(addr, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "ip4" && parts[i] != "ip6" {
			continue
		}
		v := parts[i+1]
		if j := strings.IndexByte(v, '%'); j >= 0 {
			v = v[:j] // 剥 zone：ParseAddr 不接受 "fe80::1%12"
		}
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return netip.Addr{}, false
		}
		return ip.Unmap(), true
	}
	return netip.Addr{}, false
}

// PrunePeerAddrs 修剪地址簿：每个节点最多保留 keep 条地址，其余删除，返回删除条数。
//
// 保留顺序 = 拨号顺序（成功过的优先、最近成功优先，其次后入库的靠前），
// 与 KnownAddrs 的排序口径一致，故被删掉的一定是排在最后、最不可能被用到的。
//
// 地址簿是只增不减的：每轮发现都会把对端新枚举出来的地址并进来。长期下来
// 单个节点几十条地址、绝大多数从未拨通，每次连接都要逐条试（见
// FilterDialableAddrs 的注释）。幂等，可在每次打开库时调用。
//
// keep <= 0 时取默认值 prunePeerAddrsKeep。
func (d *DB) PrunePeerAddrs(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = prunePeerAddrsKeep
	}
	res, err := d.db.ExecContext(ctx, `
DELETE FROM peer_addrs WHERE rowid IN (
	SELECT rowid FROM (
		SELECT rowid, ROW_NUMBER() OVER (
			PARTITION BY peer_id
			ORDER BY ok_count DESC, (last_ok IS NULL), last_ok DESC, rowid DESC) AS rn
		FROM peer_addrs
	) WHERE rn > ?
)`, keep)
	if err != nil {
		return 0, fmt.Errorf("peersdb: prune peer addrs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// prunePeerAddrsTx 事务内修剪单个节点的地址条数（保留排序口径同 PrunePeerAddrs）。
func prunePeerAddrsTx(ctx context.Context, tx *sql.Tx, peerID string, keep int) error {
	if keep <= 0 {
		keep = prunePeerAddrsKeep
	}
	_, err := tx.ExecContext(ctx, `
DELETE FROM peer_addrs WHERE rowid IN (
	SELECT rowid FROM (
		SELECT rowid, ROW_NUMBER() OVER (
			PARTITION BY peer_id
			ORDER BY ok_count DESC, (last_ok IS NULL), last_ok DESC, rowid DESC) AS rn
		FROM peer_addrs WHERE peer_id = ?
	) WHERE rn > ?
)`, peerID, keep)
	if err != nil {
		return fmt.Errorf("peersdb: prune peer %s addrs: %w", peerID, err)
	}
	return nil
}

// PruneUnreachableAddrs 删除地址簿里所有「结构上不可达」的地址（回环 /
// 链路本地 / 未指定 / overlay / circuit），返回删除条数。幂等，可每次开库调用。
//
// 与 PrunePeerAddrs 的分工：后者按「数量」收敛，本函数按「判据」收敛。
// 写入侧过滤只管新增，历史积累的脏地址必须靠这一遍清掉——真机实测某节点
// 地址簿 404 条里有 169 条是回环/链路本地、130 条是 circuit，全是老版本
// 对端通告进来的。
func (d *DB) PruneUnreachableAddrs(ctx context.Context) (int64, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT rowid, addr FROM peer_addrs`)
	if err != nil {
		return 0, fmt.Errorf("peersdb: scan addrs: %w", err)
	}
	var stale []any
	for rows.Next() {
		var (
			rowid int64
			addr  string
		)
		if err := rows.Scan(&rowid, &addr); err != nil {
			rows.Close()
			return 0, fmt.Errorf("peersdb: scan addrs: %w", err)
		}
		if !isStructurallyDialable(addr) {
			stale = append(stale, rowid)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("peersdb: scan addrs: %w", err)
	}
	rows.Close()
	if len(stale) == 0 {
		return 0, nil
	}
	var removed int64
	const chunkSize = 400 // 低于 SQLite 默认变量上限（999），留足余量
	for start := 0; start < len(stale); start += chunkSize {
		end := start + chunkSize
		if end > len(stale) {
			end = len(stale)
		}
		chunk := stale[start:end]
		placeholders := strings.Repeat("?,", len(chunk)-1) + "?"
		res, err := d.db.ExecContext(ctx,
			`DELETE FROM peer_addrs WHERE rowid IN (`+placeholders+`)`, chunk...)
		if err != nil {
			return removed, fmt.Errorf("peersdb: prune unreachable addrs: %w", err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return removed, nil
}

// =================================================================================
// 名称与备注（展示用身份）
// =================================================================================

// SetName 更新某节点的自报名称。对端改名后，本机下次见到时跟随更新。
//
// 与 UpsertPeer 的区别：只改 name，**不碰 last_seen**。调用方是成员表对账，
// 此刻对端可能早已离线，把 last_seen 刷成当前时间会让地址簿排序误以为它
// 刚刚活跃过。行不存在时插入（被动发现到的成员可能还没落过库）。
func (d *DB) SetName(ctx context.Context, peerID, name string) error {
	name = strings.TrimSpace(name)
	if peerID == "" || name == "" {
		return nil
	}
	now := time.Now()
	if _, err := d.db.ExecContext(ctx, `
INSERT INTO peers (peer_id, name, first_seen, last_seen) VALUES (?, ?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET name = excluded.name`,
		peerID, name, now, now); err != nil {
		return fmt.Errorf("peersdb: set name %s: %w", peerID, err)
	}
	return nil
}

// SetNotes 写入某节点的备注（本机用户手写的别名，如「家里的 NAS」）。
//
// 与 name 的语义差别是刻意的：name 来自对端自报，会被对端改名覆盖；notes
// 是本机写的，**永不被对端数据覆盖**。两者并存，展示时备注优先、真名次之，
// 这样「对方把名字改成一串乱码」也不会让列表失去可读性。
//
// 目标可能还没有 peers 行（附近节点、待审批节点都可能有备注），不存在时建一行；
// 不改变 trusted / approved 状态——备注只是标注，不是授权。
func (d *DB) SetNotes(ctx context.Context, peerID, notes string) error {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return fmt.Errorf("peersdb: set notes: 节点 ID 为空")
	}
	now := time.Now()
	if _, err := d.db.ExecContext(ctx, `
INSERT INTO peers (peer_id, notes, first_seen, last_seen) VALUES (?, ?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET notes = excluded.notes`,
		peerID, strings.TrimSpace(notes), now, now); err != nil {
		return fmt.Errorf("peersdb: set notes %s: %w", peerID, err)
	}
	return nil
}

// NameInfo 一个节点的展示用身份：对端自报名 + 本机手写备注。
type NameInfo struct {
	Name  string
	Notes string
}

// NameIndex 一次性返回「节点 ID → 名称/备注」全量索引，供控制台在渲染成员 /
// 附近 / 待审批列表时做展示兜底。
//
// 为什么需要兜底：
//   - 成员表（运行时）只有在线/近期节点才带名字，重启或对端长期离线后名字
//     就没了，列表退化成「12D3KooW…」——用户下次根本认不出谁是谁；
//   - 附近节点是**被动发现**的，本就拿不到身份信息（审批隔离），唯一真实的
//     名字来源是「对方主动申请连接」时带来的那个名字（存在 pending_requests）。
//
// 故合并两个来源：peers 表为主，pending_requests 里非空的名字作次级补充
// （仅当 peers 表无该行时采用）。表都很小（几十到几百行），一次全量读比在
// 渲染路径上逐条查询更划算。
func (d *DB) NameIndex(ctx context.Context) (map[string]NameInfo, error) {
	out := map[string]NameInfo{}
	rows, err := d.db.QueryContext(ctx, `SELECT peer_id, name, notes FROM peers`)
	if err != nil {
		return nil, fmt.Errorf("peersdb: name index: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var info NameInfo
		if err := rows.Scan(&id, &info.Name, &info.Notes); err != nil {
			return nil, err
		}
		out[id] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 次级来源：待审批请求里的名字（申请方自报）。查询失败不影响主结果。
	prows, perr := d.db.QueryContext(ctx,
		`SELECT peer_id, name FROM pending_requests WHERE name != ''`)
	if perr != nil {
		return out, nil
	}
	defer prows.Close()
	for prows.Next() {
		var id, name string
		if err := prows.Scan(&id, &name); err != nil {
			return out, nil
		}
		if cur, ok := out[id]; ok && cur.Name != "" {
			continue // peers 表已有名字，优先
		}
		out[id] = NameInfo{Name: name, Notes: out[id].Notes}
	}
	return out, nil
}
