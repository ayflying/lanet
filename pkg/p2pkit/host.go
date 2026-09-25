package p2pkit

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

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

type HostSpec struct {
	ListenAddrs  []string
	RelayService bool
	RelaySource  autorelay.PeerSource
	UserAgent    string
	// Identity 节点私钥（可空 = 随机生成）。传入持久化密钥可使
	// PeerID 跨重启稳定，从而无服务器模式下的派生虚拟 IP 也稳定。
	Identity crypto.PrivKey
	// WebRTC 是否启用 webrtc-direct 传输层（浏览器 js-libp2p 可直连）。
	// 启用后自动在 ListenAddrs 基础上追加 /udp/<同端口+1>/webrtc-direct，
	// 或由 WebRTCAddrs 显式指定监听地址。
	WebRTC      bool
	WebRTCAddrs []string
	// HolePunching 启用 DCUtR 打洞（独立于 RelaySource；无服务器模式用）。
	HolePunching bool
	// RelayServiceDedicated 专用中继模式：无限配额 + 强制声明公网可达。
	// 普通 P2P 节点（客户端即服务端）不要开启，用 RelayService 即可。
	RelayServiceDedicated bool
	// RelayServiceAlways 无条件启动 Circuit Relay v2 hop 服务。
	// libp2p 内建 EnableRelayService 只在判定「公网可达」后才启动，
	// NAT 后节点永远等不到该事件；「节点即服务端」语义下需要
	// 打洞成功后立即具备中继能力，故用底层 relayv2.New 直接注册。
	RelayServiceAlways bool
}

// NewHost 创建 libp2p Host。
//
// 注意 AutoRelay 的行为边界：EnableAutoRelayWithPeerSource 只在节点
// 「认为自身不可达（private/NAT 后）」时才会向 relay 预约。公网可达的
// 节点不会预约，导致其他节点经中继找不到它（NO_RESERVATION）。
// 因此 agent 主流程在入网后还须调用 EnsureRelayReservation 主动预约，
// 保证「任意成员都能被经中继访问」这一兜底语义。
func NewHost(ctx context.Context, spec HostSpec) (host.Host, error) {
	options := []libp2p.Option{
		libp2p.UserAgent(spec.UserAgent),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
		libp2p.AddrsFactory(announceAddrs),
		libp2p.ConnectionGater(lanetOverlayGater{}),
	}
	if spec.Identity != nil {
		options = append(options, libp2p.Identity(spec.Identity))
	}

	if len(spec.ListenAddrs) > 0 {
		options = append(options, libp2p.ListenAddrStrings(spec.ListenAddrs...))
	}
	if spec.WebRTC {
		addrs := spec.WebRTCAddrs
		if len(addrs) == 0 {
			addrs = defaultWebRTCAddrs(spec.ListenAddrs)
		}
		if len(addrs) > 0 {
			options = append(options, libp2p.ListenAddrStrings(addrs...))
		}
	}
	if spec.RelayService {
		if spec.RelayServiceDedicated {
			options = append(options,
				libp2p.ForceReachabilityPublic(),
				libp2p.EnableRelayService(relayv2.WithInfiniteLimits()),
			)
		} else {
			// 节点即服务端：默认配额，可达性交给 AutoNAT 真实探测。
			options = append(options, libp2p.EnableRelayService())
		}
	}
	if spec.RelaySource != nil {
		options = append(options,
			libp2p.ForceReachabilityPrivate(),
			libp2p.EnableHolePunching(),
			libp2p.EnableAutoRelayWithPeerSource(spec.RelaySource),
		)
	} else if spec.HolePunching {
		options = append(options, libp2p.EnableHolePunching())
	}

	h, err := libp2p.New(options...)
	if err != nil {
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}
	if spec.RelayServiceAlways {
		// 底层 hop 服务：默认资源配额，不受 reachability 事件约束。
		if _, err = relayv2.New(h); err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("start relay service: %w", err)
		}
	}
	return h, nil
}

// IsLanetOverlayAddr reports whether addr points into Lanet's virtual IP range.
// Such an address can carry application traffic, but must never be advertised as
// a libp2p transport endpoint: dialing the tunnel through itself creates a loop.
func IsLanetOverlayAddr(addr ma.Multiaddr) bool {
	ip, ok := AddrIP(addr)
	return ok && isLanetOverlayIP(ip)
}

// isCircuitAddr 报告地址是否为 Circuit Relay v2 路径（含 /p2p-circuit 段）。
func isCircuitAddr(addr ma.Multiaddr) bool {
	if addr == nil {
		return false
	}
	_, err := addr.ValueForProtocol(ma.P_CIRCUIT)
	return err == nil
}

