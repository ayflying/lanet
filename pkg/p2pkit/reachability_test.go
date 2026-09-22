package p2pkit

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

func TestAddrReachabilityRank(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		// 公网 IPv6 最优：跨网直连不依赖中继。
		{"/ip6/2408:824e:1592:7d80::577/tcp/4001", RankGlobalIPv6},
		// 私有 IPv4 次之：同一局域网直连。
		{"/ip4/192.168.50.170/tcp/4001", RankPrivateIPv4},
		{"/ip4/10.222.222.1/tcp/4001", RankPrivateIPv4},
		{"/ip4/172.25.64.1/tcp/4001", RankPrivateIPv4},
		// 公网 IPv4：需打洞/映射。
		{"/ip4/43.136.124.167/tcp/4001", RankPublicIPv4},
		// ULA IPv6：只在特定隧道内可达。
		{"/ip6/fd00::1/tcp/4001", RankULAIPv6},
		// 链路本地：几乎必然失败，排最后。
		{"/ip4/169.254.122.160/tcp/4001", RankLinkLocal},
		{"/ip6/fe80::ad53:adf6:5eae:b3c2/tcp/4001", RankLinkLocal},
		// 中继地址不参与「哪块网卡更优」的判断。
		{"/ip4/43.136.124.167/tcp/4001/p2p-circuit", RankOther},
		// 未指定地址是最低级的中性等级（正常会被 SortByReachability 剔除）。
		{"/ip4/0.0.0.0/tcp/4001", RankOther},
	}
	for _, c := range cases {
		if got := AddrReachabilityRank(ma.StringCast(c.addr)); got != c.want {
			t.Errorf("AddrReachabilityRank(%s) = %d, want %d", c.addr, got, c.want)
		}
	}
}

// TestShareableAddrsDropsDiscouragedAddrs 隧道 / 宿主虚拟网卡上的地址会被
// **直接从分享列表里剔除**（而不是仅降级）。
//
// 为什么必须剔除而不是降级：降级只改排序，名额有余量时脏地址照样被分享、
// 照样扩散。而连接码只有 4 个名额——真机实测本机连接码里混进了 ZeroTier 的
// 10.70.38.92 与 NodeBabyLink 的 10.222.222.1，对端拿去逐条拨号，时间预算
// 全耗在「只对同一台宿主上的虚拟机」或「同一个第三方 VPN 网络内」可达的
// 地址上，真正可达的那条反而没机会试。
//
// 这里用 shareableAddrsWith 显式注入剔除集合：真实判据来自本机网卡枚举
// （DiscouragedShareAddrs），依赖跑测试的机器上有哪些网卡，不适合进单测。
func TestShareableAddrsDropsDiscouragedAddrs(t *testing.T) {
	addrs := []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.50.170/tcp/49709"),
		ma.StringCast("/ip4/10.70.38.92/tcp/52000"),
		ma.StringCast("/ip4/10.222.222.1/tcp/4001"),
	}
	drop := map[string]bool{"10.70.38.92": true, "10.222.222.1": true}
	got := shareableAddrsWith(addrs, drop, nil)
	if len(got) != 1 {
		t.Fatalf("隧道 / 虚拟网卡地址应被剔除，只留物理网卡，got %v", got)
	}
	if !strings.Contains(got[0].String(), "192.168.50.170") {
		t.Fatalf("留下的应是物理网卡地址，got %v", got)
	}
}

