// Package peersdb 节点侧的本地持久化库（lanet.db，SQLite）：保存已审批的
// 可信节点、连接历史与已知地址，使节点重启后无需重新申请/审批，并让
// 「按节点 ID 连接」优先命中本地地址簿（零 DHT 查询秒连）。
//
// 设计要点：
//   - 独立文件 lanet.db（与 ctl 侧控制面库、state.json 均解耦）；
//   - 迁移以有序切片内嵌代码（与二进制同生命周期），schema_migrations
//     账本表记录版本；禁止修改已发布迁移，只允许追加；
//   - 并发：单连接（SetMaxOpenConns(1)）+ WAL，避免 SQLITE_BUSY；
//   - 隐私：只存 peer_id 与地址，不存任何业务数据。
package peersdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无需 CGO）
)

// DB 本地持久化库句柄。
type DB struct {
	db   *sql.DB
	path string
}

// Peer 一个已知节点（可信或曾连接）。
type Peer struct {
	PeerID    string    // libp2p 节点 ID（主键）
	Name      string    // 节点名（info 协议交换得到，可能为空）
	Addrs     []string  // 已知 multiaddr（不含 /p2p/<id> 也可，读写时归一化）
	Trusted   bool      // 是否已审批通过（true = 永久信任，免再审）
	Approved  bool      // 是否收到过对方（或被对方）审批
	Manually  bool      // 是否由用户手动添加（区别于自动发现）
	FirstSeen time.Time // 首次见到
	LastSeen  time.Time // 最近一次见到/通讯成功
	LastIP    string    // 最近一次使用的虚拟 IP（展示用）
	Notes     string    // 备注（用户可写）
}

// Open 打开（不存在则创建）lanet.db 并执行迁移。
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("peersdb: open %s: %w", path, err)
	}
	// 单连接：本地库并发量极低，串行化可彻底避免 SQLITE_BUSY。
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("peersdb: ping %s: %w", path, err)
	}
	d := &DB{db: sqldb, path: path}
	if err := d.migrate(ctx); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	// 启动自愈：旧版本授予信任时不清理 nearby，导致「先被被动发现写进附近、
	// 紧接着审批通过」的节点永久滞留在附近列表（且 nearby 不区分网络密钥，
	// 换 network_key 后陈旧行照旧显示）。这里每次打开库做一次幂等清理，
	// 无需追加迁移即可修好存量脏数据。尽力而为——清理失败不阻塞启动。
	_, _ = d.PruneTrustedNearby(ctx)
	return d, nil
}

// Path 返回库文件路径（日志/诊断用）。
func (d *DB) Path() string { return d.path }

// Close 关闭库。
func (d *DB) Close() error {
	if d.db == nil {
		return nil
	}
	return d.db.Close()
}

// =================================================================================
// schema 迁移
// =================================================================================

type migration struct {
	version int
	name    string
	up      string
}

