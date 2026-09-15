// 地址可达性排序：把本机监听到的一堆候选地址，按「对端最可能拨通」
// 的顺序排列，供连接种子 / 连接码分享使用。
//
// 为什么需要：一台机器常有多块网卡（物理网卡、公网 IPv6、VPN、虚拟网卡、
// 未启用网卡的 169.254 自动配置地址），libp2p 会把监听地址按接口全量展开，
// 顺序随机。而拨号侧（serverless.DialSeed）是**逐个尝试、总预算 15 秒**，
// 排在前面却不可达的地址会先把预算吃掉，导致真正能通的地址根本没机会试。
// 排序把「跨网直连最佳」的公网 IPv6 与「同局域网直连」的私有 IPv4 顶到前面，
// 把几乎必然不可达的 link-local 压到最后。
package p2pkit

import (
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"
)

// 地址可达性等级，数值越小越优先（即拨号尝试顺序）。
const (
	// RankGlobalIPv6 全局 IPv6（2000::/3）：公网可路由，跨网直连最佳路径，
	// 不需要任何中继或打洞——有它就该最先试。
	RankGlobalIPv6 = 0
	// RankPrimaryIPv4 「主接口」的私有 IPv4：操作系统出网（默认路由）所走的
	// 那块网卡上的 RFC1918 地址。它几乎总是真实物理网卡，是「同一局域网内
	// 一定能拨通」的那条路，也是本机最该被分享出去的一条地址。
	//
	// 为什么单列一级：只按「是不是 RFC1918」排序时，隧道类虚拟网卡（真机实测
	// 本机有 NodeBabyLink Tunnel 的 10.222.222.1）与物理网卡同处一级，会凭
	// libp2p 给出的随机顺序抢到更前位次，把连接码名额挤掉一块。改用
	// 「OS 自己走哪块网卡出网」作**正向**判据后，无需认识任何虚拟网卡品牌，
	// 物理网卡就能稳居首位。
	RankPrimaryIPv4 = 1
	// RankPrivateIPv4 其他真实网卡（带 MAC 地址）的 RFC1918 私有 IPv4
	// （10/8、172.16/12、192.168/16）：第二块物理网卡、USB 网卡等。
	// 对端与本机在同一局域网时直连成功率高。
	RankPrivateIPv4 = 2
	// RankPublicIPv4 公网 IPv4：需端口映射或打洞，成功率次之。
	RankPublicIPv4 = 3
	// RankULAIPv6 唯一本地地址 fc00::/7：通常只在特定隧道/VPN 内可达。
	RankULAIPv6 = 4
	// RankTunnelIface VPN / 隧道网卡上的地址：只有同一 VPN 网络内的对端能连上，
	// 但**该对端确实存在**（跨网场景里它常常正是能通的那条路），故仍然分享，
	// 只是不该排在物理网卡前面。两类都归这里：
	//   - 名字可识别的 VPN（ZeroTier、Tailscale、WireGuard 等）；
	//   - 名字认不出、但**没有 MAC 地址**的网卡：Windows 上隧道网卡的物理地址
	//     就是空的（真机实测 NodeBabyLink Tunnel、WireGuard Tunnel 均为空），
	//     而真实网卡必有 MAC，故按隧道对待比按物理网卡对待更不容易出错。
	RankTunnelIface = 5
	// RankOther 其他（含未指定地址等无法归类的）。
	RankOther = 6
	// RankHostVirtualIface 宿主内部虚拟交换机与容器网桥（WSL、Hyper-V Default
	// Switch、VMware/VirtualBox 宿主网络、docker0/br-*/virbr0/veth*）的地址：
	// 只有**同一台宿主机上的**虚拟机 / 容器能连上，对「另一台真实机器」永远
	// 不可达。真机实测未区分时，正是它们与 VPN 网卡一起把连接码名额占满、
	// 物理网卡落榜。故排在链路本地之前：有余量时仍会分享（同宿主 VM 场景
	// 确实有用），名额一紧张就最先被牺牲。
	RankHostVirtualIface = 7
	// RankLinkLocal 链路本地（169.254/16、fe80::/10）：只在同一物理链路
	// 且双方都处于自动配置状态时才可达，实际几乎必然失败，排最后。
	RankLinkLocal = 8
)

