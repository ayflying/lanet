package lanet

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// =================================================================================
// 种子交换接线（0.5.52）
//
// 把「协议层」与「存储层」接起来：serverless 只管收发与限流（不知道 SQLite 存在），
// peersdb 只管落库（不知道 libp2p 存在），本文件是两者之间唯一的桥。
//
// 两件事必须同时成立，否则「群内公网设备免好友共享」就只是纸面功能：
//  1. 有种子可用 —— 入向合并落库、出向只分享**已验证**的；
//  2. 种子能自己变成「已验证」而**不额外产生探测流量** —— 见 startSeedVerifier。
//
// 流量口径（呼应「不要让流量爆炸」）：本文件不发起任何周期性拨号。种子的验证
// 完全靠**本地核对已建立的连接**（零额外包），种子表之间靠交换协议按 10 分钟
// 一轮、每轮至多 3 个对端的既有节奏流动。
// =================================================================================

const (
	// seedWiringReadyWait 等「入网就绪」（c.disc 就位）的上限。
	seedWiringReadyWait = 2 * time.Minute
	// seedWiringInitialDelay 就绪后再等一会儿，避开启动期的 DHT 广播与中继预约。
	seedWiringInitialDelay = 12 * time.Second
	// seedVerifyInterval 本地核对种子可达性的周期。纯本地查询，不产生网络流量。
	seedVerifyInterval = 2 * time.Minute
	// seedVerifyBatch 单轮每张表最多核对多少条（种子表可达 1000 条，不必全查）。
	seedVerifyBatch = 64
	// seedHarvestBatch 单轮最多新收多少条种子候选（有界，避免一次性写爆）。
	seedHarvestBatch = 16
	// seedStaleSweepRounds 每 N 轮做一次陈旧清理（N × seedVerifyInterval）。
	seedStaleSweepRounds = 15 // ≈30 分钟
	// seedIdleUnverified 未验证种子的容忍期：从没拨通过，给一周机会。
	seedIdleUnverified = 7 * 24 * time.Hour
	// seedIdleVerified 已验证种子的容忍期：确认过可用，容忍一个月。
	seedIdleVerified = 30 * 24 * time.Hour
	// seedLimitCeiling 全域种子表容量硬上限（防止设置里写进荒谬的大数）。
	seedLimitCeiling = 20000
)

// lifeCtx 节点生命周期 context；未就绪时退化为 Background，避免回调里拿到 nil。
func (c *Client) lifeCtx() context.Context {
	if c.rootCtx != nil {
		return c.rootCtx
	}
	return context.Background()
}

// SeedSettings 种子交换设置（落 app_settings，控制台可改）。
//
// 群内默认**开**：同一网络密钥内的公网设备本就是本群的合法入口，
// 「免好友共享」正是本需求的目的。全域默认**关**：它会让本机在私有 DHT 的
// 全局 rendezvous key 上暴露自己（跨网络密钥可见 PeerID 与地址），必须由
// 用户显式开启。
type SeedSettings struct {
	// GroupEnabled 群内种子共享开关。
	GroupEnabled bool `json:"group_enabled"`
	// GlobalEnabled 全域种子开关（跨网络密钥打通）。
	GlobalEnabled bool `json:"global_enabled"`
	// GlobalLimit 全域种子表容量上限（溢出即淘汰最差）。
	GlobalLimit int `json:"global_limit"`
}

// defaultSeedSettings 默认值：群内开、全域关、上限 1000。
func defaultSeedSettings() SeedSettings {
	return SeedSettings{
		GroupEnabled:  true,
		GlobalEnabled: false,
		GlobalLimit:   peersdb.DefaultGlobalSeedLimit,
	}
}

// normalize 把越界/缺失的值收敛到合法区间。
func (s SeedSettings) normalize() SeedSettings {
	if s.GlobalLimit <= 0 {
		s.GlobalLimit = peersdb.DefaultGlobalSeedLimit
	}
	if s.GlobalLimit > seedLimitCeiling {
		s.GlobalLimit = seedLimitCeiling
	}
	return s
}