// isDialableUnderlay 报告地址是否值得作为 libp2p 承载地址（收发两侧共用判据）。
//
// 五类被剔除，判据只依赖地址形态、不需要任何对端信息：
//   - 回环（127.0.0.0/8、::1）：只有对端自己可达，拨它必然失败；
//   - 链路本地（169.254/16、fe80::/10）：只在同一物理链路上有意义；
//   - 未指定（0.0.0.0、::）：那是监听通配地址，不是可拨地址；
//   - lanet overlay（10.7.0.0/16）：拨它成环（见 IsLanetOverlayAddr）；
//   - /p2p-circuit：中继路径是临时中转，随中继与预约变化，不该被持久化。
//     真机实测某节点地址簿 404 条里有 130 条是 circuit 变体。
func isDialableUnderlay(addr ma.Multiaddr) bool {
	if isCircuitAddr(addr) {
		return false
	}
	ip, ok := AddrIP(addr)
	if !ok {
		return true // /dns4/… 之类无 IP 的地址交给拨号侧解析
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	return !isLanetOverlayIP(ip)
}

// FilterUnderlayAddrs 只保留值得承载 libp2p 的地址，顺序不变。
//
// 这是**收发共用的唯一出口**：出向挂在 libp2p.AddrsFactory 上（决定本机
// 对外通告什么），入向用于 peerstore 写入前的清洗（见 serverless.addMember
// 与 handleInfo）。一处改动即同时收敛「自己别乱发」与「别把对端的脏地址收下」。
//
// 保底：若过滤后一条不剩（典型是单元测试只监听 127.0.0.1，或节点只在链路
// 本地可见），退回「既非 overlay 也非 circuit」的那批——特殊拓扑仍保留连接
// 能力，但 overlay / circuit 永不复活。全都不合规时返回空：宁可不通告，
// 也不通告必然失败的地址。
func FilterUnderlayAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	filtered := make([]ma.Multiaddr, 0, len(addrs))
	for _, addr := range addrs {
		if isDialableUnderlay(addr) {
			filtered = append(filtered, addr)
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	fallback := make([]ma.Multiaddr, 0, len(addrs))
	for _, addr := range addrs {
		if !IsLanetOverlayAddr(addr) && !isCircuitAddr(addr) {
			fallback = append(fallback, addr)
		}
	}
	return fallback
}

// isHarmfulUnderlayAddr 报告地址是否「结构性有害」——它不只是可能拨不通，
// 而是会造成实际问题或无限膨胀：
//   - overlay（10.7.0.0/16）：经隧道再拨隧道会成环；
//   - circuit：中继路径是临时中转，随中继与预约变化，每轮都会被重新通告。
//
// 与 isDialableUnderlay 的分工很关键：那个判据用于**收集与通告**（把可能
// 拨不通的地址挡在地址簿与对外通告之外，避免扩散）；本判据用于**清理已经
// 写进 peerstore 的存量**——只清确定有害的，绝不顺手删掉回环 / 链路本地，
// 因为同机多实例、同一物理链路等特殊拓扑下它们可能是唯一通路。
// 实测教训：早期版本用「不可达」判据清 peerstore，直接把单测里靠 127.0.0.1
// 建立的 DHT 发现链切断（TestDualDHTPrivateDiscovery 失败）。
func isHarmfulUnderlayAddr(addr ma.Multiaddr) bool {
	return IsLanetOverlayAddr(addr) || isCircuitAddr(addr)
}

// PrunePeerstoreAddrs 清掉 peerstore 里「结构性有害」的地址（overlay /
// circuit），返回清理条数；幂等，可在每轮发现时调用。
//
// 为什么必须主动清：peerstore 里的地址带 1 小时 TTL，但发现循环每轮都会把
// 对端通告的地址重新写入，等价于永不过期。老版本节点仍在乱发地址，只有在
// 写入侧持续清洗，存量脏地址才会收敛。
func PrunePeerstoreAddrs(ps peerstore.Peerstore, id peer.ID) int {
	if ps == nil {
		return 0
	}
	removed := 0
	for _, addr := range ps.Addrs(id) {
		if !isHarmfulUnderlayAddr(addr) {
			continue
		}
		ps.SetAddr(id, addr, 0) // TTL=0 即删除
		removed++
	}
	return removed
}

// maxUnderlayAddrsPerPeer 单个节点在 peerstore / 地址簿里保留的地址条数上限。
//
// 取值参考：正常节点 1~2 块网卡 × 2~3 种传输 ≈ 6 条；12 条留足多网卡 / 多
// 端口场景的余量，同时把「几十条」压到不会造成拨号风暴的量级。
const maxUnderlayAddrsPerPeer = 12

// announceAddrs 本机对外通告的地址工厂（挂在 libp2p.AddrsFactory 上，
// 决定 identify / DHT 向外宣告什么）。
//
// 自定义对外地址（AdvertiseAddrs，见 container.go）存在时**整体替换**：
// 对外只通告用户声明的地址。这与 ShareableAddrs 的替换语义保持同一口径——
// 否则会出现「连接码里只有自定义地址、对端 identify 却学到一堆内网地址」
// 的分裂，自定义就失去了意义。
//
// 没有自定义地址时走 CleanUnderlayAddrs。注意后者还是**入向**清洗的公共
// 入口（拨号前、写 peerstore 前都调它），所以替换逻辑必须独立成函数包在
// 外面，绝不能写进 CleanUnderlayAddrs 本体——对端地址的清洗与本机的对外
// 声明是两回事，混在一起会让本机的自定义地址污染对端地址的处理路径。
func announceAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	if custom := AdvertiseAddrs(); len(custom) > 0 {
		return custom
	}
	return CleanUnderlayAddrs(addrs)
}

// CleanUnderlayAddrs = 剔除不可达 + 按可达性排序 + 折叠同链路冗余，
// 是「拿到一批候选地址后」的标准入口：拨号前、写 peerstore / 地址簿前都用它。
// 也直接挂在 libp2p.AddrsFactory 上（决定本机对外通告什么）。
func CleanUnderlayAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	filtered := FilterUnderlayAddrs(addrs)
	if len(filtered) == 0 {
		return filtered
	}
	// 复用项目既有的两级收敛：SortByReachability 按网卡类型排序（物理网卡
	// 私网 > 公网 > 隧道网卡 > 宿主虚拟交换机 > 链路本地），再由
	// CollapseRedundant 按「链路前缀 + 传输」折叠冗余。后者正是把
	// 「一块网卡 × 多端口 × 多传输」压到个位数的关键——实测单节点曾累积
	// 110 条，绝大多数是同链路的重复形态。
	out := CollapseRedundant(SortByReachability(filtered))
	// 保底：SortByReachability 面向「分享给对端」，会主动剔除回环与未指定
	// 地址。若过滤结果只剩它们（单测监听 127.0.0.1、同机多实例），这里会
	// 返回空——而本函数还挂在 AddrsFactory 上，返回空等于让 host 变成零地址，
	// 别人再也拿不到本机任何地址。此时退回过滤结果（宁可保留弱地址）。
	// 实测教训：少了这段保底，TestDualDHTPrivateDiscovery 的引导种子会变空，
	// 私有 DHT 起不来（B 只能等 A 反向连入，来源退化成 inbound）。
	if len(out) == 0 {
		out = filtered
	}
	// 端口不同的地址折叠不掉（折叠键含端口），必须再截断一道：真机实测
	// 一台装了 WSL/VMware 的机器会通告「5 个网卡 × 3 个端口 × 4 种传输」
	// 共几十条，截断前单节点地址簿曾累积 110 条。
	if len(out) > maxUnderlayAddrsPerPeer {
		out = out[:maxUnderlayAddrsPerPeer]
	}
	return out
}

