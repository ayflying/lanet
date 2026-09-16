package peersdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// =================================================================================
// 种子表（scope=group|global）
// =================================================================================
//
// 语义：种子 = 「无需加好友、直接可用」的公网可达节点。
//
// 与 peers（好友/成员）的区别：
//   - peers 是双向信任关系，可走完整虚拟网；种子只是**发现与中转入口**，
//     本机可以拨它、可以让它指路，但它不等于虚拟网成员（服务级放行 ≠ 成员）；
//   - peers 无容量上限、无淘汰；种子有上限与淘汰（长期不上线的先剔）。
//
// 与 nearby 的区别：nearby 是「被动发现到的同密钥节点」= 潜在好友候选，
// 不区分网络密钥、没有可达性验证字段。把两者混在一张表里会让
// 「谁是我能直接用的入口」与「谁可能成为我好友」互相污染，故分表。

// SeedScope 种子范围。**两张表物理隔离**：群内种子默认可用，全域种子只在
// 「全域开关」打开时参与，任何一处逻辑都不该看到另一个范围的数据。
type SeedScope string

const (
	// SeedScopeGroup 同一网络密钥（群）内的种子。容量小、淘汰慢。
	SeedScopeGroup SeedScope = "group"
	// SeedScopeGlobal 整个私有 DHT 网络的种子（跨网络密钥）。容量默认 1000、可配。
	SeedScopeGlobal SeedScope = "global"
)

// DefaultGlobalSeedLimit 全域种子表默认容量上限（用户可配）。
const DefaultGlobalSeedLimit = 1000

// DefaultGroupSeedLimit 群内种子表默认容量上限。
//
// 一个群里的公网可达设备本来就不多（通常 1~3 台），留 64 个名额足够容纳
// 地址变动带来的新旧记录并存，又不至于让脏数据长期堆积。
const DefaultGroupSeedLimit = 64

// seedTables 范围 → 表名白名单。
//
// 表名只能靠字符串拼接进 SQL（SQLite 不支持把表名当参数绑定），
// 故必须走这张白名单：任何未登记的范围一律报错，**绝不回退到某张默认表**，
// 否则一个拼错的 scope 就会把数据写进另一个范围，破坏隔离。
var seedTables = map[SeedScope]string{
	SeedScopeGroup:  "group_seeds",
	SeedScopeGlobal: "global_seeds",
}

// seedTable 返回范围对应的表名，未登记的范围返回错误。
func seedTable(scope SeedScope) (string, error) {
	t, ok := seedTables[scope]
	if !ok {
		return "", fmt.Errorf("peersdb: 未知种子范围 %q", string(scope))
	}
	return t, nil
}

// ValidSeedScope 判断范围字符串是否有效（配置面/接口入参校验用）。
func ValidSeedScope(scope SeedScope) bool {
	_, ok := seedTables[scope]
	return ok
}

// Seed 一条种子记录。
type Seed struct {
	PeerID string
	Name   string
	Addrs  []string
	// PublicReachable 是否**实测**公网可达（有公网地址且被外部拨通过）。
	// 注意：这是本机观测到的结论，不接受对端自报——「我能被你拨通」
	// 与「我能被所有人拨通」是两件事，自报会把 NAT 后的节点误判成种子。
	PublicReachable bool
	// OKCount 累计拨通成功次数；**验证门**：0 表示从未成功，不能作为种子分发出去。
	OKCount   int
	LastOK    time.Time
	FirstSeen time.Time
	LastSeen  time.Time
	Source    string // direct / exchange / self
	UpdatedAt time.Time
}

// Verified 是否通过验证门（至少拨通过一次）。
func (s Seed) Verified() bool { return s.OKCount > 0 }

// seedColumns 两表共用的列清单与扫描顺序（保持与 scanSeed 一致）。
const seedColumns = `peer_id, name, addrs, public_reachable, ok_count,
	last_ok, first_seen, last_seen, source, updated_at`

func scanSeed(s rowScanner) (*Seed, error) {
	var seed Seed
	var addrs string
	var publicReachable int
	var lastOK sql.NullTime
	var firstSeen, lastSeen sql.NullTime
	var updatedAt sql.NullTime
	if err := s.Scan(&seed.PeerID, &seed.Name, &addrs, &publicReachable, &seed.OKCount,
		&lastOK, &firstSeen, &lastSeen, &seed.Source, &updatedAt); err != nil {
		return nil, err
	}
	seed.Addrs = splitAddrs(addrs)
	seed.PublicReachable = publicReachable != 0
	seed.LastOK = lastOK.Time
	seed.FirstSeen = firstSeen.Time
	seed.LastSeen = lastSeen.Time
	seed.UpdatedAt = updatedAt.Time
	return &seed, nil
}