// TestShareableAddrsKeepsAddrsWhenAllDiscouraged 保底：整机只挂着隧道 / 虚拟
// 网卡时（例如只能靠 ZeroTier 出网），不能因为治理策略把分享列表清空——
// 连接码 / 种子列表为空对用户完全不可用，宁可分享一条弱地址。
func TestShareableAddrsKeepsAddrsWhenAllDiscouraged(t *testing.T) {
	got := ShareableAddrs([]ma.Multiaddr{
		ma.StringCast("/ip4/10.70.38.92/tcp/52000"),
	})
	if len(got) == 0 {
		t.Fatal("过滤后为空时应退回未过滤结果，而不是返回空列表")
	}
}
func TestSortByReachabilityOrdersAndFilters(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/ip4/169.254.122.160/tcp/4001"),          // link-local：最后
		ma.StringCast("/ip4/10.7.207.102/tcp/4001"),             // overlay：剔除
		ma.StringCast("/ip4/192.168.50.170/tcp/4001"),           // 局域网
		ma.StringCast("/ip6/::1/tcp/4001"),                      // 回环：剔除
		ma.StringCast("/ip6/2408:824e:1592:7d80::577/tcp/4001"), // 公网 IPv6：最前
		ma.StringCast("/ip4/0.0.0.0/tcp/4001"),                  // 未指定：剔除
		ma.StringCast("/ip4/43.136.124.167/tcp/4001"),           // 公网 IPv4
	}
	got := SortByReachability(input)
	want := []string{
		"/ip6/2408:824e:1592:7d80::577/tcp/4001",
		"/ip4/192.168.50.170/tcp/4001",
		"/ip4/43.136.124.167/tcp/4001",
		"/ip4/169.254.122.160/tcp/4001",
	}
	if len(got) != len(want) {
		t.Fatalf("地址数不匹配：got %v", toStrings(got))
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("第 %d 条不匹配：got %s want %s（全量 %v）", i, got[i], want[i], toStrings(got))
		}
	}
}

// TestSortByReachabilityStableAndDedup 同等级保持原序（tcp 在 quic 前），
// 完全重复的地址只留一条。
func TestSortByReachabilityStableAndDedup(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.50.170/tcp/4001"),
		ma.StringCast("/ip4/10.70.38.92/udp/4001/quic-v1"),
		ma.StringCast("/ip4/192.168.50.170/tcp/4001"), // 重复
		ma.StringCast("/ip4/10.70.38.92/tcp/4001"),
	}
	got := toStrings(SortByReachability(input))
	want := []string{
		"/ip4/192.168.50.170/tcp/4001",
		"/ip4/10.70.38.92/udp/4001/quic-v1",
		"/ip4/10.70.38.92/tcp/4001",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// TestSortByReachabilityEmpty 空输入不 panic（节点只绑回环时会出现）。
func TestSortByReachabilityEmpty(t *testing.T) {
	if got := SortByReachability(nil); len(got) != 0 {
		t.Fatalf("空输入应返回空，got %v", toStrings(got))
	}
	if got := SortByReachability([]ma.Multiaddr{ma.StringCast("/ip6/::1/tcp/4001")}); len(got) != 0 {
		t.Fatalf("只剩回环时应返回空，got %v", toStrings(got))
	}
}

func TestDialableHostPort(t *testing.T) {
	cases := []struct {
		addr      string
		want      string
		wantTrans string
		wantOkay  bool
	}{
		// 裸 TCP 可拨。
		{"/ip4/192.168.50.170/tcp/49709", "192.168.50.170:49709", "tcp", true},
		// QUIC 可拨。
		{"/ip4/192.168.50.170/udp/53500/quic-v1", "192.168.50.170:53500", "quic", true},
		// IPv6 需加方括号，JoinHostPort 负责。
		{"/ip6/2408:824e::577/tcp/4001", "[2408:824e::577]:4001", "tcp", true},
		// ws 的端口是独立监听，不能用 IP:端口 直接拨。
		{"/ip4/192.168.50.170/tcp/49710/ws", "", "", false},
		// webrtc-direct 端口同理不可当普通 UDP 拨。
		{"/ip4/192.168.50.170/udp/101/webrtc-direct", "", "", false},
		// 无端口协议段。
		{"/ip4/192.168.50.170", "", "", false},
	}
	for _, c := range cases {
		got, trans, ok := DialableHostPort(ma.StringCast(c.addr))
		if ok != c.wantOkay || got != c.want || trans != c.wantTrans {
			t.Errorf("DialableHostPort(%s) = (%q,%q,%v), want (%q,%q,%v)",
				c.addr, got, trans, ok, c.want, c.wantTrans, c.wantOkay)
		}
	}
}

// TestAddrIP 覆盖各形态，并把 IPv4-mapped IPv6 归一为 v4。
// 注意：带 zone 的 IPv6（fe80::1%12）在 multiaddr 解析阶段就被拒绝，
// 构造不出这种地址——代码里的 zone 剥离只是防御性兜底，此处无法断言。
func TestAddrIP(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"/ip4/192.168.50.170/tcp/4001", "192.168.50.170"},
		{"/ip6/2408:824e::577/tcp/4001", "2408:824e::577"},
		{"/ip6/fe80::1/tcp/4001", "fe80::1"},
		{"/ip6/::ffff:1.2.3.4/tcp/4001", "1.2.3.4"}, // v4-mapped 归一
	}
	for _, c := range cases {
		ip, ok := AddrIP(ma.StringCast(c.addr))
		if !ok {
			t.Errorf("AddrIP(%s) 解析失败", c.addr)
			continue
		}
		if ip.String() != c.want {
			t.Errorf("AddrIP(%s) = %s, want %s", c.addr, ip, c.want)
		}
	}
	if _, ok := AddrIP(ma.StringCast("/dns4/example.com/tcp/4001")); ok {
		t.Error("无 IP 协议段应返回 false")
	}
}

