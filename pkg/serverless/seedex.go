// 本文件：种子交换协议（免好友共享「已验证的公网入口」）。
//
// ==================== 要解决什么 ====================
//
// 用户需求原话：「相同网络密钥里面，有带公网的设备，不需要加好友，直接给公网
// 密钥内所有用户使用……只要连上一个设备就可以知道公网设备是哪几个。」
//
// 也就是说：群里的公网设备应该成为**全群共享的发现/中继入口**，而获取它
// 不该要求逐一双向加好友（加好友是一次人工审批，几十个成员的群不可能一个个批）。
// 这个协议就是这条通道：交换的是「谁的地址实测能拨通」，不含任何身份细节。
//
// ==================== 两条隔离的通道 ====================
//
//   - **群内**：协议 ID 由群密钥派生（/lanet/<群指纹>/seeds/1.0.0）。异群节点
//     在 multistream 阶段就协商不上，与「群内种子表 / 全域种子表物理隔离」对应。
//   - **全域**：固定 ID /lanet/seeds-global/1.0.0。只有显式打开全域开关的节点
//     才注册这个 handler，**双向判定天然成立**——对端没开就不会来拨、也拨不通。
//     这是用户明确要求的「不同网络密钥的用户进行数据打通」，代价是显式的隐私
//     让渡（异群能看到你的节点 ID 与地址），故默认关闭。
//
// 载荷里还带一个 Scope 字段并做校验：群内通道来的记录绝不允许落进全域表
// （反之亦然）。协议 ID 已隔离一次，这里是第二道闸——防的是「故意在群内通道里
// 塞 scope=global」的构造消息，那会直接破坏两表隔离。
//
// ==================== 流量天花板（用户最关心的一条）====================
//
// 用户要求「重点测试不要让流量爆炸」。这里的每一处上限都是硬编码的：
//
//   - 单条消息 ≤ 64 条记录，每条记录 ≤ 4 个地址；读取侧还有 64KB 硬闸，
//     超大消息直接丢弃（不解析、不缓存）。
//   - 对同一对端 ≥ 5 分钟才允许一次交换（出入向共用同一把闸）。
//   - 全局并发 ≤ 2。
//   - 主动交换每 10 分钟一轮，每轮最多挑 3 个对端。
//   - 拨出失败（对端没开这个协议）的对端进入 1 小时退避，不再反复试探。
//
// 最坏情况的网络开销：3 次 × (64 记录 × 4 地址 × ~40B) ≈ 30KB / 10 分钟，
// 且只在真的有 64 条已验证种子时才会达到；正常规模是几百字节。
// 全部筛选都是本地内存/数据库读，不产生任何额外 DHT 查询。
package serverless

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

// ProtocolSeedsGlobal 全域种子交换协议 ID（固定值）。
//
// 固定而非派生：它要跨网络密钥互联，派生 ID 会把异群节点挡在门外，
// 与「全域打通」的目标正好相反。参与资格不靠协议 ID 保密，而靠
// 「双方都显式打开了全域开关」——未开启者不注册 handler，拨过来直接失败。
const ProtocolSeedsGlobal libprotocol.ID = "/lanet/seeds-global/1.0.0"

// 种子范围标识。取值与 pkg/peersdb 的 SeedScope 一致（那边是 string 别名），
// 保持字面量一致以便上层直接透传，避免多一层映射出错。
const (
	SeedScopeGroup  = "group"
	SeedScopeGlobal = "global"
)

const (
	// seedsMaxRecords 单条消息携带的种子条数上限（硬性流量天花板）。
	seedsMaxRecords = 64
	// seedsMaxAddrs 单条种子携带的地址数上限。
	seedsMaxAddrs = 4
	// seedsMaxPayload 单条消息的字节上限（读取侧硬闸）。
	seedsMaxPayload = 64 * 1024
	// seedsPeerCooldown 同一对端两次交换的最小间隔（出入向共用）。
	seedsPeerCooldown = 5 * time.Minute
	// seedsMaxConcurrent 并发交换上限。
	seedsMaxConcurrent = 2
	// seedsRoundInterval 主动交换周期。
	seedsRoundInterval = 10 * time.Minute
	// seedsPeersPerRound 每轮最多挑几个对端交换。
	seedsPeersPerRound = 3
	// seedsStreamTimeout 单次交换的读写预算。
	seedsStreamTimeout = 20 * time.Second
	// seedsUnsupportedBackoff 对端不支持该协议（协商失败）后的退避时长。
	// 没有它就会出现「每轮都对着没开开关的对端白试一次」的固定浪费。
	seedsUnsupportedBackoff = time.Hour
	// seedsInitialDelay 入网后多久开始第一轮交换。
	seedsInitialDelay = 45 * time.Second
)