// lanetOverlayGater is the final guard against old peers reintroducing overlay
// addresses through DHT or Identify after discovery-time filtering. It blocks
// nested libp2p connections over the TUN in both directions.
type lanetOverlayGater struct{}

var _ connmgr.ConnectionGater = lanetOverlayGater{}

func (lanetOverlayGater) InterceptPeerDial(peer.ID) bool { return true }

func (lanetOverlayGater) InterceptAddrDial(_ peer.ID, addr ma.Multiaddr) bool {
	return !IsLanetOverlayAddr(addr)
}

func (lanetOverlayGater) InterceptAccept(addrs network.ConnMultiaddrs) bool {
	return !IsLanetOverlayAddr(addrs.RemoteMultiaddr())
}

func (lanetOverlayGater) InterceptSecured(_ network.Direction, _ peer.ID, addrs network.ConnMultiaddrs) bool {
	return !IsLanetOverlayAddr(addrs.RemoteMultiaddr())
}

func (lanetOverlayGater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

// defaultWebRTCAddrs 依据 TCP/QUIC 监听地址推导 webrtc-direct 监听地址：
// 端口 = 原 UDP 端口 + 101（避开 quic 端口），同网段监听。
// 仅识别 /ip4 与 /ip6 的 udp quic 地址；无匹配时返回空（不额外监听）。
func defaultWebRTCAddrs(listenAddrs []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range listenAddrs {
		addr, err := ma.NewMultiaddr(raw)
		if err != nil {
			continue
		}
		ipComponent, ipErr := addr.ValueForProtocol(ma.P_IP4)
		if ipErr != nil {
			ipComponent, ipErr = addr.ValueForProtocol(ma.P_IP6)
		}
		if ipErr != nil {
			continue
		}
		udpComponent, udpErr := addr.ValueForProtocol(ma.P_UDP)
		if udpErr != nil {
			continue
		}
		port := 101
		if udpComponent != "0" {
			if parsed, parseErr := strconv.Atoi(udpComponent); parseErr == nil {
				port = parsed + 101
			}
		}
		target := fmt.Sprintf("/ip%s/%s/udp/%d/webrtc-direct",
			map[bool]string{true: "4", false: "6"}[strings.Contains(ipComponent, ".")],
			ipComponent, port)
		if !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
	}
	return out
}

func AddrInfo(h host.Host) peer.AddrInfo {
	return peer.AddrInfo{
		ID:    h.ID(),
		Addrs: h.Addrs(),
	}
}

// CandidateSource 一次返回一批中继候选（serverless.Discovery.Candidates 即此形状）。
type CandidateSource func(ctx context.Context, number int) ([]peer.AddrInfo, error)

// PeerSourceFromCandidates 把「一次返回候选列表」的函数适配成 autorelay 需要的
// 流式 PeerSource。
//
// 为什么需要这层适配：autorelay.PeerSource 是 `func(ctx, n) <-chan peer.AddrInfo`，
// 而发现服务天然提供的是「同步取一批」的 Candidates。中间这层薄适配让候选
// 逻辑（限流、去幽灵、地址筛选）只写一遍，不必为接口形态再实现一次。
//
// 不做缓存：autorelay 需要候选时就现取，避免拿着过期的离线节点反复重试
// （候选筛选本身是纯本地内存读，开销可忽略）。
func PeerSourceFromCandidates(src CandidateSource, fallbackNumber int) autorelay.PeerSource {
	return func(ctx context.Context, number int) <-chan peer.AddrInfo {
		out := make(chan peer.AddrInfo)
		if src == nil {
			close(out)
			return out
		}
		if number <= 0 {
			number = fallbackNumber
		}
		go func() {
			defer close(out)
			candidates, err := src(ctx, number)
			if err != nil {
				return
			}
			for _, candidate := range candidates {
				select {
				case <-ctx.Done():
					return
				case out <- candidate:
				}
			}
		}()
		return out
	}
}

// EnsureRelayReservation 向候选中继逐个发起预约，成功一次即返回。
// 用途：agent 入网后主动在 relay 上留下预约（Reservation），
// 使任意其他成员都能经中继访问本节点——AutoRelay 只在节点
// 自认不可达时才预约，公网可达节点必须靠这里兜底。
// source 为 nil 或全部候选预约失败时返回错误（不阻塞主流程，可周期重试）。
func EnsureRelayReservation(ctx context.Context, h host.Host, source autorelay.PeerSource, number int) error {
	if source == nil {
		return fmt.Errorf("nil relay source")
	}
	if number < 1 {
		number = 2
	}
	for candidate := range source(ctx, number) {
		reserveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := relayclient.Reserve(reserveCtx, h, candidate)
		cancel()
		if err == nil {
			return nil
		}
		// 记录但不中断：继续尝试下一个候选。
	}
	return fmt.Errorf("no relay reservation succeeded")
}

// IPv6ListenAvailable 探测本机能否监听 IPv6。
//
// 用真实监听试探，而不是只查网卡上有没有 IPv6 地址：某些环境明明有 IPv6
// 地址却禁止绑定（系统策略、宿主未放行、容器网络命名空间限制），只看地址
// 会得出错误结论，进而让 libp2p 监听失败、整个节点起不来。
// 探测用端口 0（内核自动分配）并立即关闭，无副作用、成本极低。
func IPv6ListenAvailable() bool {
	ln, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// HasIPv6Listen 判断监听地址列表里是否含 IPv6 项。
func HasIPv6Listen(addrs []string) bool {
	for _, raw := range addrs {
		if a, err := ma.NewMultiaddr(raw); err == nil {
			if _, ipErr := a.ValueForProtocol(ma.P_IP6); ipErr == nil {
				return true
			}
		}
	}
	return false
}

// StripIPv6Listen 去掉监听地址里的 IPv6 项，保留其余项与原顺序。
// 用途：IPv6 监听失败时降级重试（宁可只跑 IPv4，也不能让节点起不来）。
func StripIPv6Listen(addrs []string) []string {
	out := make([]string, 0, len(addrs))
	for _, raw := range addrs {
		if a, err := ma.NewMultiaddr(raw); err == nil {
			if _, ipErr := a.ValueForProtocol(ma.P_IP6); ipErr == nil {
				continue
			}
		}
		out = append(out, raw)
	}
	return out
}