// toStrings 测试辅助：multiaddr 列表转字符串列表。
func toStrings(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// TestCollapseRedundantSamePrefix 同一块网卡的多个地址（同前缀同传输）折叠成一条，
// 但不同传输、不同前缀都要保留——这是「每块网卡 × 每种传输各一条」的关键。
func TestCollapseRedundantSamePrefix(t *testing.T) {
	input := []ma.Multiaddr{
		// 同一 IPv6 网卡的三个地址（主地址 + 两个隐私临时地址），同一监听端口。
		ma.StringCast("/ip6/2408:824e:1592:7d80::577/tcp/57714"),
		ma.StringCast("/ip6/2408:824e:1592:7d80:1076:c2a9:e3c6:e71d/tcp/57714"),
		ma.StringCast("/ip6/2408:824e:1592:7d80:7619:3a83:f518:d970/tcp/57714"),
		// 同一链路的另一种传输：保留。
		ma.StringCast("/ip6/2408:824e:1592:7d80::577/udp/55441/quic-v1"),
		// 不同链路：保留。
		ma.StringCast("/ip4/192.168.50.170/tcp/57712"),
		ma.StringCast("/ip4/10.70.38.92/tcp/57712"),
	}
	got := toStrings(CollapseRedundant(input))
	want := []string{
		"/ip6/2408:824e:1592:7d80::577/tcp/57714",
		"/ip6/2408:824e:1592:7d80::577/udp/55441/quic-v1",
		"/ip4/192.168.50.170/tcp/57712",
		"/ip4/10.70.38.92/tcp/57712",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	}
}

// TestCollapseRedundantLinkLocal /16 折叠：整块 169.254 都不可达，
// 没必要按 /24 留六条（真机实测该机就有六条 169.254 地址）。
func TestCollapseRedundantLinkLocal(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/ip4/169.254.122.160/tcp/57712"),
		ma.StringCast("/ip4/169.254.153.138/tcp/57712"),
		ma.StringCast("/ip4/169.254.177.82/tcp/57712"),
	}
	if got := CollapseRedundant(input); len(got) != 1 {
		t.Fatalf("链路本地应折叠成 1 条，got %v", toStrings(got))
	}
}

// TestCollapseRedundantKeepsUnclassifiable 无 IP 段的地址不参与折叠，一律保留。
func TestCollapseRedundantKeepsUnclassifiable(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/dns4/a.example.com/tcp/4001"),
		ma.StringCast("/dns4/b.example.com/tcp/4001"),
	}
	if got := CollapseRedundant(input); len(got) != 2 {
		t.Fatalf("无 IP 段应全部保留，got %v", toStrings(got))
	}
}