// SeedRecord 一条种子记录（跨节点传输用的中立结构）。
//
// 刻意不依赖 pkg/peersdb 的类型：pkg/serverless 是叶子包，不该为了一个
// 传输结构反向依赖数据库层。上层在 Config 回调里做一次字段映射即可。
type SeedRecord struct {
	PeerID string   `json:"peer_id"`
	Name   string   `json:"name,omitempty"`
	Addrs  []string `json:"addrs,omitempty"`
	// Public 发送方观测到的「公网可达」结论。**接收方不得直接采信**：
	// 「你能拨通它」不等于「我也能拨通它」。故它只作提示，接收方入库时
	// 一律按未验证（ok_count=0）处理，等自己拨通才算数。
	Public bool `json:"public,omitempty"`
	// UpdatedAt 记录版本（Unix 秒），用于「旧数据不覆盖新数据」。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// seedPayload 种子交换的线上载荷。
type seedPayload struct {
	Scope string `json:"scope"`
	// Fingerprint 群指纹（群内范围必填）：与协议 ID 的双重校验，
	// 防「在群内通道里塞异群/全域记录」。
	Fingerprint string       `json:"fp,omitempty"`
	Records     []SeedRecord `json:"records"`
}

// seedRateLimiter 出/入向共用的限流器：同对端冷却 + 全局并发上限。
type seedRateLimiter struct {
	mu     sync.Mutex
	last   map[string]time.Time // peerID -> 上次放行时刻
	active int
}

func newSeedRateLimiter() *seedRateLimiter {
	return &seedRateLimiter{last: map[string]time.Time{}}
}

// allow 判断是否可以与该对端交换（冷却已过且并发未满）。放行时占用一个并发额度。
func (r *seedRateLimiter) allow(peerID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active >= seedsMaxConcurrent {
		return false
	}
	if t, ok := r.last[peerID]; ok && now.Sub(t) < seedsPeerCooldown {
		return false
	}
	if len(r.last) > 512 { // 惰性清理：条目不多，但别让它无限长
		for k, v := range r.last {
			if now.Sub(v) > 24*time.Hour {
				delete(r.last, k)
			}
		}
	}
	r.last[peerID] = now
	r.active++
	return true
}

// release 归还并发额度（与 allow 成对调用）。
func (r *seedRateLimiter) release() {
	r.mu.Lock()
	if r.active > 0 {
		r.active--
	}
	r.mu.Unlock()
}

// backoff 主动放弃某个对端一段时间（协议协商失败等）。
func (r *seedRateLimiter) backoff(peerID string, until time.Time) {
	r.mu.Lock()
	r.last[peerID] = until
	r.mu.Unlock()
}

// initSeedRuntime 把 Config 里的种子相关初值灌进运行期字段。
//
// 构造 Discovery 时必须调一次——这些字段才是运行期的唯一事实来源，
// Config 里的同名项只作初值，之后改开关一律走 EnableSeedExchange。
// 单独抽成方法是为了能在不起 libp2p host 的前提下直接测这段桥接。
func (d *Discovery) initSeedRuntime(cfg Config) {
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	d.seedGate = newSeedRateLimiter()
	d.seedGroupOn = cfg.GroupSeedsEnabled
	d.seedGlobalOn = cfg.GlobalSeedsEnabled
	d.seedLimit = cfg.GlobalSeedsLimit
	d.seedSourceFn = cfg.SeedSource
	d.seedMergeFn = cfg.SeedMerge
	d.seedPeersFn = cfg.SeedPeers
	d.seedCandFn = cfg.SeedCandidates
}

// SeedExchangeOptions 运行期打开/调整种子交换的参数。
//
// 为什么需要运行期入口：开关值存在地址簿的设置表里（用户可在控制台改），
// 构造 Discovery 时上层不一定已经读到；而「改了开关要立即生效」是基本要求。
type SeedExchangeOptions struct {
	// GroupEnabled 群内通道开关。
	GroupEnabled bool
	// GlobalEnabled 全域通道开关。
	GlobalEnabled bool
	// Limit 全域种子表容量上限（仅用于上报/展示，落库淘汰由上层做）。
	Limit int
	// Source / Merge / Peers 见 Config 里的同名项说明。
	Source func(scope string, max int) []SeedRecord
	Merge  func(scope, from string, records []SeedRecord)
	Peers  func(scope string, max int) []string
	// Candidates 同时供中继候选使用（种子即最可信的中继候选）。
	Candidates func(ctx context.Context, number int) []peer.AddrInfo
}

// EnableSeedExchange 在运行期设置种子交换参数，并按需注册协议 handler。
//
// 可重复调用（控制台改开关后重新调用即生效）；handler 只注册一次。
// 关闭某个范围时**不注销** handler（libp2p 支持注销，但注销后重新打开需要
// 重新注册，来回切换容易漏；而「关了就不处理」由 seedScopeEnabled 判定，
// 关掉时入向请求直接 Reset，不产生任何处理开销）。
func (d *Discovery) EnableSeedExchange(opt SeedExchangeOptions) {
	d.seedMu.Lock()
	d.seedGroupOn = opt.GroupEnabled
	d.seedGlobalOn = opt.GlobalEnabled
	if opt.Limit > 0 {
		d.seedLimit = opt.Limit
	}
	if opt.Source != nil {
		d.seedSourceFn = opt.Source
	}
	if opt.Merge != nil {
		d.seedMergeFn = opt.Merge
	}
	if opt.Peers != nil {
		d.seedPeersFn = opt.Peers
	}
	if opt.Candidates != nil {
		d.seedCandFn = opt.Candidates
	}
	if d.seedGate == nil {
		// 兜底：未经 initSeedRuntime 构造时也要有限流器，否则入向请求会 panic。
		d.seedGate = newSeedRateLimiter()
	}
	needRegister := !d.seedHandlers
	d.seedHandlers = true
	d.seedMu.Unlock()

	if needRegister {
		d.registerSeedHandlers()
	}
}

// registerSeedHandlers 注册种子交换协议 handler。重复调用无副作用。
func (d *Discovery) registerSeedHandlers() {
	if d.host == nil {
		return
	}
	if d.protoSeedsGroup != "" {
		d.host.SetStreamHandler(d.protoSeedsGroup, d.handleSeedsGroup)
	}
	d.host.SetStreamHandler(ProtocolSeedsGlobal, d.handleSeedsGlobal)
}

// seedScopeEnabled 该范围是否启用（读运行期开关）。
func (d *Discovery) seedScopeEnabled(scope string) bool {
	d.seedMu.RLock()
	defer d.seedMu.RUnlock()
	switch scope {
	case SeedScopeGroup:
		return d.seedGroupOn
	case SeedScopeGlobal:
		return d.seedGlobalOn
	}
	return false
}

// seedAnyScopeEnabled 是否至少有一个范围启用（决定要不要起交换循环）。
func (d *Discovery) seedAnyScopeEnabled() bool {
	d.seedMu.RLock()
	defer d.seedMu.RUnlock()
	return d.seedGroupOn || d.seedGlobalOn
}

// seedCandidates 中继候选用的种子（运行期回调，未设置返回 nil）。
func (d *Discovery) seedCandidates(ctx context.Context, number int) []peer.AddrInfo {
	d.seedMu.RLock()
	fn := d.seedCandFn
	d.seedMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, number)
}