// migrations 按版本升序；禁止修改已发布迁移，只允许追加新版本。
var migrations = []migration{
	{
		version: 1,
		name:    "baseline: peers / peer_addrs",
		up: `
CREATE TABLE IF NOT EXISTS peers (
	peer_id     TEXT PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	trusted     INTEGER NOT NULL DEFAULT 0,  -- 1 = 已审批，永久信任
	approved    INTEGER NOT NULL DEFAULT 0,  -- 1 = 审批流程已完成
	manual      INTEGER NOT NULL DEFAULT 0,  -- 1 = 用户手动添加
	first_seen  DATETIME,
	last_seen   DATETIME,
	last_ip     TEXT NOT NULL DEFAULT '',
	notes       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_peers_trusted ON peers(trusted);
CREATE INDEX IF NOT EXISTS idx_peers_last_seen ON peers(last_seen);

-- 已知地址：一个节点可有多条（多网卡/多协议），按 last_ok 排序优先尝试。
CREATE TABLE IF NOT EXISTS peer_addrs (
	peer_id  TEXT NOT NULL REFERENCES peers(peer_id) ON DELETE CASCADE,
	addr     TEXT NOT NULL,
	last_ok  DATETIME,                        -- 最近一次拨通成功时刻（NULL = 从未成功）
	ok_count INTEGER NOT NULL DEFAULT 0,      -- 累计成功次数（用于排序，越多越优先）
	PRIMARY KEY (peer_id, addr)
);
CREATE INDEX IF NOT EXISTS idx_peer_addrs_peer ON peer_addrs(peer_id);`,
	},
	{
		version: 2,
		name:    "pending_requests: 待审批连接请求",
		up: `
CREATE TABLE IF NOT EXISTS pending_requests (
	peer_id    TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	addrs      TEXT NOT NULL DEFAULT '',   -- 逗号分隔的 multiaddr 快照
	requested_at DATETIME NOT NULL,
	reason     TEXT NOT NULL DEFAULT ''    -- 请求来源说明（如 "DHT 发现" / "主动拨号"）
);
CREATE INDEX IF NOT EXISTS idx_pending_requested ON pending_requests(requested_at);`,
	},
	{
		version: 3,
		name:    "nearby + unfriended: 附近节点观察表与删除好友墓碑",
		up: `
-- 附近节点：发现到（DHT/mDNS）但尚未成为好友的同网络密钥节点。
-- 只存公开可发现的信息（节点 ID + 地址），名称等身份细节因审批隔离拿不到。
-- 被删除过的好友同样会回到这里（可重新申请连接）。
CREATE TABLE IF NOT EXISTS nearby (
	peer_id   TEXT PRIMARY KEY,
	name      TEXT NOT NULL DEFAULT '',
	addrs     TEXT NOT NULL DEFAULT '',   -- 逗号分隔（重新申请连接时免查 DHT）
	source    TEXT NOT NULL DEFAULT '',   -- dht-private / dht / mdns
	first_seen DATETIME,
	last_seen  DATETIME
);
CREATE INDEX IF NOT EXISTS idx_nearby_last_seen ON nearby(last_seen);

-- 删除好友墓碑：本机主动删除过的节点。对方若仍把本机当好友，它下次来
-- 握手时本机据此明确回「已被删除好友」（而不是沉默的未审批），对方收到
-- 后自动把本机也从它的列表删除——删除因此是双向的。对方重新申请连接时
-- 清除墓碑、重新进入待审批，允许恢复。
CREATE TABLE IF NOT EXISTS unfriended (
	peer_id  TEXT PRIMARY KEY,
	name     TEXT NOT NULL DEFAULT '',
	at       DATETIME NOT NULL
);`,
	},
	{
		version: 4,
		name:    "seeds(群内/全域两张隔离表) + app_settings 设置存储",
		up: `
-- 种子表：记录「不需要加好友、直接可用」的公网可达节点。
--
-- 为什么单独建表而不复用 nearby：nearby 是「被动发现到的同密钥节点」，
-- 语义是「可选好友候选」，且不区分网络密钥、没有可达性验证字段；种子表要的是
-- 「实测能直拨的中转/发现入口」，必须有 ok_count / last_ok 作验证门、必须有
-- 容量上限与淘汰顺序，两者语义与生命周期都不同，混用会互相污染。
--
-- 两张表**物理隔离**而非用 scope 列：请求方是不同开关下的两套逻辑
-- （群内种子默认可用；全域种子只在「全域开关」打开时才参与），
-- 分表能让「跨范围串数据」在 SQL 层面就不可能发生。
-- 两表结构完全一致，见 seeds.go 的 seedColumns。
CREATE TABLE IF NOT EXISTS group_seeds (
	peer_id          TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	addrs            TEXT NOT NULL DEFAULT '',   -- 逗号分隔的 multiaddr 快照
	public_reachable INTEGER NOT NULL DEFAULT 0, -- 1 = 实测公网可达（免好友共享的前提）
	ok_count         INTEGER NOT NULL DEFAULT 0, -- 累计拨通成功次数（验证门）
	last_ok          DATETIME,                   -- 最近一次拨通成功（NULL = 从未成功）
	first_seen       DATETIME NOT NULL,
	last_seen        DATETIME NOT NULL,
	source           TEXT NOT NULL DEFAULT '',   -- direct / exchange / gossip
	updated_at       DATETIME NOT NULL           -- 记录版本，防旧数据覆盖新数据
);
CREATE INDEX IF NOT EXISTS idx_group_seeds_entry ON group_seeds(public_reachable, last_ok);
CREATE INDEX IF NOT EXISTS idx_group_seeds_seen ON group_seeds(last_seen);

-- 全域种子：只在「全域开关」打开的本机上收集与使用，需要存整个私有 DHT 网络
-- 的入口节点，故容量上限（默认 1000，可配）比群内大得多，淘汰也更激进
-- （长期不上线的优先剔）。
CREATE TABLE IF NOT EXISTS global_seeds (
	peer_id          TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	addrs            TEXT NOT NULL DEFAULT '',
	public_reachable INTEGER NOT NULL DEFAULT 0,
	ok_count         INTEGER NOT NULL DEFAULT 0,
	last_ok          DATETIME,
	first_seen       DATETIME NOT NULL,
	last_seen        DATETIME NOT NULL,
	source           TEXT NOT NULL DEFAULT '',
	updated_at       DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_global_seeds_entry ON global_seeds(public_reachable, last_ok);
CREATE INDEX IF NOT EXISTS idx_global_seeds_seen ON global_seeds(last_seen);

-- 键值设置表：存节点级运行配置（如全域开关与容量上限）。
-- 放数据库而不是只读环境变量，是为了让控制台能在线改、且重启后仍生效；
-- 环境变量仍作后备（见 SDK 侧配置面）。
CREATE TABLE IF NOT EXISTS app_settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL DEFAULT '',
	updated_at DATETIME NOT NULL
);`,
	},
}

const schemaMigrationsMeta = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, schemaMigrationsMeta); err != nil {
		return fmt.Errorf("peersdb: ensure schema_migrations: %w", err)
	}
	var cur sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&cur); err != nil {
		return fmt.Errorf("peersdb: read schema_migrations: %w", err)
	}
	current := 0
	if cur.Valid {
		current = int(cur.Int64)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if _, err := d.db.ExecContext(ctx, m.up); err != nil {
			return fmt.Errorf("peersdb: apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := d.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name); err != nil {
			return fmt.Errorf("peersdb: record migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaVersion 返回当前迁移版本（诊断用）。
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var cur sql.NullInt64
	err := d.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&cur)
	if err != nil {
		return 0, err
	}
	if !cur.Valid {
		return 0, nil
	}
	return int(cur.Int64), nil
}