// TestPrefixKey 前缀粒度：IPv4 /24、IPv6 /64、IPv4 链路本地 /16。
func TestPrefixKey(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"192.168.50.170", "192.168.50.0/24"},
		{"192.168.50.171", "192.168.50.0/24"},
		{"192.168.51.170", "192.168.51.0/24"},
		{"2408:824e:1592:7d80::577", "2408:824e:1592:7d80::/64"},
		{"2408:824e:1592:7d80:1076:c2a9:e3c6:e71d", "2408:824e:1592:7d80::/64"},
		{"169.254.122.160", "169.254.0.0/16"},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("用例 IP 非法: %s", c.ip)
		}
		if got := PrefixKey(ip); got != c.want {
			t.Errorf("PrefixKey(%s) = %s, want %s", c.ip, got, c.want)
		}
	}
}

// TestShareableAddrsPipeline 组合入口：过滤 → 排序 → 折叠，一次到位。
func TestShareableAddrsPipeline(t *testing.T) {
	got := toStrings(ShareableAddrs([]ma.Multiaddr{
		ma.StringCast("/ip4/169.254.122.160/tcp/4001"),             // link-local → 最后
		ma.StringCast("/ip4/10.7.207.102/tcp/4001"),                // overlay → 剔除
		ma.StringCast("/ip6/2408:824e:1592:7d80:aaaa::1/tcp/4001"), // 公网 IPv6 → 最前
		ma.StringCast("/ip6/2408:824e:1592:7d80:bbbb::1/tcp/4001"), // 同前缀 → 折叠
		ma.StringCast("/ip4/192.168.50.170/tcp/4001"),              // 局域网
	}))
	want := []string{
		"/ip6/2408:824e:1592:7d80:aaaa::1/tcp/4001",
		"/ip4/192.168.50.170/tcp/4001",
		"/ip4/169.254.122.160/tcp/4001",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	}
}

// TestIfaceClassify 网卡分类：宿主内部虚拟交换机与 VPN 隧道分开判定，
// 物理网卡与未知名称的网卡一律不被降级（宁可多试，不可误降）。
func TestIfaceClassify(t *testing.T) {
	hasMAC := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}

	// 宿主内部虚拟交换机 / 容器网桥：只有同宿主机的虚拟机 / 容器可达。
	hostVirtual := []string{
		"vEthernet (WSL (Hyper-V firewall))",
		"vEthernet (Default Switch)",
		"VMware Network Adapter VMnet1",
		"VirtualBox Host-Only Network",
		"docker0",
		"br-1a2b3c4d",
		"virbr0",
		"veth9f8e7d",
	}
	for _, n := range hostVirtual {
		if !isHostVirtualIface(n) {
			t.Errorf("isHostVirtualIface(%q) 应为 true", n)
		}
		if isTunnelIface(n, hasMAC) {
			t.Errorf("isTunnelIface(%q, 有MAC) 应为 false（宿主虚拟交换机不是 VPN 隧道）", n)
		}
	}

	// 名字可识别的 VPN 隧道：即便有 MAC 也要认出来。
	tunnelsByName := []string{
		"ZeroTier One [76fc96e49865d436]",
		"Tailscale",
		"WireGuard Tunnel",
	}
	for _, n := range tunnelsByName {
		if !isTunnelIface(n, hasMAC) {
			t.Errorf("isTunnelIface(%q, 有MAC) 应为 true", n)
		}
		if isHostVirtualIface(n) {
			t.Errorf("isHostVirtualIface(%q) 应为 false", n)
		}
	}

	// 真机实测：NodeBabyLink Tunnel 名字里没有任何关键字，靠「空 MAC」认出——
	// 它曾凭 RFC1918 地址抢占连接码第一名额，把物理网卡挤下去。
	if !isTunnelIface("NodeBabyLink", nil) {
		t.Error("空 MAC 的网卡应判为隧道（真机 NodeBabyLink 即此情形）")
	}

	// 真实网卡（含名字像虚拟的）一律不降级。
	plain := []string{
		"以太网",
		"以太网 2",
		"WLAN",
		"无线局域网适配器 WLAN",
		"Ethernet",
		"eth0",
		"en0",
		"wlan0",
		"NodeBabyLink",    // 与上面同名的网卡，有 MAC 时按主力对待
		"Fortune Adapter", // 含 "tun" 子串但非隧道网卡（防短关键词误伤）
		"Bridgetown LAN",  // 含 "bridge" 邻域子串
	}
	for _, n := range plain {
		if isHostVirtualIface(n) {
			t.Errorf("isHostVirtualIface(%q) 应为 false（不应误降物理网卡）", n)
		}
		if isTunnelIface(n, hasMAC) {
			t.Errorf("isTunnelIface(%q, 有MAC) 应为 false（不应误降物理网卡）", n)
		}
	}
}