// enabledFor serverless 范围是否启用。
func (s SeedSettings) enabledFor(scope string) bool {
	switch scope {
	case serverless.SeedScopeGroup:
		return s.GroupEnabled
	case serverless.SeedScopeGlobal:
		return s.GlobalEnabled
	}
	return false
}

// limitFor 该范围的容量上限（群内固定，全域可配）。
func (s SeedSettings) limitFor(scope string) int {
	if scope == serverless.SeedScopeGlobal {
		return s.normalize().GlobalLimit
	}
	return peersdb.DefaultGroupSeedLimit
}

// readSeedSettings 读取设置，缺项回落默认值。
func readSeedSettings(ctx context.Context, db *peersdb.DB) (SeedSettings, error) {
	s := defaultSeedSettings()
	if db == nil {
		return s, nil
	}
	if v, ok, err := db.GetSetting(ctx, peersdb.SettingGroupSeedsEnabled); err != nil {
		return s, err
	} else if ok {
		s.GroupEnabled = parseBoolSetting(v, s.GroupEnabled)
	}
	if v, ok, err := db.GetSetting(ctx, peersdb.SettingGlobalSeedsEnabled); err != nil {
		return s, err
	} else if ok {
		s.GlobalEnabled = parseBoolSetting(v, s.GlobalEnabled)
	}
	if v, ok, err := db.GetSetting(ctx, peersdb.SettingGlobalSeedsLimit); err != nil {
		return s, err
	} else if ok {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			s.GlobalLimit = n
		}
	}
	return s.normalize(), nil
}

// parseBoolSetting 宽松解析布尔设置（"1"/"true"/"on"/"yes" 为真）。
func parseBoolSetting(v string, fallback bool) bool {
	switch v {
	case "":
		return fallback
	case "1", "true", "TRUE", "True", "on", "ON", "yes", "YES":
		return true
	case "0", "false", "FALSE", "False", "off", "OFF", "no", "NO":
		return false
	}
	return fallback
}

