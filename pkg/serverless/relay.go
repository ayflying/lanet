// 本文件：中继候选治理 + 「常驻中继预约」。
//
// ============================ 为什么需要 ============================
//
// 无服务器（standalone）模式下，SDK 的 Client.Run 在 `c.disc != nil` 时直接
// return（见 sdk/go/lanet/client.go），**从不调用 EnsureRelayReservation**。
// 而发行形态 `lanet` 只有一个程序、跑的就是 standalone 分支——也就是说
// 现网所有节点里，没有一个会主动去预约 circuitv2 中继。叠加两条 go-libp2p
// 的硬约束，纯 NAT 群（谁都没有公网 IP）就彻底建不起连接：
//
//  1. circuitv2 中继服务端对没有预约的请求直接回 NO_RESERVATION，不转发；
//  2. DCUtR 打洞要求「打洞请求必须来自已经在中继链路上的对端」
//     （go-libp2p/p2p/protocol/holepunch/svc.go：a hole punch request should
//     only come from peers behind a relay）。
//
// 于是「没有公网设备 → 找不到好友也打不了洞」并非拓扑必然，而是这段代码
// 缺失导致的。这里补上常驻预约：入网后周期性向候选预约，成功一次即持有。
//
// ============================ 候选怎么挑 ============================
//
// 原实现是「遍历成员表取前 N 个」，有三个真机实证的问题：
//   - 成员表是 map，遍历顺序随机 → 每次给的候选都不一样，autorelay 反复
//     对着不同节点重试；
//   - 不过滤地址：历史身份遗留的 169.254.x.x（link-local）与 /p2p-circuit
//     地址也会被当候选，前者对别的机器永远不可达，后者等于「经中继去预约
//     另一个中继」，成环且几乎必然失败；
//   - 不过滤成员新鲜度：已经超期（等同幽灵）的成员仍会被挑中，白等一轮超时。
//
// 现在按「已验证种子 → 当前已连上的成员 → 近期活跃成员」的顺序给候选，
// 且统一做地址筛选，行为稳定且不浪费拨号预算。
package serverless

import (
	"context"
	"sort"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	// relayCandidatesMax 单次交给 autorelay 的候选上限。
	relayCandidatesMax = 10
	// relayReserveNumber 期望持有的中继数量（凑够一个保底链路即可，
	// 多了纯属浪费预约请求）。
	relayReserveNumber = 2
	// relayReserveInitialDelay 入网后多久开始第一次预约：避开启动期的
	// DHT 自举与首轮广播，那段时间连候选都还没进成员表。
	relayReserveInitialDelay = 12 * time.Second
	// relayReserveInterval 预约补充周期。circuitv2 预约有效期约 1 小时，
	// 15 分钟补一次余量充足，而一次预约只是一个流请求，开销可忽略。
	relayReserveInterval = 15 * time.Minute
	// relayReserveBudget 单轮预约的总预算（内部每个候选各有 10s 子预算）。
	relayReserveBudget = 30 * time.Second
	// relayReserveLogEvery 连续失败时每多少次打一条日志（降噪：
	// 纯 NAT 群里没有公网设备时，预约失败是常态，不该刷屏）。
	relayReserveLogEvery = 4
)

// relayUsableAddrs 从对端的已知地址里挑出「可用于中继预约」的那些。
//
// 剔除三类：
//   - /p2p-circuit 中继地址：经中继去预约另一个中继会让保底链路依赖别人的
//     保底链路，成环且必然失败；
//   - 链路本地（169.254/16、fe80::/10）：只在同一物理链路上有意义，
//     对另一台机器永远不可达；
//   - 回环 / 未指定：本来就是监听或本机地址，不是可拨地址。
//
// 其余地址按可达性排序（公网 IPv6 → 出网网卡私网 → 公网 IPv4 → …），
// 让 autorelay 先试最可能通的。
func relayUsableAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	for _, a := range p2pkit.SortByReachability(addrs) {
		if _, err := a.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			continue
		}
		ip, ok := p2pkit.AddrIP(a)
		if !ok {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		out = append(out, a)
	}
	return out
}

// relayCandidate 内部排序用的候选表示。
type relayCandidate struct {
	id        peer.ID
	peerID    string
	lastSeen  time.Time
	addrs     []ma.Multiaddr
	connected bool
	tier      int // 0 = 种子，1 = 成员
}

