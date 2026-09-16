package p2pkit

import (
	"net/netip"
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

// TestCgroupSaysContainer 覆盖各容器运行时的 cgroup 形态，以及裸机形态不被误判。
func TestCgroupSaysContainer(t *testing.T) {
	yes := []struct{ name, content string }{
		// cgroup v2（现代 docker）
		{"docker-v2", "0::/system.slice/docker-3f2a9c1b4e5d.scope\n"},
		// cgroup v1
		{"docker-v1", "12:pids:/docker/3f2a9c1b4e5d\n11:memory:/docker/3f2a9c1b4e5d\n"},
		{"kubepods", "11:memory:/kubepods/besteffort/pod1234/abc\n"},
		{"containerd", "0::/system.slice/containerd.service\n"},
		{"lxc", "10:cpu:/lxc/ubuntu-01\n"},
		{"podman", "0::/machine.slice/libpod-abc123.scope\n"},
		{"uppercase", "0::/SYSTEM.SLICE/DOCKER-ABC.SCOPE\n"},
	}
	for _, c := range yes {
		if !cgroupSaysContainer(c.content) {
			t.Errorf("cgroupSaysContainer(%s) 应为 true，内容=%q", c.name, c.content)
		}
	}

	no := []struct{ name, content string }{
		{"bare-systemd", "0::/init.scope\n"},
		{"bare-user", "1:name=systemd:/user.slice/user-1000.slice/session-1.scope\n"},
		{"empty", ""},
		// 关键防误伤：真机 cgroup 里可能出现 docker 字样仅因挂载了 docker.sock
		// 之类无关路径，这里断言的是「只有明确运行时路径才算」——
		// 命令名单独出现不算，故 /usr/bin/dockerd 这种不进判据。
		{"not-hint", "0::/system.slice/sshd.service\n"},
	}
	for _, c := range no {
		if cgroupSaysContainer(c.content) {
			t.Errorf("cgroupSaysContainer(%s) 应为 false，内容=%q", c.name, c.content)
		}
	}
}

// TestParseAdvertiseSpec 自声明地址解析：ip:port 展开两路、multiaddr 原样、非法项跳过。
func TestParseAdvertiseSpec(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := parseAdvertiseSpec(""); len(got) != 0 {
			t.Errorf("空配置应得 0 条，实际 %d：%v", len(got), got)
		}
		if got := parseAdvertiseSpec("  ,  , "); len(got) != 0 {
			t.Errorf("全空白应得 0 条，实际 %d：%v", len(got), got)
		}
	})

	t.Run("ip4-hostport", func(t *testing.T) {
		got := parseAdvertiseSpec("1.2.3.4:4001")
		want := []string{"/ip4/1.2.3.4/tcp/4001", "/ip4/1.2.3.4/udp/4001/quic-v1"}
		assertAddrStrings(t, got, want)
	})

	t.Run("ip6-hostport-bracket", func(t *testing.T) {
		got := parseAdvertiseSpec("[2408:824e::1]:4001")
		want := []string{"/ip6/2408:824e::1/tcp/4001", "/ip6/2408:824e::1/udp/4001/quic-v1"}
		assertAddrStrings(t, got, want)
	})

	t.Run("multiaddr-passthrough", func(t *testing.T) {
		got := parseAdvertiseSpec("/ip4/9.9.9.9/tcp/4001")
		assertAddrStrings(t, got, []string{"/ip4/9.9.9.9/tcp/4001"})
	})

	t.Run("invalid-skipped", func(t *testing.T) {
		// 一条写错不该让整个节点起不来：合法的照收，非法的静默跳过。
		got := parseAdvertiseSpec("nonsense, 1.2.3.4, /ip4/x/tcp/1, 5.6.7.8:4001")
		assertAddrStrings(t, got, []string{"/ip4/5.6.7.8/tcp/4001", "/ip4/5.6.7.8/udp/4001/quic-v1"})
	})

	t.Run("capped", func(t *testing.T) {
		// maxAdvertise 是硬上限：误配一长串不该把种子名额挤空。
		spec := "1.1.1.1:4001,1.1.1.2:4001,1.1.1.3:4001,1.1.1.4:4001,1.1.1.5:4001,1.1.1.6:4001,1.1.1.7:4001,1.1.1.8:4001"
		got := parseAdvertiseSpec(spec)
		if len(got) != maxAdvertise {
			t.Errorf("应被截到 %d 条，实际 %d", maxAdvertise, len(got))
		}
	})
}