// boolSetting 布尔设置落库形态。
func boolSetting(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// SeedSettings 读取当前种子交换设置。
func (c *Client) SeedSettings(ctx context.Context) (SeedSettings, error) {
	return readSeedSettings(ctx, c.peers)
}

// SetSeedSettings 保存设置并**立即生效**（重新调用 EnableSeedExchange）。
//
// 立即生效是硬要求：用户把开关一改，下一轮交换就该按新设置走，
// 而不是等进程重启。因此这里不走「等巡检下一轮」那条慢路径。
func (c *Client) SetSeedSettings(ctx context.Context, s SeedSettings) (SeedSettings, error) {
	if c.peers == nil {
		return s, errors.New("lanet: 未启用地址簿（DBPath 为 \"-\"），设置无法保存")
	}
	s = s.normalize()
	if err := c.peers.SetSetting(ctx, peersdb.SettingGroupSeedsEnabled, boolSetting(s.GroupEnabled)); err != nil {
		return s, err
	}
	if err := c.peers.SetSetting(ctx, peersdb.SettingGlobalSeedsEnabled, boolSetting(s.GlobalEnabled)); err != nil {
		return s, err
	}
	if err := c.peers.SetSetting(ctx, peersdb.SettingGlobalSeedsLimit, strconv.Itoa(s.GlobalLimit)); err != nil {
		return s, err
	}
	c.applySeedExchange(ctx, c.peers)
	return s, nil
}

// seedScopeOf serverless 范围 → peersdb 范围（两边的字符串取值刻意一致，
// 但类型不同，这里显式转换并校验，避免拼错时静默写进另一张表）。
func seedScopeOf(scope string) (peersdb.SeedScope, bool) {
	s := peersdb.SeedScope(scope)
	if !peersdb.ValidSeedScope(s) {
		return "", false
	}
	return s, true
}

// applySeedExchange 按当前设置装配回调并交给 Discovery。
func (c *Client) applySeedExchange(ctx context.Context, db *peersdb.DB) {
	if c.disc == nil || db == nil {
		return
	}
	s, err := readSeedSettings(ctx, db)
	if err != nil {
		c.logf("种子交换：读取设置失败，沿用上一次设置: %v", err)
		return
	}
	c.disc.EnableSeedExchange(serverless.SeedExchangeOptions{
		GroupEnabled:  s.GroupEnabled,
		GlobalEnabled: s.GlobalEnabled,
		Limit:         s.GlobalLimit,
		Source:        c.seedSourceFn(db, s),
		Merge:         c.seedMergeFn(db, s),
		Peers:         c.seedPeersFn(s),
		Candidates:    c.seedCandidateFn(db, s),
	})
	switch {
	case s.GroupEnabled && s.GlobalEnabled:
		c.logf("种子交换已启用：群内 + 全域（全域上限 %d）", s.GlobalLimit)
	case s.GroupEnabled:
		c.logf("种子交换已启用：仅群内")
	case s.GlobalEnabled:
		c.logf("种子交换已启用：仅全域（全域上限 %d）", s.GlobalLimit)
	default:
		c.logf("种子交换已关闭：群内与全域都不参与（不产生任何交换流量）")
	}
}

// startSeedWiring 入网就绪后装配种子交换（异步、一次性）。
//
// db 显式传入而不读 c.peers：openPeersDB 返回后调用方才赋值 c.peers，
// 用入参可避开这段时序依赖。
func (c *Client) startSeedWiring(ctx context.Context, db *peersdb.DB) {
	if db == nil {
		return
	}
	go func() {
		if !c.waitReady(ctx, seedWiringReadyWait) {
			return // 启动失败或被取消
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(seedWiringInitialDelay):
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c.applySeedExchange(wctx, db)
		cancel()
	}()
}

// =================================================================================
// 四个回调
// =================================================================================

// seedSourceFn 本机愿意分享的记录：**只分享已验证的**。
//
// 这是验证门的第一道：ok_count = 0 的条目（别人转发来、本机从没拨通）绝不外发，
// 否则脏地址会像谣言一样在网络里循环，越传越广、越传越旧。
func (c *Client) seedSourceFn(db *peersdb.DB, s SeedSettings) func(string, int) []serverless.SeedRecord {
	return func(scope string, max int) []serverless.SeedRecord {
		pScope, ok := seedScopeOf(scope)
		if !ok || !s.enabledFor(scope) || max <= 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(c.lifeCtx(), 3*time.Second)
		defer cancel()
		seeds, err := db.ListSeeds(ctx, pScope, 0)
		if err != nil {
			return nil
		}
		out := make([]serverless.SeedRecord, 0, max)
		for _, sd := range seeds {
			if len(out) >= max {
				break
			}
			if sd.PeerID == "" || sd.PeerID == c.peerID || !sd.Verified() || len(sd.Addrs) == 0 {
				continue
			}
			out = append(out, serverless.SeedRecord{
				PeerID: sd.PeerID, Name: sd.Name, Addrs: sd.Addrs,
				Public: sd.PublicReachable, UpdatedAt: sd.UpdatedAt.Unix(),
			})
		}
		return out
	}
}

// seedMergeFn 入向合并：一律按**未验证**入库，再执行容量闸。
//
// 对端自报的 Public 结论不采信（见 serverless.SeedRecord 注释），
// 也没有ok_count 可传 —— UpsertSeed 本身就不碰验证字段，两道保险。
func (c *Client) seedMergeFn(db *peersdb.DB, s SeedSettings) func(string, string, []serverless.SeedRecord) {
	return func(scope, from string, records []serverless.SeedRecord) {
		pScope, ok := seedScopeOf(scope)
		if !ok || !s.enabledFor(scope) || len(records) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(c.lifeCtx(), 10*time.Second)
		defer cancel()

		added := 0
		for _, r := range records {
			if r.PeerID == "" || r.PeerID == c.peerID {
				continue
			}
			wrote, err := db.UpsertSeed(ctx, pScope, peersdb.Seed{
				PeerID: r.PeerID,
				Name:   r.Name,
				Addrs:  r.Addrs,
				Source: "exchange",
			})
			if err != nil {
				continue
			}
			if wrote {
				added++
			}
		}

		// 容量闸：溢出即淘汰「最差」的（未验证优先、其次最久没动的）。
		if n, err := db.EvictSeeds(ctx, pScope, s.limitFor(scope)); err == nil && n > 0 {
			c.logf("种子表（%s）已满，淘汰 %d 条最差记录", scope, n)
		}
		if added > 0 {
			c.logf("种子交换（%s）入向：合并 %d 条新种子（来自 %s）", scope, added, shortSeedID(from))
		}
	}
}

// seedPeersFn 全域范围的额外交换对象：私有 DHT 路由表里的节点。
//
// 这就是「跨网络密钥打通」的入口 —— 路由表里有全网 lanet 节点，
// 从中挑几个碰运气问种子，问到就存下来，问不到也不亏（限流保证不刷屏）。
func (c *Client) seedPeersFn(s SeedSettings) func(string, int) []string {
	return func(scope string, max int) []string {
		if scope != serverless.SeedScopeGlobal || !s.GlobalEnabled || max <= 0 || c.disc == nil {
			return nil
		}
		all := c.disc.DHTRoutingPeers()
		out := make([]string, 0, max)
		for _, id := range all {
			if len(out) >= max {
				break
			}
			if id == "" || id == c.peerID {
				continue
			}
			out = append(out, id)
		}
		return out
	}
}

// seedCandidateFn 把已验证种子作为中继候选（稳定的公网入口最可能预约成功）。
//
// 只为「已启用的范围」供数：全域关着时不该因为全域的种子去导流。
func (c *Client) seedCandidateFn(db *peersdb.DB, s SeedSettings) func(context.Context, int) []peer.AddrInfo {
	return func(ctx context.Context, number int) []peer.AddrInfo {
		if number <= 0 {
			return nil
		}
		scopes := make([]peersdb.SeedScope, 0, 2)
		if s.GroupEnabled {
			scopes = append(scopes, peersdb.SeedScopeGroup)
		}
		if s.GlobalEnabled {
			scopes = append(scopes, peersdb.SeedScopeGlobal)
		}
		var out []peer.AddrInfo
		seen := make(map[peer.ID]bool, number)
		for _, sc := range scopes {
			if len(out) >= number {
				break
			}
			seeds, err := db.ListSeeds(ctx, sc, number)
			if err != nil {
				continue
			}
			for _, sd := range seeds {
				if len(out) >= number {
					break
				}
				if sd.PeerID == c.peerID || !sd.Verified() || len(sd.Addrs) == 0 {
					continue
				}
				id, err := peer.Decode(sd.PeerID)
				if err != nil || seen[id] {
					continue
				}
				addrs := parseAddrs(sd.Addrs)
				if len(addrs) == 0 {
					continue
				}
				seen[id] = true
				out = append(out, peer.AddrInfo{ID: id, Addrs: addrs})
			}
		}
		return out
	}
}

// =================================================================================
// 种子自维护：本地核对 + 陈旧清理
// =================================================================================

// startSeedVerifier 周期性核对种子的可达性与新鲜度（**纯本地，零额外流量**）。
//
// 为什么不做主动探测：用户明确要求「自测流量不爆炸」。主动拨号 N 个种子意味着
// N 条常驻连接 + 心跳，1000 条种子的规模下完全不可接受。而 libp2p 已经维护着
// 连接状态：只要某条种子正好被本机连上（作为 bootstrap、被 DHT 发现、被中继
// 预约选中……），Network().Connectedness() 立刻就能查到 —— 那就把它标记为
// 已验证，并把**真正连上的地址**回写，供后续分发。零成本，且结论最真实。
func (c *Client) startSeedVerifier(ctx context.Context, db *peersdb.DB) {
	if db == nil {
		return
	}
	go func() {
		if !c.waitReady(ctx, seedWiringReadyWait) {
			return
		}
		t := time.NewTicker(seedVerifyInterval)
		defer t.Stop()
		round := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			round++
			c.verifySeedsOnce(ctx, db)
			if round%seedStaleSweepRounds == 0 {
				c.sweepStaleSeeds(ctx, db)
			}
		}
	}()
}

// verifySeedsOnce 核对一轮：把「当前确实连上」的种子升级为已验证。
func (c *Client) verifySeedsOnce(ctx context.Context, db *peersdb.DB) {
	if c.node == nil || c.disc == nil {
		return
	}
	settings, err := readSeedSettings(ctx, db)
	if err != nil {
		return
	}
	verified := 0
	harvested := 0
	for _, scope := range []string{serverless.SeedScopeGroup, serverless.SeedScopeGlobal} {
		if !settings.enabledFor(scope) {
			continue
		}
		pScope, ok := seedScopeOf(scope)
		if !ok {
			continue
		}
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		seeds, lerr := db.ListSeeds(qctx, pScope, seedVerifyBatch)
		cancel()
		if lerr != nil {
			continue
		}
		for _, sd := range seeds {
			if sd.Verified() || sd.PeerID == c.peerID {
				continue
			}
			id, derr := peer.Decode(sd.PeerID)
			if derr != nil {
				continue
			}
			if c.node.Network().Connectedness(id) != network.Connected {
				continue // 没连上就什么都不做：不要刷 last_seen，让陈旧条目自然老化
			}
			nctx, ncancel := context.WithTimeout(ctx, 5*time.Second)
			_ = db.NoteSeedDialResult(nctx, pScope, sd.PeerID, true)
			c.refreshSeedAddrs(nctx, db, pScope, sd.PeerID, id)
			ncancel()
			verified++
		}

		// 自举采集：把「当前已连上的公网对端」收进种子表。
		hctx, hcancel := context.WithTimeout(ctx, 8*time.Second)
		harvested += c.harvestSeedCandidates(hctx, db, settings, pScope)
		hcancel()
	}
	if verified > 0 {
		c.logf("种子核对：%d 条种子由「已连接」升级为已验证", verified)
	}
	if harvested > 0 {
		c.logf("种子自举：从当前已连对端收进 %d 个公网入口", harvested)
	}
}

// harvestSeedCandidates 从「当前确实连上的对端」里挑出可当入口的**公网设备**，
// 落进种子表并标记为已验证。
//
// 为什么必须有这条路径：交换协议只能互相转发**已有的**条目，它不产生新条目。
// 没有自举，两张表会永远是空的、功能等于没做。
//
// 判据刻意收紧，宁可少收也不污染：
//   - 只认**当前已连上**的对端（不是猜、不是别人自报的）；
//   - 只认**带公网地址**的对端 —— NAT 后的私网地址对别人没用，收进来只会占名额；
//   - **群内范围只允许成员表里的对端**（成员表 = 同网络密钥，天然满足群隔离），
//     全域范围才允许任意已连对端（它本来就是跨密钥通道）。
func (c *Client) harvestSeedCandidates(ctx context.Context, db *peersdb.DB, settings SeedSettings, scope peersdb.SeedScope) int {
	if c.node == nil || !settings.enabledFor(string(scope)) {
		return 0
	}
	ids := make([]peer.ID, 0, seedHarvestBatch)
	if scope == peersdb.SeedScopeGroup {
		if c.disc == nil {
			return 0
		}
		for _, m := range c.disc.Peers() {
			if m.PeerID == "" || m.PeerID == c.peerID {
				continue
			}
			if id, err := peer.Decode(m.PeerID); err == nil {
				ids = append(ids, id)
			}
		}
	} else {
		ids = append(ids, c.node.Network().Peers()...)
	}

	added := 0
	for _, id := range ids {
		if added >= seedHarvestBatch {
			break
		}
		if id.String() == c.peerID {
			continue
		}
		if c.node.Network().Connectedness(id) != network.Connected {
			continue
		}
		addrs := c.node.Peerstore().Addrs(id)
		raw := make([]string, 0, len(addrs))
		for _, a := range addrs {
			if s := a.String(); s != "" {
				raw = append(raw, s)
			}
		}
		if len(raw) == 0 || !hasPublicAddr(raw) {
			continue // 没有公网地址：当不了别人的入口
		}
		sort.Strings(raw)
		wrote, err := db.UpsertSeed(ctx, scope, peersdb.Seed{
			PeerID: id.String(), Addrs: raw, PublicReachable: true, Source: "self",
		})
		if err != nil {
			continue
		}
		// 已经连上了，就是「本机实测拨通过」——直接过验证门。
		if err := db.NoteSeedDialResult(ctx, scope, id.String(), true); err != nil {
			continue
		}
		if wrote {
			added++
		}
	}
	return added
}

// refreshSeedAddrs 用 peerstore 里真正生效的地址覆盖种子地址。
//
// 只回写、不新增：连上的那一刻 peerstore 里就是这条链路的真实地址，
// 比交换来的（可能是几天前的自报）更可信。清空的情况直接跳过，
// 免得把已知地址冲掉。
func (c *Client) refreshSeedAddrs(ctx context.Context, db *peersdb.DB, scope peersdb.SeedScope, peerID string, id peer.ID) {
	addrs := c.node.Peerstore().Addrs(id)
	if len(addrs) == 0 {
		return
	}
	raw := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if s := a.String(); s != "" {
			raw = append(raw, s)
		}
	}
	if len(raw) == 0 {
		return
	}
	sort.Strings(raw) // 稳定顺序，避免每次核对都改写 updated_at
	_, _ = db.UpsertSeed(ctx, scope, peersdb.Seed{
		PeerID: peerID, Addrs: raw, PublicReachable: hasPublicAddr(raw), Source: "self",
	})
}

