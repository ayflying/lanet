package lanet

import (
	"context"
	"strings"
	"testing"

	"github.com/ayflying/pvn/pkg/invitecode"
	"github.com/ayflying/pvn/pkg/p2pkit"
	ma "github.com/multiformats/go-multiaddr"
)

// 本机真实形态的地址样本：一块物理网卡监听 tcp+ws+quic，一块 VPN 网卡只有 quic，
// 一条公网 IPv6，另加一条 link-local 与 overlay（后两者应被排序层剔除）。
//
// ️ 这里刻意用 10.99.99.99 而不是本机 ZeroTier 真实的 10.70.38.92：分享层会
// 把「本机识别出来的隧道 / 宿主虚拟网卡地址」直接剔除（见
// p2pkit.DiscouragedShareAddrs），用真实地址会让断言随「跑测试的机器上有没有
// ZeroTier」而变。这个 IP 不属于本机任何网卡，故稳定地落在「普通私有 IPv4」，
// 只用于验证排序与链路折叠。隧道剔除语义由 p2pkit 的
// TestShareableAddrsDropsDiscouragedAddrs 单独覆盖。
func localAddrFixture() []ma.Multiaddr {
	return []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.50.170/tcp/49709"),
		ma.StringCast("/ip4/192.168.50.170/tcp/49710/ws"),
		ma.StringCast("/ip4/192.168.50.170/udp/53500/quic-v1"),
		ma.StringCast("/ip4/10.99.99.99/udp/52000/quic-v1"),
		ma.StringCast("/ip6/2408:824e:1592:7d80::577/tcp/4001"),
		ma.StringCast("/ip4/169.254.122.160/tcp/49709"),
		ma.StringCast("/ip4/10.7.207.102/tcp/49709"),
	}
}

// TestPickInviteHostPortsOrdersByReachability 连接码应把「公网 IPv6 → 局域网
// → VPN(私有 IPv4)」按可达性排好，每块网卡只出现一次。
func TestPickInviteHostPortsOrdersByReachability(t *testing.T) {
	got := pickInviteHostPorts(p2pkit.ShareableAddrs(localAddrFixture()))
	want := []string{
		"[2408:824e:1592:7d80::577]:4001", // 公网 IPv6 最优
		"192.168.50.170:49709",            // 局域网（取裸 TCP，跳过 ws 的 49710）
		"10.99.99.99:52000",               // VPN：无裸 TCP，退回 QUIC 端口
		"169.254.122.160:49709",           // link-local 垫底，但仍带上
	}
	if len(got) != len(want) {
		t.Fatalf("地址数不匹配：got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个地址不匹配：got %q want %q（全量 %v）", i, got[i], want[i], got)
		}
	}
}

// TestPickInviteHostPortsPrefersBareTCP 同一网卡同时有裸 TCP 与 QUIC 时取裸 TCP：
// 对端会据它自行展开 TCP+QUIC 两种传输，TCP 在国内网络更易通过。
func TestPickInviteHostPortsPrefersBareTCP(t *testing.T) {
	addrs := p2pkit.ShareableAddrs([]ma.Multiaddr{
		ma.StringCast("/ip4/192.168.50.170/udp/53500/quic-v1"),
		ma.StringCast("/ip4/192.168.50.170/tcp/49709"),
	})
	got := pickInviteHostPorts(addrs)
	if len(got) != 1 || got[0] != "192.168.50.170:49709" {
		t.Fatalf("应取裸 TCP 端口且只留一个：got %v", got)
	}
}

// TestPickInviteHostPortsSkipsNonDialablePorts ws / webrtc-direct 的端口属
// 另一套监听，用 IP:端口 拨不过去，必须跳过——否则对端白费一次拨号尝试。
func TestPickInviteHostPortsSkipsNonDialablePorts(t *testing.T) {
	addrs := []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.50.170/tcp/49710/ws"),
		ma.StringCast("/ip4/192.168.50.170/udp/101/webrtc-direct"),
	}
	if got := pickInviteHostPorts(addrs); len(got) != 0 {
		t.Fatalf("不可拨端口应全部跳过，got %v", got)
	}
}

// TestPickInviteHostPortsCaps 地址数受 invitecode.MaxAddrs 约束（连接码要能复制）。
func TestPickInviteHostPortsCaps(t *testing.T) {
	addrs := make([]ma.Multiaddr, 0, 8)
	for i := 0; i < 8; i++ {
		addrs = append(addrs, ma.StringCast(
			// 每块网卡一个不同 IP，均属私有段，排序后顺序即输入顺序。
			"/ip4/192.168.1."+string(rune('1'+i))+"/tcp/4001"))
	}
	got := pickInviteHostPorts(addrs)
	if len(got) != invitecode.MaxAddrs {
		t.Fatalf("应截断到 %d 个，got %d: %v", invitecode.MaxAddrs, len(got), got)
	}
}

// TestPickInviteHostPortsEmpty 只绑回环/overlay 时返回空（连接码退化为仅身份形式）。
func TestPickInviteHostPortsEmpty(t *testing.T) {
	addrs := p2pkit.ShareableAddrs([]ma.Multiaddr{
		ma.StringCast("/ip4/127.0.0.1/tcp/4001"),
		ma.StringCast("/ip4/10.7.207.102/tcp/4001"),
	})
	if got := pickInviteHostPorts(addrs); len(got) != 0 {
		t.Fatalf("应返回空，got %v", got)
	}
}