// collectRelayCandidates 收集并排序中继候选（调用方需已持 d.mu 读锁或无并发写）。
func (d *Discovery) collectRelayCandidates(ctx context.Context, number int) []peer.AddrInfo {
	self := d.host.ID()
	netw := d.host.Network()
	store := d.host.Peerstore()
	// 幽灵成员过滤：与 reapExpired 同一时限。超期未通讯的成员即使还在表里
	// （回收周期没到），也不该拿去预约。
	cutoff := time.Now().Add(-d.memberTTL)

	candidates := make([]relayCandidate, 0, len(d.members)+2)

	// 第一层：已验证种子。它们是「本机实测拨通过的入口」，最可能真的可用，
	// 且不要求同群、不要求加过好友（见 Config.SeedCandidates）。
	for _, ai := range d.seedCandidates(ctx, number) {
		if ai.ID == self || len(ai.Addrs) == 0 {
			continue
		}
		addrs := relayUsableAddrs(ai.Addrs)
		if len(addrs) == 0 {
			continue
		}
		candidates = append(candidates, relayCandidate{
			id: ai.ID, peerID: ai.ID.String(), addrs: addrs,
			connected: netw.Connectedness(ai.ID) == network.Connected, tier: 0,
		})
	}

	// 第二层：成员表里的同群节点（含好友与被动发现到的同密钥节点）。
	for _, m := range d.members {
		if m.PeerID == self.String() || m.LastSeen.Before(cutoff) {
			continue
		}
		id, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		addrs := relayUsableAddrs(store.Addrs(id))
		if len(addrs) == 0 {
			continue
		}
		candidates = append(candidates, relayCandidate{
			id: id, peerID: m.PeerID, lastSeen: m.LastSeen, addrs: addrs,
			connected: netw.Connectedness(id) == network.Connected, tier: 1,
		})
	}

	// 稳定排序 + 去重 + 截断（抽成纯函数，便于单测）。
	return orderRelayCandidates(candidates, number)
}

// orderRelayCandidates 对候选做稳定排序、去重、截断，产出 peer.AddrInfo。
//
// 排序键（依次）：种子优先（tier）→ 已连上的优先（现成可达，预约最可能成功）
// → 最近见过优先 → 节点 ID（兜底，保证同一输入必得同一输出，避免 autorelay
// 每轮对着不同候选反复重试）。
//
// 抽成独立纯函数的原因：真实 collectRelayCandidates 需要 libp2p host 才能跑，
// 而排序/去重/截断才是最容易出错、也最值得回归保护的部分。
func orderRelayCandidates(candidates []relayCandidate, number int) []peer.AddrInfo {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.connected != b.connected {
			return a.connected
		}
		if !a.lastSeen.Equal(b.lastSeen) {
			return a.lastSeen.After(b.lastSeen)
		}
		return a.peerID < b.peerID
	})

	out := make([]peer.AddrInfo, 0, number)
	seen := make(map[peer.ID]bool, len(candidates))
	for _, c := range candidates {
		if len(out) >= number {
			break
		}
		if seen[c.id] {
			continue
		}
		seen[c.id] = true
		out = append(out, peer.AddrInfo{ID: c.id, Addrs: c.addrs})
	}
	return out
}

// startRelayReservation 启动常驻中继预约循环（异步、随 ctx 结束、只启一次）。
//
// 无服务器模式下这是唯一会让节点在中继上留下预约的地方（见文件头注释）。
// 失败不重试得太密：没有公网设备时失败是常态，15 分钟一轮足够，
// 也避免纯 NAT 群互相刷预约请求。纯本地候选筛选 + 一次流请求，流量可忽略。
func (d *Discovery) startRelayReservation(ctx context.Context) {
	d.relayOnce.Do(func() {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(relayReserveInitialDelay):
			}
			source := p2pkit.PeerSourceFromCandidates(d.Candidates, relayReserveNumber)
			fails := 0
			announced := false
			for {
				runCtx, cancel := context.WithTimeout(ctx, relayReserveBudget)
				err := p2pkit.EnsureRelayReservation(runCtx, d.host, source, relayReserveNumber)
				cancel()
				switch {
				case err == nil:
					// 只在「首次成功」或「从中断中恢复」时打日志，避免每轮刷屏。
					if !announced || fails > 0 {
						d.logf("中继预约成功（无服务器模式常驻保底链路已建立）")
					}
					announced = true
					fails = 0
				default:
					fails++
					// 首轮必打一次，便于判断「是从来没有候选，还是候选都不给预约」；
					// 之后每 relayReserveLogEvery 次一条，避免纯 NAT 群刷屏。
					if fails == 1 || fails%relayReserveLogEvery == 0 {
						d.logf("中继预约未成功（第 %d 次，稍后自动重试）: %v", fails, err)
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(relayReserveInterval):
				}
			}
		}()
	})
}