// seedLimiter 取限流器（加锁读，允许运行期替换；未初始化返回 nil）。
func (d *Discovery) seedLimiter() *seedRateLimiter {
	d.seedMu.RLock()
	defer d.seedMu.RUnlock()
	return d.seedGate
}

// sanitizeSeedRecords 对即将发出的记录做规范化与截断：
// 去空 ID、去自报地址中的脏项、按 ID 去重、限制条数与每条的地址数。
//
// 发送侧也做一遍（不只接收侧）：把「本机地址治理」的结论直接带到线上，
// 避免把回环/容器内网这类必然失败的地址传播出去污染别人的种子表。
func sanitizeSeedRecords(records []SeedRecord, maxRecords int) []SeedRecord {
	if maxRecords <= 0 || maxRecords > seedsMaxRecords {
		maxRecords = seedsMaxRecords
	}
	out := make([]SeedRecord, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if len(out) >= maxRecords {
			break
		}
		if r.PeerID == "" || seen[r.PeerID] {
			continue
		}
		addrs := sanitizeSeedAddrs(r.Addrs)
		if len(addrs) == 0 {
			continue
		}
		seen[r.PeerID] = true
		r.Addrs = addrs
		out = append(out, r)
	}
	return out
}

// sanitizeSeedAddrs 清洗种子地址：解析 → 剔除回环/未指定/链路本地/中继地址
// → 去重 → 按可达性排序 → 截断到 seedsMaxAddrs。
//
// 与 relayUsableAddrs 同源（都走 p2pkit.SortByReachability，后者已剔掉回环、
// 未指定与 lanet overlay），额外多剔一类 /p2p-circuit：中继地址是别人当前
// 那一次转发的路径，写进种子表会在对端过期后变成死地址。
func sanitizeSeedAddrs(addrs []string) []string {
	parsed := make([]ma.Multiaddr, 0, len(addrs))
	for _, raw := range addrs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			continue
		}
		parsed = append(parsed, a)
	}
	out := make([]string, 0, len(parsed))
	for _, a := range relayUsableAddrs(parsed) {
		if len(out) >= seedsMaxAddrs {
			break
		}
		out = append(out, a.String())
	}
	return out
}