// UpsertSeed 写入/更新一条种子记录，返回是否发生了写入。
//
// 三条硬规则：
//  1. **好友永不入种子表**：已信任节点在成员表里，本来就能直接拨，
//     再存一份只会白占种子名额、并让「种子数」这个指标失真。命中即静默跳过。
//  2. **不碰验证字段**：ok_count / last_ok 只由 NoteSeedDialResult 维护，
//     这里不写——避免「一次握手顺带把未验证的种子洗成已验证」。
//  3. **旧数据不覆盖新数据**：仅当入参 updated_at 不早于库内记录时才更新，
//     防止乱序到达的交换数据把更新的地址/可达性冲掉。
func (d *DB) UpsertSeed(ctx context.Context, scope SeedScope, seed Seed) (bool, error) {
	table, err := seedTable(scope)
	if err != nil {
		return false, err
	}
	if seed.PeerID == "" {
		return false, fmt.Errorf("peersdb: 种子缺少 peer_id")
	}
	now := time.Now()
	updatedAt := seed.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	firstSeen := seed.FirstSeen
	if firstSeen.IsZero() {
		firstSeen = now
	}
	lastSeen := seed.LastSeen
	if lastSeen.IsZero() {
		lastSeen = now
	}
	addrs := joinAddrs(NormalizeAddrs(seed.Addrs))

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("peersdb: begin upsert seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 规则 1：是好友就不入种子表。
	var trusted int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM peers WHERE peer_id = ? AND trusted = 1`, seed.PeerID).Scan(&trusted); err != nil {
		return false, fmt.Errorf("peersdb: upsert seed 检查好友: %w", err)
	}
	if trusted > 0 {
		return false, nil
	}

	// 表名来自白名单（见 seedTables），不会是外部可控字符串。
	query := fmt.Sprintf(`
INSERT INTO %s (peer_id, name, addrs, public_reachable, first_seen, last_seen, source, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(peer_id) DO UPDATE SET
	name             = CASE WHEN excluded.name != '' THEN excluded.name ELSE %s.name END,
	addrs            = CASE WHEN excluded.addrs != '' THEN excluded.addrs ELSE %s.addrs END,
	public_reachable = %s.public_reachable OR excluded.public_reachable,
	last_seen        = CASE WHEN excluded.last_seen > %s.last_seen THEN excluded.last_seen ELSE %s.last_seen END,
	source           = CASE WHEN excluded.source != '' THEN excluded.source ELSE %s.source END,
	updated_at       = excluded.updated_at
WHERE excluded.updated_at >= %s.updated_at`, table, table, table, table, table, table, table, table)

	res, err := tx.ExecContext(ctx, query, seed.PeerID, seed.Name, addrs,
		boolInt(seed.PublicReachable), firstSeen, lastSeen, seed.Source, updatedAt)
	if err != nil {
		return false, fmt.Errorf("peersdb: upsert seed %s: %w", seed.PeerID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("peersdb: commit upsert seed: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetSeed 读取单条种子，不存在返回 (nil, nil)。
func (d *DB) GetSeed(ctx context.Context, scope SeedScope, peerID string) (*Seed, error) {
	table, err := seedTable(scope)
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE peer_id = ?`, seedColumns, table), peerID)
	seed, err := scanSeed(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peersdb: get seed %s: %w", peerID, err)
	}
	return seed, nil
}

// ListSeeds 列出某个范围的种子，按「最适合当入口」排序：
// 公网可达 → 成功次数多 → 最近拨通 → 最近见到。
//
// limit <= 0 表示不限制。已验证（Verified）过滤由调用方决定：分发种子时必须
// 只要已验证的，展示时可以全给。
func (d *DB) ListSeeds(ctx context.Context, scope SeedScope, limit int) ([]Seed, error) {
	table, err := seedTable(scope)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT %s FROM %s
ORDER BY public_reachable DESC, ok_count DESC, (last_ok IS NULL), last_ok DESC, last_seen DESC`, seedColumns, table)
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("peersdb: list seeds: %w", err)
	}
	defer rows.Close()
	var out []Seed
	for rows.Next() {
		s, err := scanSeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// CountSeeds 返回某个范围的种子条数。
func (d *DB) CountSeeds(ctx context.Context, scope SeedScope) (int, error) {
	table, err := seedTable(scope)
	if err != nil {
		return 0, err
	}
	var n int
	if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(1) FROM %s`, table)).Scan(&n); err != nil {
		return 0, fmt.Errorf("peersdb: count seeds: %w", err)
	}
	return n, nil
}