// TestHostPortAddrs v4-mapped 归一到 v4，避免出现 /ip6/::ffff:1.2.3.4 这种怪地址。
func TestHostPortAddrs(t *testing.T) {
	cases := []struct{ in, wantProto, wantIP string }{
		{"1.2.3.4", "ip4", "1.2.3.4"},
		{"::ffff:1.2.3.4", "ip4", "1.2.3.4"},
		{"2408:824e::1", "ip6", "2408:824e::1"},
	}
	for _, c := range cases {
		ip := mustAddr(t, c.in)
		got := hostPortAddrs(ip, "4001")
		if len(got) != 2 {
			t.Fatalf("%s 应派生 2 条地址，实际 %d：%v", c.in, len(got), got)
		}
		want := "/" + c.wantProto + "/" + c.wantIP + "/tcp/4001"
		if got[0].String() != want {
			t.Errorf("%s → 首条应为 %s，实际 %s", c.in, want, got[0].String())
		}
	}
}

// TestDropContainerInternalAddrs 本机在容器里时，容器内网地址必须被**剔除**
// （而不是仅降级——降级过的地址名额有余量时照样扩散）。
//
// 这是 VPS stack 61 的真机症状：容器把自己的 docker 内网地址 192.168.64.2
// 当成对外地址发布，同群节点反复拨它空耗超时。
func TestDropContainerInternalAddrs(t *testing.T) {
	dockerInternal := map[string]bool{
		"192.168.64.2": true,
		"172.17.0.2":   true,
	}
	addrs := []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.64.2/tcp/4001"),
		ma.StringCast("/ip4/192.168.64.2/udp/4001/quic-v1"),
		ma.StringCast("/ip4/172.17.0.2/tcp/4001"),
		ma.StringCast("/ip6/2408:824e::99/tcp/4001"), // 容器上的公网 IPv6：真可路由，保留
		ma.StringCast("/ip4/1.2.3.4/tcp/4001"),       // 公网 IPv4：保留
	}
	got := DropAddrSet(addrs, dockerInternal)
	want := []string{"/ip6/2408:824e::99/tcp/4001", "/ip4/1.2.3.4/tcp/4001"}
	assertAddrStrings(t, got, want)

	// 空集合（裸机）不动任何东西。
	if out := DropAddrSet(addrs, nil); len(out) != len(addrs) {
		t.Errorf("空剔除集合不应改动列表：%d → %d", len(addrs), len(out))
	}
}

// TestShareableAddrsInContainer 端到端串一遍：剔除容器内网地址 + 自声明地址置顶。
func TestShareableAddrsInContainer(t *testing.T) {
	addrs := []ma.Multiaddr{
		ma.StringCast("/ip4/192.168.64.2/tcp/4001"),
		ma.StringCast("/ip6/2408:824e::99/tcp/4001"),
	}
	adv := []ma.Multiaddr{ma.StringCast("/ip4/43.136.124.167/tcp/4001")}
	// 容器内被剔除的集合（等价于 collectContainerInternalAddrs 在 VPS 容器上的产出）。
	dockerInternal := map[string]bool{"192.168.64.2": true}

	// 裸机：docker 内网地址仍会按 RFC1918 被分享出去（历史行为）。
	bare := shareableAddrsWith(addrs, nil, nil)
	if len(bare) != 2 {
		t.Fatalf("裸机应保留 2 条，实际 %d：%v", len(bare), bare)
	}

	// 容器：docker 内网地址被剔除，只剩公网 IPv6。
	inC := shareableAddrsWith(addrs, dockerInternal, nil)
	assertAddrStrings(t, inC, []string{"/ip6/2408:824e::99/tcp/4001"})

	// 容器 + 自声明：显式声明的对外地址顶到最前。
	withAdv := shareableAddrsWith(addrs, dockerInternal, adv)
	assertAddrStrings(t, withAdv, []string{"/ip4/43.136.124.167/tcp/4001", "/ip6/2408:824e::99/tcp/4001"})
}

// TestMergeAddrsFirst 去重且保序，first 段整体优先。
func TestMergeAddrsFirst(t *testing.T) {
	first := []ma.Multiaddr{ma.StringCast("/ip4/1.1.1.1/tcp/4001")}
	base := []ma.Multiaddr{
		ma.StringCast("/ip4/1.1.1.1/tcp/4001"), // 与 first 重复
		ma.StringCast("/ip4/2.2.2.2/tcp/4001"),
	}
	got := MergeAddrsFirst(first, base)
	assertAddrStrings(t, got, []string{"/ip4/1.1.1.1/tcp/4001", "/ip4/2.2.2.2/tcp/4001"})

	if out := MergeAddrsFirst(nil, base); len(out) != len(base) {
		t.Errorf("first 为空时应原样返回 base")
	}
}

// --- 测试助手 ---

func assertAddrStrings(t *testing.T, got []ma.Multiaddr, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("条数不符：want %d %v，got %d %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("第 %d 条：want %s，got %s", i, want[i], got[i].String())
		}
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("解析 %q 失败：%v", s, err)
	}
	return a
}