// AddrReachabilityRank 返回 multiaddr 的可达性等级。
// 不含 IP 协议段（如 /dns4、/p2p-circuit 中继地址）的返回 RankOther。
func AddrReachabilityRank(a ma.Multiaddr) int {
	ip, ok := AddrIP(a)
	if !ok {
		return RankOther
	}
	// 中继地址（/p2p-circuit/...）虽然含 IP，走的是别人的转发路径，
	// 与本机网卡的直连能力无关，不参与「哪块网卡更优」的判断。
	if _, err := a.ValueForProtocol(ma.P_CIRCUIT); err == nil {
		return RankOther
	}
	if ip.IsLoopback() {
		return RankOther // 调用方负责剔除，这里给中性等级
	}
	if ip.Is4() {
		switch {
		case ip.IsLinkLocalUnicast():
			return RankLinkLocal
		case ip.IsPrivate():
			return RankPrivateIPv4
		case ip.IsUnspecified():
			return RankOther
		default:
			return RankPublicIPv4
		}
	}
	switch {
	case ip.IsLinkLocalUnicast():
		return RankLinkLocal
	case ip.IsUnspecified():
		return RankOther
	case ip.IsPrivate(): // fc00::/7（ULA）
		return RankULAIPv6
	case ip.IsGlobalUnicast():
		return RankGlobalIPv6
	default:
		return RankOther
	}
}

// SortByReachability 过滤 + 去重 + 按可达性稳定排序，返回新切片。
//
// 过滤掉三类对分享毫无价值的地址：
//   - Lanet overlay（10.7.0.0/16）：经自身隧道再拨号会成环；
//   - 回环（127.0.0.0/8、::1）：只有本机可达；
//   - 未指定（0.0.0.0、::）：是监听通配地址，不是可拨地址。
//
// 排序是**稳定**的：同一等级内保持 libp2p 给出的原顺序，
// 避免把「同 IP 的 tcp 与 quic」这类等价候选打乱成不可预期的次序。
//
// 排序不只按地址形态，还要结合**本机网卡类别**（见 ifaceAddrSets）：主接口
// （OS 出网网卡）的私有 IPv4 提到最前，VPN / 隧道网卡次之，宿主内部虚拟交换机
// （WSL、Hyper-V、docker 网桥）压到链路本地之前。真机实测不区分网卡类别时，
// 连接码名额会被 ZeroTier 与 WSL 网卡占满、物理网卡 192.168.50.170 落榜。
func SortByReachability(addrs []ma.Multiaddr) []ma.Multiaddr {
	return sortByReachability(addrs, localIfaceAddrSets())
}

// sortByReachability 是 SortByReachability 的实现体，网卡分类由参数注入
// （生产代码传本机枚举结果，测试传固定集合，从而不依赖运行测试的机器有哪些网卡）。
func sortByReachability(addrs []ma.Multiaddr, sets *ifaceAddrSets) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	seen := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if IsLanetOverlayAddr(a) {
			continue
		}
		if ip, ok := AddrIP(a); ok {
			if ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
		}
		key := a.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return sets.rankOf(out[i]) < sets.rankOf(out[j])
	})
	return out
}

// ifaceAddrSets 本机网卡的分类结果：IP 字符串 → 类别归属，三张表互不重叠。
type ifaceAddrSets struct {
	// primary 主接口（OS 出网网卡）上的**全部**地址。按网卡而非单个 IP 记录：
	// IPv6 隐私扩展地址会轮换，出网探到的那个地址随时可能变，但「哪块网卡在
	// 出网」是稳定的。
	primary map[string]bool
	// tunnel VPN / 隧道网卡上的地址。
	tunnel map[string]bool
	// hostVirtual 宿主内部虚拟交换机 / 容器网桥上的地址。
	hostVirtual map[string]bool
}

// rankOf 返回地址的最终等级（在 AddrReachabilityRank 之上叠加网卡类别）。
//
// 规则：
//   - 链路本地永远最后，与网卡类别无关——隧道网卡上的 fe80:: 同样不可达；
//   - 宿主内部虚拟交换机 / 隧道网卡上的地址整体降到对应等级，**不区分它是
//     私有还是公网地址**（Hyper-V 上有公网 IPv6 也一样只有同宿主可达）；
//   - 主接口的私有 IPv4 提到 RankPrimaryIPv4，压过其它私有 IPv4。
func (s *ifaceAddrSets) rankOf(a ma.Multiaddr) int {
	base := AddrReachabilityRank(a)
	if s == nil {
		return base
	}
	ip, ok := AddrIP(a)
	if !ok {
		return base
	}
	key := ip.String()
	switch {
	case base == RankLinkLocal:
		return RankLinkLocal
	case s.hostVirtual[key]:
		return RankHostVirtualIface
	case s.tunnel[key]:
		return RankTunnelIface
	case base == RankPrivateIPv4 && s.primary[key]:
		return RankPrimaryIPv4
	}
	return base
}