// seedMergeResult 合并结果（仅用于日志与测试断言）。
type seedMergeResult struct {
	Accepted int
	Dropped  int
}

// selfID 本机节点 ID。
//
// host 未就绪（纯逻辑单测构造的 Discovery）时返回空串而不是 panic：
// 种子合并这类纯校验逻辑不该因为「没有真主机」而不可测。
func (d *Discovery) selfID() string {
	if d.host == nil {
		return ""
	}
	return d.host.ID().String()
}

// mergeIncomingSeeds 校验并合并对端送来的记录，返回统计。
//
// 校验顺序（任一不过就直接丢弃整批，不做部分接受——半接受会让
// 「越权数据混进来」变得难以定位）：
//  1. scope 必须与当前通道一致；
//  2. 记录数与单条地址数必须在硬上限内；
//  3. 每条记录的 PeerID 必须能解析为合法 libp2p 节点 ID。
func (d *Discovery) mergeIncomingSeeds(scope, from string, payload seedPayload) seedMergeResult {
	var res seedMergeResult
	if payload.Scope != scope {
		// 跨范围注入：整批丢弃。这是两表隔离的第二道闸。
		res.Dropped = len(payload.Records)
		d.logf("种子交换拒绝：对端 %s 在 %s 通道里声明 scope=%q（疑似跨范围注入）",
			shortID(from), scope, payload.Scope)
		return res
	}
	if len(payload.Records) > seedsMaxRecords {
		res.Dropped = len(payload.Records)
		d.logf("种子交换拒绝：对端 %s 一次送来 %d 条（上限 %d）",
			shortID(from), len(payload.Records), seedsMaxRecords)
		return res
	}
	valid := make([]SeedRecord, 0, len(payload.Records))
	self := d.selfID()
	for _, r := range payload.Records {
		if r.PeerID == "" || r.PeerID == self {
			res.Dropped++
			continue
		}
		if _, err := peer.Decode(r.PeerID); err != nil {
			res.Dropped++
			continue
		}
		if len(r.Addrs) > seedsMaxAddrs {
			r.Addrs = r.Addrs[:seedsMaxAddrs]
		}
		valid = append(valid, r)
	}
	if len(valid) == 0 {
		return res
	}
	d.seedMu.RLock()
	mergeFn := d.seedMergeFn
	d.seedMu.RUnlock()
	if mergeFn != nil {
		mergeFn(scope, from, valid)
	}
	res.Accepted = len(valid)
	return res
}

// handleSeedsGroup / handleSeedsGlobal 入向处理器。
func (d *Discovery) handleSeedsGroup(s network.Stream)  { d.handleSeeds(s, SeedScopeGroup) }
func (d *Discovery) handleSeedsGlobal(s network.Stream) { d.handleSeeds(s, SeedScopeGlobal) }