// TestHostVirtualIfaceKernelBridge 内核网桥确定性判据：自建网桥可以叫任何名字
// （br0、lan、bridge0…），按名字猜必然漏——真机实证某云服务器自建网桥上的
// 10.222.222.1 挤进连接码。isHostVirtualIface 在名字不命中时回退探测
// /sys/class/net/<名>/bridge（Linux 网桥必有该目录），本测试注入探测桩验证
// 回退逻辑本身，不依赖跑测试的机器是不是 Linux。
func TestHostVirtualIfaceKernelBridge(t *testing.T) {
	orig := bridgeIfaceProbe
	t.Cleanup(func() { bridgeIfaceProbe = orig })

	// 探测桩：只有名为 "lan" 与 "br0" 的网卡报「是内核网桥」。
	bridgeIfaceProbe = func(name string) bool { return name == "lan" || name == "br0" }

	// 名字不命中关键词、但探测报网桥 → 判为宿主虚拟网卡。
	for _, n := range []string{"lan", "br0"} {
		if !isHostVirtualIface(n) {
			t.Errorf("isHostVirtualIface(%q) 应为 true（内核网桥探测命中）", n)
		}
	}
	// 名字不命中、探测也不报 → 不降级（真实物理网卡）。
	for _, n := range []string{"eth0", "以太网", "wlan0"} {
		if isHostVirtualIface(n) {
			t.Errorf("isHostVirtualIface(%q) 应为 false（非网桥不误降）", n)
		}
	}
	// 名字命中关键词时无需探测即判 true（短路优先）。
	bridgeIfaceProbe = func(string) bool { return false }
	if !isHostVirtualIface("docker0") {
		t.Error("isHostVirtualIface(docker0) 应为 true（名字命中，不依赖探测）")
	}
}

// TestBridgeIfaceProbeDefault 默认探测实现在非 Linux 平台（/sys 路径不存在）
// 必须安全返回 false，且对空名 / 含路径分隔符的名字不 panic。
func TestBridgeIfaceProbeDefault(t *testing.T) {
	for _, n := range []string{"", "docker0", `..\evil`, `a/b`, "eth0"} {
		if bridgeIfaceProbe(n) {
			t.Logf("bridgeIfaceProbe(%q) 在本机返回 true（Linux 网桥机器上 eth0 之外的名字出现 true 才是异常）", n)
		}
	}
	// docker0 在真 Linux 网桥机器上应命中；其余平台 /sys 不存在应返回 false。
	// 两种结果都合法，这里只验证「不 panic、有确定返回」。
}