// sweepStaleSeeds 清理长期没动静的种子（两档容忍期，见 peersdb.PruneStaleSeeds）。
func (c *Client) sweepStaleSeeds(ctx context.Context, db *peersdb.DB) {
	settings, err := readSeedSettings(ctx, db)
	if err != nil {
		return
	}
	for _, scope := range []string{serverless.SeedScopeGroup, serverless.SeedScopeGlobal} {
		if !settings.enabledFor(scope) {
			continue
		}
		pScope, ok := seedScopeOf(scope)
		if !ok {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		n, perr := db.PruneStaleSeeds(sctx, pScope, seedIdleUnverified, seedIdleVerified)
		cancel()
		if perr == nil && n > 0 {
			c.logf("种子表（%s）清理了 %d 条长期不上线的记录", scope, n)
		}
	}
}

// hasPublicAddr 判断地址里是否有公网 IP（供种子排序/优选使用）。
func hasPublicAddr(addrs []string) bool {
	for _, a := range addrs {
		host := addrHost(a)
		if host == "" {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsPrivate() && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// addrHost 从 multiaddr 字符串里取 IP 部分（只认 /ip4/ 与 /ip6/ 两种形态）。
//
// 刻意不引入 multiaddr 解析：这里只做「有没有公网地址」的粗判，
// 字符串切分足够，且不必处理解析失败的边界。
func addrHost(a string) string {
	const (
		v4 = "/ip4/"
		v6 = "/ip6/"
	)
	switch {
	case len(a) > len(v4) && a[:len(v4)] == v4:
		return cutSegment(a[len(v4):])
	case len(a) > len(v6) && a[:len(v6)] == v6:
		return cutSegment(a[len(v6):])
	}
	return ""
}

// cutSegment 取到下一个 '/' 为止的一段。
func cutSegment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

// shortSeedID 日志用的短 ID。
func shortSeedID(id string) string {
	const keep = 10
	if len(id) <= keep {
		return id
	}
	return id[:keep] + "…"
}