// hostVirtualIfaceHints 判定「宿主内部虚拟交换机 / 容器网桥」的接口名关键字
// （小写子串匹配）。这类网卡只有**同一台宿主机上的**虚拟机 / 容器能连上，
// 对「另一台真实机器」永远不可达，故优先级最低（只高于链路本地）。
//
// **刻意不收** tun、tap、bridge 这类短关键词：它们是极短的子串，容易误伤
// 真实网卡名（如 Fortune、Bridgetown），一旦误判就会把真正对外的网卡降级。
var hostVirtualIfaceHints = []string{
	// Windows 宿主内部虚拟交换机（WSL 与 Hyper-V Default Switch 都是 vEthernet (…)，故 vethernet 命中）
	"vethernet",
	"wsl",
	"hyper-v",
	"default switch",
	"vmware",
	"virtualbox",
	"vmnet",
	// 容器 / 虚拟化网桥（以 Linux 为主）
	"docker",
	"podman",
	"veth",
	"virbr",
	"libvirt",
	"br-", // docker 自定义网桥标准前缀（br-1a2b3c4d）；"Bridgetown" 小写后不含 "br-"，不会误伤
}

// tunnelIfaceHints 判定「VPN / 隧道网卡」的接口名关键字（小写子串匹配）。
//
// 这类网卡上的地址只有**同一 VPN 网络内**的对端能连上；但该对端确实存在，
// 跨网场景里常常正是能通的那条路，故仍然分享，只是必须排在真实网卡之后——
// 真机实测不区分时，连接码 4 个名额被 ZeroTier 与 WSL 网卡占满，物理网卡落榜。
//
// 同样刻意不收 tun、tap 这类短关键词（会误伤 Fortune 之类的真实网卡名）；
// 名字认不出的隧道网卡改由「空 MAC」判据兜住（见 isTunnelIface）。
var tunnelIfaceHints = []string{
	"zerotier",
	"tailscale",
	"wireguard",
	"hamachi",
	"softether",
	"openvpn",
}

// ifaceSetsTTL 网卡枚举结果的缓存时长。控制台 /api/state 每几秒轮询一次，
// 每次轮询都要算种子列表，而枚举接口在 Windows 上不算便宜，故做短期缓存；
// 网卡热插拔最多几秒后生效，可接受。
const ifaceSetsTTL = 30 * time.Second

var (
	ifaceSetsMu     sync.Mutex
	ifaceSetsCache  *ifaceAddrSets
	ifaceSetsExpire time.Time
)

// localIfaceAddrSets 返回本机网卡分类结果（带短期缓存），供 SortByReachability 使用。
func localIfaceAddrSets() *ifaceAddrSets {
	ifaceSetsMu.Lock()
	defer ifaceSetsMu.Unlock()
	if ifaceSetsCache != nil && time.Now().Before(ifaceSetsExpire) {
		return ifaceSetsCache
	}
	ifaceSetsCache, ifaceSetsExpire = enumerateIfaceAddrSets(), time.Now().Add(ifaceSetsTTL)
	return ifaceSetsCache
}

// enumerateIfaceAddrSets 是本机枚举的实现体（不缓存），单独抽出便于测试。
//
// 分类优先级：宿主内部虚拟交换机 → VPN / 隧道 → 主接口 → 其余（普通网卡）。
// 顺序有讲究：
//   - 宿主内部虚拟交换机放最前：若 OS 出网恰好走了 WSL 虚拟交换机，它仍只是
//     「同宿主可达」，不能因为坐在默认路由上就被当成物理网卡；
//   - 隧道先于主接口：若某台机器只能靠 ZeroTier 出网，那块网卡仍是隧道——
//     同 VPN 的对端能连、别的对端连不上，不该顶到物理网卡前面；
//   - 其余网卡不单列：它们按地址形态排序即可（私有 IPv4 已是第二优先级）。
func enumerateIfaceAddrSets() *ifaceAddrSets {
	s := &ifaceAddrSets{
		primary:     map[string]bool{},
		tunnel:      map[string]bool{},
		hostVirtual: map[string]bool{},
	}
	primaryNames := outboundIfaceNames()
	ifaces, err := net.Interfaces()
	if err != nil {
		return s
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		var dst map[string]bool
		switch {
		case isHostVirtualIface(ifc.Name):
			dst = s.hostVirtual
		case isTunnelIface(ifc.Name, ifc.HardwareAddr):
			dst = s.tunnel
		case primaryNames[ifc.Name]:
			dst = s.primary
		default:
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				dst[ipn.IP.String()] = true
			}
		}
	}
	return s
}