// handleSeeds 处理一次入向种子交换：读对端记录 → 校验合并 → 回自己的记录。
//
// 与信息握手不同，这里**不校验审批状态**：种子只是「谁能当入口」，
// 不含名称/主机名/版本等身份细节，而且「免好友共享」正是本需求的目的。
// 隔离靠协议 ID（群内派生 / 全域开关）与 scope 校验，不靠审批门。
func (d *Discovery) handleSeeds(s network.Stream, scope string) {
	defer func() { _ = s.Close() }()
	if !d.seedScopeEnabled(scope) {
		_ = s.Reset()
		return
	}
	// 限流器缺失属于构造期遗漏（编程错误）。网络输入不该把进程打崩，
	// 这里直接关流 —— 与「超限」同样是 fail-closed。
	gate := d.seedLimiter()
	if gate == nil {
		_ = s.Reset()
		return
	}
	remote := s.Conn().RemotePeer().String()
	now := time.Now()
	if !gate.allow(remote, now) {
		// 超出冷却或并发：直接关闭，不给对端任何回执（避免成为放大面）。
		_ = s.Reset()
		return
	}
	defer gate.release()

	_ = s.SetDeadline(now.Add(seedsStreamTimeout))
	raw, err := io.ReadAll(io.LimitReader(s, seedsMaxPayload))
	if err != nil {
		return
	}
	var payload seedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	res := d.mergeIncomingSeeds(scope, remote, payload)
	if !d.cfg.Quiet {
		d.logf("种子交换（%s）入向：来自 %s，接受 %d 条，丢弃 %d 条",
			scope, shortID(remote), res.Accepted, res.Dropped)
	}

	// 回自己的记录（本机已验证的），同样受限。
	records := d.localSeedRecords(scope, seedsMaxRecords)
	if len(records) == 0 {
		return
	}
	out, err := json.Marshal(seedPayload{
		Scope: scope, Fingerprint: d.seedFingerprint(scope), Records: records,
	})
	if err != nil || len(out) > seedsMaxPayload {
		return
	}
	_, _ = s.Write(out)
}

// localSeedRecords 取本机愿意分享的记录（走 Config.SeedSource，纯本地读）。
func (d *Discovery) localSeedRecords(scope string, max int) []SeedRecord {
	d.seedMu.RLock()
	fn := d.seedSourceFn
	d.seedMu.RUnlock()
	if fn == nil {
		return nil
	}
	return sanitizeSeedRecords(fn(scope, max), max)
}

// seedFingerprint 群内范围的指纹（全域范围为空——它本来就跨群）。
func (d *Discovery) seedFingerprint(scope string) string {
	if scope == SeedScopeGroup {
		return GroupFingerprint(d.groupKey)
	}
	return ""
}

// seedPeerTargets 选择本轮交换对象：
//   - 优先当前已连上的成员（交换最可能成功）；
//   - 其次成员表里未超期的其他节点；
//   - 全域范围下再并入 Config.SeedPeers 提供的节点（通常来自 DHT 路由表），
//     这是「跨网络密钥打通」的入口。
//
// 排序稳定（ID 升序兜底），保证限流冷却能均匀覆盖到所有对端而不是反复挑同几个。
func (d *Discovery) seedPeerTargets(scope string, max int) []peer.ID {
	self := d.host.ID()
	now := time.Now()
	cutoff := now.Add(-d.memberTTL)

	d.mu.RLock()
	members := make([]Member, 0, len(d.members))
	for _, m := range d.members {
		members = append(members, *m)
	}
	d.mu.RUnlock()

	type cand struct {
		id        peer.ID
		connected bool
		lastSeen  time.Time
	}
	var cands []cand
	seen := map[peer.ID]bool{}
	appendID := func(id peer.ID, lastSeen time.Time) {
		if id == self || seen[id] {
			return
		}
		seen[id] = true
		cands = append(cands, cand{
			id: id, lastSeen: lastSeen,
			connected: d.host.Network().Connectedness(id) == network.Connected,
		})
	}
	for _, m := range members {
		if m.LastSeen.Before(cutoff) {
			continue
		}
		id, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		appendID(id, m.LastSeen)
	}
	d.seedMu.RLock()
	extraPeers := d.seedPeersFn
	d.seedMu.RUnlock()
	if scope == SeedScopeGlobal && extraPeers != nil {
		for _, raw := range extraPeers(scope, seedsMaxRecords) {
			if id, err := peer.Decode(raw); err == nil {
				appendID(id, time.Time{})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].connected != cands[j].connected {
			return cands[i].connected
		}
		if !cands[i].lastSeen.Equal(cands[j].lastSeen) {
			return cands[i].lastSeen.After(cands[j].lastSeen)
		}
		return cands[i].id.String() < cands[j].id.String()
	})

	out := make([]peer.ID, 0, max)
	for _, c := range cands {
		if len(out) >= max {
			break
		}
		out = append(out, c.id)
	}
	return out
}