// TestInviteCodeRoundTripViaPick 端到端：挑出的多地址连成连接码后，
// 对端解析回来应得到对应网卡的 TCP 与 QUIC 地址，且数量正确。
func TestInviteCodeRoundTripViaPick(t *testing.T) {
	const pid = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
	ports := pickInviteHostPorts(p2pkit.ShareableAddrs(localAddrFixture()))
	code := invitecode.EncodeList(pid, ports)

	id, addrs, err := invitecode.ToMultiaddrs(code)
	if err != nil {
		t.Fatalf("连接码解析失败: %v", err)
	}
	if id.String() != pid {
		t.Fatalf("节点 ID 不匹配: %s", id)
	}
	if len(addrs) != len(ports)*2 {
		t.Fatalf("应展开为 %d 条 multiaddr（每地址 TCP+QUIC），got %d", len(ports)*2, len(addrs))
	}
	// 首个地址是公网 IPv6，应对应它的 TCP 与 QUIC 两种传输。
	if addrs[0].String() != "/ip6/2408:824e:1592:7d80::577/tcp/4001" {
		t.Fatalf("首条 multiaddr 不匹配: %s", addrs[0])
	}
	if addrs[1].String() != "/ip6/2408:824e:1592:7d80::577/udp/4001/quic-v1" {
		t.Fatalf("次条 multiaddr 不匹配: %s", addrs[1])
	}
	if !invitecode.IsInviteCode(code) {
		t.Fatal("生成的连接码应被识别为连接码")
	}
}

// TestInviteCodeCoversAllNICsNotPrivacyAddrs 回归测试（实测踩到的坑）：
// Windows 隐私扩展会让同一块 IPv6 网卡在同一前缀下有多个地址，若按 IP 去重
// 而不按链路前缀折叠，连接码的 4 个名额会被这一块网卡占满，局域网 IPv4 与
// VPN 网卡全都挤不进去——与「把所有网卡都分享出去」的目标正好相反。
//
// 同 localAddrFixture：第二块网卡用 10.99.99.99 而非本机真实的 ZeroTier
// 地址，避免断言随机器网卡枚举结果变化。
func TestInviteCodeCoversAllNICsNotPrivacyAddrs(t *testing.T) {
	// 同一 /64 前缀下的三个地址（主地址 + 两个隐私临时地址，真实形态）。
	const p = "2408:824e:1592:7d80"
	fixture := []ma.Multiaddr{
		ma.StringCast("/ip6/" + p + "::577/tcp/4001"),
		ma.StringCast("/ip6/" + p + ":1076:c2a9:e3c6:e71d/tcp/4001"),
		ma.StringCast("/ip6/" + p + ":7619:3a83:f518:d970/tcp/4001"),
		ma.StringCast("/ip4/192.168.50.170/tcp/49709"),
		ma.StringCast("/ip4/10.99.99.99/udp/52000/quic-v1"),
	}
	got := pickInviteHostPorts(p2pkit.ShareableAddrs(fixture))
	want := []string{
		"[" + p + "::577]:4001", // 该链路的代表（排序后首条）
		"192.168.50.170:49709",  // 局域网网卡
		"10.99.99.99:52000",     // 另一块网卡
	}
	if len(got) != len(want) {
		t.Fatalf("应覆盖 3 条不同链路，got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个不匹配：got %q want %q（全量 %v）", i, got[i], want[i], got)
		}
	}
}

// TestSystemShareHostPortsIgnoresCustomAdvertise 真 Host 回归（审计矩阵第 10 条）：
// custom 生效时 SystemShareHostPorts 必须仍返回**系统监听**地址，不能把自定义
// 地址当"系统默认"吐给"恢复默认"按钮。
//
// bug 形态：输入源若是 node.Addrs()（经 announceAddrs/AddrsFactory），custom
// 生效时它返回的就是自定义地址——再走 AutoShareableAddrs 也还原不出系统地址。
// 修复后输入源为 Network().ListenAddresses()（绕过 factory）。
func TestSystemShareHostPortsIgnoresCustomAdvertise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs: []string{"/ip4/0.0.0.0/tcp/0"},
	})
	if err != nil {
		t.Fatalf("起 host 失败: %v", err)
	}
	defer func() { _ = h.Close() }()

	p2pkit.SetAdvertiseSpec("1.2.3.4:4001")
	t.Cleanup(func() { p2pkit.SetAdvertiseSpec("") })

	c := &Client{node: h}
	got := c.SystemShareHostPorts()
	if len(got) == 0 {
		t.Fatal("应至少返回一条系统监听地址（0.0.0.0 应展开到真实网卡）")
	}
	for _, hp := range got {
		if strings.HasPrefix(hp, "1.2.3.4:") {
			t.Fatalf("自定义地址 %q 泄入「系统默认」——输入源必须绕过 announceAddrs: %v", hp, got)
		}
	}
	// 正向：应来自本机监听（非自定义的任何一条即可证明来源正确）。
	found := false
	for _, hp := range got {
		if !strings.HasPrefix(hp, "1.2.3.4:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("未找到任何系统监听地址: %v", got)
	}
}