// outboundIfaceNames 返回「操作系统出网时实际走的那几块网卡」的名字集合。
//
// 做法：用 UDP 对公网地址做一次 connect——UDP 无连接，这一步**不发送任何数据
// 包**，只是让内核按路由表选一次源地址，再从 LocalAddr 反查是哪块网卡。因此它
// 在「外网 UDP 被防火墙拦截」的机器上同样成立（真机实测该机 UDP/53 被拦，
// 此调用依然正常返回物理网卡）。
//
// IPv4 与 IPv6 各探一次并取并集：双栈机器上两者可能落在不同网卡（如 IPv4 走
// 物理网卡、IPv6 走隧道），两块都算「OS 认可的出网口」。探不到时（无默认路由
// 的离线机器）返回空集，排序退化为「纯按地址形态」，不影响功能。
func outboundIfaceNames() map[string]bool {
	probed := make(map[string]bool)
	for _, target := range []string{"223.5.5.5:53", "[2400:3200::1]:53"} {
		conn, err := net.Dial("udp", target)
		if err != nil {
			continue
		}
		if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsUnspecified() {
			probed[ua.IP.String()] = true
		}
		_ = conn.Close()
	}
	return ifaceNamesHolding(probed)
}

// ifaceNamesHolding 把一组 IP 反查成「持有这些 IP 的网卡名」。
func ifaceNamesHolding(ips map[string]bool) map[string]bool {
	names := make(map[string]bool)
	if len(ips) == 0 {
		return names
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return names
	}
	for _, ifc := range ifaces {
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ips[ipn.IP.String()] {
				names[ifc.Name] = true
				break
			}
		}
	}
	return names
}

// isHostVirtualIface 判断网卡是否为宿主内部虚拟交换机 / 容器网桥。
func isHostVirtualIface(name string) bool {
	return containsAnyHint(strings.ToLower(name), hostVirtualIfaceHints)
}

// isTunnelIface 判断网卡是否为 VPN / 隧道类。
//
// 两个判据取或：
//   - 接口名命中已知品牌（ZeroTier、Tailscale、WireGuard…）；
//   - **物理地址为空**：Windows 上隧道类网卡的 MAC 就是空的（真机实测
//     NodeBabyLink Tunnel、WireGuard Tunnel 均为空），而真实网卡——包括
//     ZeroTier 这种纯虚拟网卡——都有 MAC。空 MAC 恰好只命中隧道，是
//     「不认识品牌也能认出来」的兜底判据；真机实测 NodeBabyLink 的名字里
//     没有任何关键字，正是靠它被正确降级，物理网卡 192.168.50.170 才得以上位。
func isTunnelIface(name string, mac net.HardwareAddr) bool {
	if containsAnyHint(strings.ToLower(name), tunnelIfaceHints) {
		return true
	}
	return len(mac) == 0
}