// exchangeSeeds 主动向一个对端发起一次交换（出向）。
func (d *Discovery) exchangeSeeds(ctx context.Context, id peer.ID, scope string) error {
	records := d.localSeedRecords(scope, seedsMaxRecords)
	proto := d.seedsProtoFor(scope)
	if proto == "" {
		return errors.New("种子交换未启用")
	}
	// 主动去换的前提是「我们有东西给别人」或「我们想要别人的」：两者都没有
	// 就没必要产生任何流量（纯 NAT 且刚装好的群就是这种情况）。
	targets := d.seedPeerTargets(scope, 1)
	if len(targets) == 0 && len(records) == 0 {
		return errors.New("无交换目标")
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, seedsStreamTimeout)
	defer cancel()
	s, err := d.host.NewStream(exchangeCtx, id, proto)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	_ = s.SetDeadline(time.Now().Add(seedsStreamTimeout))

	payload, err := json.Marshal(seedPayload{
		Scope: scope, Fingerprint: d.seedFingerprint(scope), Records: records,
	})
	if err != nil {
		return err
	}
	if _, err := s.Write(payload); err != nil {
		return err
	}
	// 本机记录为空时对端不会回包：不等它，直接成功返回。
	if len(records) == 0 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(s, seedsMaxPayload))
	if err != nil || len(raw) == 0 {
		return err
	}
	var reply seedPayload
	if err := json.Unmarshal(raw, &reply); err != nil {
		return err
	}
	res := d.mergeIncomingSeeds(scope, id.String(), reply)
	if !d.cfg.Quiet {
		d.logf("种子交换（%s）出向：对端 %s，接受 %d 条，丢弃 %d 条",
			scope, shortID(id.String()), res.Accepted, res.Dropped)
	}
	return nil
}

// seedsProtoFor 该范围的协议 ID。
func (d *Discovery) seedsProtoFor(scope string) libprotocol.ID {
	switch scope {
	case SeedScopeGroup:
		if d.cfg.LegacyProtocols {
			return ""
		}
		return d.protoSeedsGroup
	case SeedScopeGlobal:
		return ProtocolSeedsGlobal
	}
	return ""
}

// seedRound 执行一轮种子交换（两种范围各一遍）。
func (d *Discovery) seedRound(ctx context.Context) {
	gate := d.seedLimiter()
	if gate == nil {
		return
	}
	for _, scope := range []string{SeedScopeGroup, SeedScopeGlobal} {
		if !d.seedScopeEnabled(scope) {
			continue
		}
		now := time.Now()
		var wg sync.WaitGroup
		started := 0
		for _, id := range d.seedPeerTargets(scope, seedsPeersPerRound) {
			if started >= seedsMaxConcurrent {
				break
			}
			if !gate.allow(id.String(), now) {
				continue
			}
			started++
			wg.Add(1)
			go func(id peer.ID) {
				defer wg.Done()
				defer gate.release()
				if err := d.exchangeSeeds(ctx, id, scope); err != nil {
					// 对端多半没开这个协议或根本不可达：退避一小时，别再每轮白试。
					gate.backoff(id.String(), time.Now().Add(seedsUnsupportedBackoff))
					if !d.cfg.Quiet {
						d.logf("种子交换（%s）出向失败，对端 %s 退避 %s: %v",
							scope, shortID(id.String()), seedsUnsupportedBackoff, err)
					}
				}
			}(id)
		}
		wg.Wait()
	}
}

// startSeedExchange 启动种子交换循环（异步、随 ctx 结束、只启一次）。
//
// 开销复盘（对应用户「不要让流量爆炸」的要求）：每 10 分钟一轮，每范围最多
// 3 个对端、每个对端 ≤64 条记录 × ≤4 地址。没有成员、没有种子时整轮零流量
// （seedPeerTargets 返回空，循环直接结束）。
func (d *Discovery) startSeedExchange(ctx context.Context) {
	if !d.seedAnyScopeEnabled() {
		return
	}
	d.seedOnce.Do(func() {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(seedsInitialDelay):
			}
			for {
				d.seedRound(ctx)
				select {
				case <-ctx.Done():
					return
				case <-time.After(seedsRoundInterval):
				}
			}
		}()
	})
}