// TestRankOfIfaceClasses rankOf 在地址形态之上叠加网卡类别。
func TestRankOfIfaceClasses(t *testing.T) {
	sets := &ifaceAddrSets{
		primary:     map[string]bool{"192.168.50.170": true},
		tunnel:      map[string]bool{"10.70.38.92": true},
		hostVirtual: map[string]bool{"172.25.64.1": true},
	}
	cases := []struct {
		addr string
		want int
	}{
		// 主接口私有 IPv4 压过其它真实网卡的私有 IPv4。
		{"/ip4/192.168.50.170/tcp/4001", RankPrimaryIPv4},
		{"/ip4/10.1.2.3/tcp/4001", RankPrivateIPv4},
		// VPN 隧道与宿主虚拟交换机各自降级。
		{"/ip4/10.70.38.92/tcp/4001", RankTunnelIface},
		{"/ip4/172.25.64.1/tcp/4001", RankHostVirtualIface},
		// 主接口上的全局 IPv6 仍是最优先，不被网卡类别压下去。
		{"/ip6/2408:824e::1/tcp/4001", RankGlobalIPv6},
		// 链路本地永远最后，与网卡类别无关（隧道的 fe80:: 也一样不可达）。
		{"/ip6/fe80::1/tcp/4001", RankLinkLocal},
	}
	for _, c := range cases {
		if got := sets.rankOf(ma.StringCast(c.addr)); got != c.want {
			t.Errorf("rankOf(%s) = %d, want %d", c.addr, got, c.want)
		}
	}
	tunLinkLocal := &ifaceAddrSets{tunnel: map[string]bool{"fe80::1": true}}
	if got := tunLinkLocal.rankOf(ma.StringCast("/ip6/fe80::1/tcp/4001")); got != RankLinkLocal {
		t.Errorf("隧道网卡上的 link-local = %d, want %d", got, RankLinkLocal)
	}
}

// TestSortByReachabilityDemotesTunnelAndHostVirtual 真机场景复现。
//
// 该机同时挂着物理局域网（192.168.50.170）、名字认不出的隧道网卡
// （NodeBabyLink，10.222.222.1）、VPN 隧道（ZeroTier，10.70.38.92）与宿主
// 虚拟交换机（WSL，172.25.64.1）。修复前它们同处 RFC1918 一级、按 libp2p
// 给的随机顺序抢名额，物理网卡直接落榜；修复后物理网卡必须拿到第一名额。
func TestSortByReachabilityDemotesTunnelAndHostVirtual(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/ip4/10.222.222.1/tcp/57712"),    // NodeBabyLink 隧道
		ma.StringCast("/ip4/172.25.64.1/tcp/57712"),     // WSL 虚拟交换机
		ma.StringCast("/ip4/10.70.38.92/tcp/57712"),     // ZeroTier
		ma.StringCast("/ip4/192.168.50.170/tcp/57712"),  // 物理网卡（必须第一）
		ma.StringCast("/ip4/169.254.122.160/tcp/57712"), // 链路本地（最后）
	}
	sets := &ifaceAddrSets{
		primary:     map[string]bool{"192.168.50.170": true},
		tunnel:      map[string]bool{"10.222.222.1": true, "10.70.38.92": true},
		hostVirtual: map[string]bool{"172.25.64.1": true},
	}
	got := toStrings(sortByReachability(input, sets))
	want := []string{
		"/ip4/192.168.50.170/tcp/57712",
		"/ip4/10.222.222.1/tcp/57712",
		"/ip4/10.70.38.92/tcp/57712",
		"/ip4/172.25.64.1/tcp/57712",
		"/ip4/169.254.122.160/tcp/57712",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条不匹配：got %v\nwant %v", i, got, want)
		}
	}
	if !strings.HasPrefix(got[0], "/ip4/192.168.50.170") {
		t.Fatalf("物理网卡未拿到首个名额：%v", got)
	}
}

// TestLocalIfaceAddrSetsSmoke 真机枚举冒烟：分类表不得有同一 IP 落进两类
// （rankOf 的 switch 依赖互斥，重叠会让优先级静默失效）。
func TestLocalIfaceAddrSetsSmoke(t *testing.T) {
	sets := enumerateIfaceAddrSets()
	if sets == nil {
		t.Fatal("enumerateIfaceAddrSets 不应返回 nil")
	}
	for ip := range sets.primary {
		if sets.tunnel[ip] || sets.hostVirtual[ip] {
			t.Errorf("IP %s 同时落在 primary 与其它类别", ip)
		}
	}
	for ip := range sets.tunnel {
		if sets.hostVirtual[ip] {
			t.Errorf("IP %s 同时落在 tunnel 与 hostVirtual", ip)
		}
	}
	t.Logf("本机分类：primary=%d tunnel=%d hostVirtual=%d",
		len(sets.primary), len(sets.tunnel), len(sets.hostVirtual))
}