// DeleteSeed 删除一条种子（成为好友、或被判定为不可用）。
func (d *DB) DeleteSeed(ctx context.Context, scope SeedScope, peerID string) error {
	table, err := seedTable(scope)
	if err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE peer_id = ?`, table), peerID); err != nil {
		return fmt.Errorf("peersdb: delete seed %s: %w", peerID, err)
	}
	return nil
}

// NoteSeedDialResult 记录一次对种子的拨号结果：
//   - ok=true：ok_count +1、last_ok = now（该种子首次成功即通过验证门）；
//   - 失败：只刷新 last_seen，不动 ok_count —— 失败不改写历史，
//     淘汰顺序靠「ok_count 是否 >0」与 last_ok 的新旧体现。
func (d *DB) NoteSeedDialResult(ctx context.Context, scope SeedScope, peerID string, ok bool) error {
	table, err := seedTable(scope)
	if err != nil {
		return err
	}
	now := time.Now()
	var res sql.Result
	if ok {
		res, err = d.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s SET ok_count = ok_count + 1, last_ok = ?, last_seen = ?
WHERE peer_id = ?`, table), now, now, peerID)
	} else {
		res, err = d.db.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET last_seen = ? WHERE peer_id = ?`, table), now, peerID)
	}
	if err != nil {
		return fmt.Errorf("peersdb: note seed dial %s: %w", peerID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 种子表里没有它（可能刚被剔或被升级成好友），不算错误。
		return nil
	}
	return nil
}

// MarkSeedPublic 更新「实测公网可达」标记（探测结果回写）。
// 只升不降：一旦实测可达就长期有效，避免探测抖动把种子入口标记翻来覆去。
func (d *DB) MarkSeedPublic(ctx context.Context, scope SeedScope, peerID string, reachable bool) error {
	if !reachable {
		return nil
	}
	table, err := seedTable(scope)
	if err != nil {
		return err
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET public_reachable = 1 WHERE peer_id = ?`, table), peerID); err != nil {
		return fmt.Errorf("peersdb: mark seed public %s: %w", peerID, err)
	}
	return nil
}