// containsAnyHint 在已转小写的接口名中做子串匹配。
func containsAnyHint(lower string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// DialableHostPort 取 multiaddr 里可直接拨号的「IP:端口」及其传输类型。
//
// 只认两类：裸 TCP（/tcp/<p>，后面不再跟 /ws、/wss 等需要额外协商的协议）
// 与 QUIC（/udp/<p>/quic-v1）。这两类用 IP:端口 就能完整表达。
// /ws、/webrtc-direct 的端口是独立的另一套监听，不能当普通 TCP 拨，
// 故返回 ok=false——生成连接码时必须跳过它们，否则对端拿到一个
// 「拨过去协议不对」的端口，白白浪费一次拨号尝试。
//
// transport 返回 "tcp" 或 "quic"，供调用方在同一块网卡上按需取舍
// （连接码只需一个代表端口，因为对端会据它同时展开 TCP 与 QUIC）。
func DialableHostPort(a ma.Multiaddr) (hostPort, transport string, ok bool) {
	ip, ipOK := AddrIP(a)
	if !ipOK {
		return "", "", false
	}
	host := ip.String()
	if p, err := a.ValueForProtocol(ma.P_TCP); err == nil {
		if _, wsErr := a.ValueForProtocol(ma.P_WS); wsErr != nil {
			if _, wssErr := a.ValueForProtocol(ma.P_WSS); wssErr != nil {
				return net.JoinHostPort(host, p), "tcp", true
			}
		}
	}
	if p, err := a.ValueForProtocol(ma.P_UDP); err == nil {
		if _, qErr := a.ValueForProtocol(ma.P_QUIC_V1); qErr == nil {
			return net.JoinHostPort(host, p), "quic", true
		}
	}
	return "", "", false
}

// AddrIP 从 multiaddr 取 IP，兼容带 zone 的 IPv6（fe80::1%12）与
// IPv4-mapped IPv6（::ffff:1.2.3.4 → 归一为 IPv4）。
func AddrIP(a ma.Multiaddr) (netip.Addr, bool) {
	if v, err := a.ValueForProtocol(ma.P_IP4); err == nil {
		if ip, perr := netip.ParseAddr(v); perr == nil {
			return ip.Unmap(), true
		}
		return netip.Addr{}, false
	}
	v, err := a.ValueForProtocol(ma.P_IP6)
	if err != nil {
		return netip.Addr{}, false
	}
	// 剥离 zone：ParseAddr 不接受 "fe80::1%12" 这种带接口名的形式。
	if i := strings.IndexByte(v, '%'); i >= 0 {
		v = v[:i]
	}
	ip, perr := netip.ParseAddr(v)
	if perr != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// ShareableAddrs 整理出「可供分享」的地址列表：过滤 → 去重 → 按可达性排序
// → 按链路前缀折叠冗余。连接种子与连接码统一走这里，保证两者口径一致。
func ShareableAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	return CollapseRedundant(SortByReachability(addrs))
}

// CollapseRedundant 折叠「同一条链路」上的冗余地址，保留每个组合的首条。
//
// 为什么需要：一块网卡常同时持有多个地址——Windows 的隐私扩展临时地址会让
// 同一块网卡的同一个 IPv6 前缀下出现七八个地址，同一个监听端口重复七八遍。
// 这些地址走的是**同一条物理链路**，对端连哪个都一样，留在分享列表里只会
// 挤占名额（实测：连接码 4 个名额被同一块网卡的隐私地址占满，局域网 IPv4
// 反而没能进榜，与「把所有网卡都分享出去」的目标正好相反）。
//
// 折叠键 = 网络前缀 + 传输（含端口）：
//   - 前缀：IPv4 取 /24、IPv6 取 /64；IPv4 链路本地取 /16——整块 169.254
//     都不可达，没必要按 /24 留六条。
//   - 传输：忽略 IP 段后的剩余 multiaddr（如 /tcp/4001、/udp/4001/quic-v1），
//     故同一链路上的 tcp、ws、quic 仍各留一条，不丢传输形态。
//
// 传入已排序列表时，保留的是该链路里可达性最好的那条。
func CollapseRedundant(addrs []ma.Multiaddr) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	seen := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		key := collapseKey(a)
		if key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, a)
	}
	return out
}

// collapseKey 生成折叠键；无法归类的地址返回空串（调用方一律保留）。
func collapseKey(a ma.Multiaddr) string {
	ip, ok := AddrIP(a)
	if !ok {
		return ""
	}
	sig, ok := transportSig(a)
	if !ok {
		return ""
	}
	return PrefixKey(ip) + "|" + sig
}

// PrefixKey 返回 IP 所属的链路前缀（IPv4 /24、IPv6 /64；IPv4 链路本地 /16）。
func PrefixKey(ip netip.Addr) string {
	bits := 64
	if ip.Is4() {
		bits = 24
		if ip.IsLinkLocalUnicast() {
			bits = 16
		}
	}
	p, err := ip.Prefix(bits)
	if err != nil {
		return ip.String()
	}
	return p.String()
}

// transportSig 返回去掉 IP 段后的 multiaddr 串（传输 + 端口的完整形态），
// 用于区分同一条链路上的不同传输。例如：
//
//	/ip4/1.2.3.4/tcp/4001     → /tcp/4001
//	/ip6/::1/udp/4001/quic-v1 → /udp/4001/quic-v1
//	/ip4/1.2.3.4/tcp/4001/ws  → /tcp/4001/ws
//
// 无 IP 段的地址返回 ok=false（不参与折叠）。
func transportSig(a ma.Multiaddr) (string, bool) {
	if _, ok := AddrIP(a); !ok {
		return "", false
	}
	parts := make([]string, 0, 4)
	for _, p := range a.Protocols() {
		if p.Code == ma.P_IP4 || p.Code == ma.P_IP6 {
			continue // 丢弃 IP 段本身
		}
		v, err := a.ValueForProtocol(p.Code)
		if err != nil {
			continue
		}
		parts = append(parts, "/"+p.Name+"/"+v)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, ""), true
}