// EvictSeeds 把种子表压到 limit 以内，返回删除条数。limit <= 0 不淘汰。
//
// 淘汰顺序（最差的先删）：
//  1. 从未拨通（ok_count = 0）—— 没过验证门，留着也是空耗拨号预算；
//  2. last_ok 最旧（NULL 视为最旧）—— 越久没成功过越可能是失效节点；
//  3. last_seen 最旧 —— 长期不上线的先剔（用户明确要求）。
//
// 不使用 public_reachable 作首要判据：群内种子可能只是「群内可达」，
// 它不是公网节点但依然是本群的合法入口，不该因此被优先剔除。
func (d *DB) EvictSeeds(ctx context.Context, scope SeedScope, limit int) (int64, error) {
	table, err := seedTable(scope)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, nil
	}
	total, err := d.CountSeeds(ctx, scope)
	if err != nil {
		return 0, err
	}
	if total <= limit {
		return 0, nil
	}
	// 子查询按「最差在前」排序并取前 overflow 条，即要删掉的那些。
	// 注意不能用 `LIMIT -1 OFFSET limit`：那是跳过最差的 limit 条去删剩下的
	// 好行，正好删反（实测把最好的两条删掉、留下了未验证的那条）。
	overflow := total - limit
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE peer_id IN (
	SELECT peer_id FROM %s
	ORDER BY (ok_count > 0) ASC, (last_ok IS NULL) DESC, last_ok ASC, last_seen ASC
	LIMIT ?
)`, table, table), overflow)
	if err != nil {
		return 0, fmt.Errorf("peersdb: evict seeds: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneTrustedSeeds 清理两张种子表里「其实已经是好友」的行，返回删除条数。
//
// 与 PruneTrustedNearby 同理：UpsertSeed 会拦新写入，但**存量**行不会自动消失
// （比如先当种子、后被审批成好友）。每次打开库做一次幂等清理。
func (d *DB) PruneTrustedSeeds(ctx context.Context) (int64, error) {
	var total int64
	for _, table := range seedTables {
		res, err := d.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s WHERE peer_id IN (
	SELECT peer_id FROM peers WHERE trusted = 1)`, table))
		if err != nil {
			return total, fmt.Errorf("peersdb: prune trusted seeds: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// PruneStaleSeeds 清理长期没有动静的种子，返回删除条数。
//
// 「长期不上线」分两档，因为两类记录的「值钱程度」完全不同：
//   - 未验证（ok_count = 0）：从没拨通过，只是别人转发来的一条地址。
//     留一小段时间给它一次机会，之后即清——否则脏数据会把名额占满，
//     让真正可用的种子因溢出被淘汰。
//   - 已验证（ok_count > 0）：曾经真的拨通过，是本机确认过的入口。
//     容忍期长得多（可能只是对方关机/出差），但也不能永久保留。
//
// maxIdle <= 0 表示该档不清理。
func (d *DB) PruneStaleSeeds(ctx context.Context, scope SeedScope, maxIdleUnverified, maxIdleVerified time.Duration) (int64, error) {
	table, err := seedTable(scope)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	var total int64
	if maxIdleUnverified > 0 {
		res, err := d.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s
WHERE ok_count = 0 AND COALESCE(last_seen, first_seen, updated_at) < ?`, table),
			now.Add(-maxIdleUnverified))
		if err != nil {
			return total, fmt.Errorf("peersdb: prune stale unverified seeds: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if maxIdleVerified > 0 {
		res, err := d.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s
WHERE ok_count > 0 AND COALESCE(last_ok, last_seen, updated_at) < ?`, table),
			now.Add(-maxIdleVerified))
		if err != nil {
			return total, fmt.Errorf("peersdb: prune stale verified seeds: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// =================================================================================
// 设置存储（app_settings）
// =================================================================================

// SettingKey 已知设置项。集中在常量里，避免各处手写字符串拼错。
const (
	// SettingGlobalSeedsEnabled 全域种子开关（"1" / "0"）。默认关。
	//
	// 打开后：本机会额外向**全局 rendezvous key** 广播自己的存在，并收集整个
	// 私有 DHT 网络的种子；代价是跨网络密钥可见性（别人能看到你的 PeerID 与
	// 地址），故默认关闭、由用户显式开启。
	SettingGlobalSeedsEnabled = "global_seeds_enabled"
	// SettingGlobalSeedsLimit 全域种子表容量上限（整数，默认 DefaultGlobalSeedLimit）。
	SettingGlobalSeedsLimit = "global_seeds_limit"
	// SettingGroupSeedsEnabled 群内种子共享开关（"1" / "0"）。默认开。
	SettingGroupSeedsEnabled = "group_seeds_enabled"
)

// GetSetting 读取设置项，不存在返回 (""、false、nil)。
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.db.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("peersdb: get setting %s: %w", key, err)
	}
	return v, true, nil
}

// SetSetting 写入设置项（不存在即插入）。
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("peersdb: 设置项缺少 key")
	}
	if _, err := d.db.ExecContext(ctx, `
INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now()); err != nil {
		return fmt.Errorf("peersdb: set setting %s: %w", key, err)
	}
	return nil
}

// ListSettings 返回全部设置项快照（展示用）。
func (d *DB) ListSettings(ctx context.Context) (map[string]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT key, value FROM app_settings`)
	if err != nil {
		return nil, fmt.Errorf("peersdb: list settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// DeleteSetting 删除设置项（恢复默认）。
func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("peersdb: delete setting %s: %w", key, err)
	}
	return nil
}

// ---- 内部助手 ----

// joinAddrs / splitAddrs：地址列表用逗号存成单列（与 peers/nearby 的历史做法一致）。
// multiaddr 本身不含逗号，故无需转义。
func joinAddrs(addrs []string) string {
	out := ""
	for i, a := range addrs {
		if i > 0 {
			out += ","
		}
		out += a
	}
	return out
}

func splitAddrs(s string) []string {
	if s == "" {
		return nil
	}
	return NormalizeAddrs(splitOnComma(s))
}

func splitOnComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
