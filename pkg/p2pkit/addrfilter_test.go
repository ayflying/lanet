package p2pkit

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	ma "github.com/multiformats/go-multiaddr"
)

func maList(t *testing.T, ss ...string) []ma.Multiaddr {
	t.Helper()
	out := make([]ma.Multiaddr, 0, len(ss))
	for _, s := range ss {
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			t.Fatalf("解析 multiaddr %q: %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

func maStrings(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 五类「结构上不可达」的地址必须被剔除：回环 / 链路本地 / 未指定 /
// lanet overlay / circuit 中继路径。它们全部来自真机实测的脏地址样本。
func TestFilterUnderlayAddrsDropsUnreachable(t *testing.T) {
	in := maList(t,
		"/ip4/127.0.0.1/tcp/52854",           // 回环
		"/ip6/::1/tcp/4001",                  // 回环 v6
		"/ip4/169.254.153.138/tcp/52854",     // 链路本地
		"/ip6/fe80::1/tcp/4001",              // 链路本地 v6
		"/ip4/0.0.0.0/tcp/4001",              // 未指定（监听通配）
		"/ip4/10.7.207.102/tcp/4001",         // lanet overlay
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",  // 中继路径
		"/ip4/192.168.50.170/tcp/52854",      // 私网：保留
		"/ip4/43.136.124.167/tcp/4001",       // 公网：保留
		"/ip4/192.168.50.170/udp/65067/quic-v1",
	)
	got := maStrings(FilterUnderlayAddrs(in))
	want := []string{
		"/ip4/192.168.50.170/tcp/52854",
		"/ip4/43.136.124.167/tcp/4001",
		"/ip4/192.168.50.170/udp/65067/quic-v1",
	}
	if !sameStrings(got, want) {
		t.Fatalf("FilterUnderlayAddrs = %v, want %v", got, want)
	}
}

// 保底语义：过滤后一条不剩时必须退回「非 overlay / 非 circuit」的那批。
// 典型是单测只监听 127.0.0.1、以及只能靠链路本地互通的特殊拓扑——此时
// 返回空会让 host 变成零地址，比保留弱地址更糟。
func TestFilterUnderlayAddrsKeepsWeakAddrWhenNothingElse(t *testing.T) {
	got := maStrings(FilterUnderlayAddrs(maList(t, "/ip4/169.254.1.2/tcp/4001")))
	if !sameStrings(got, []string{"/ip4/169.254.1.2/tcp/4001"}) {
		t.Fatalf("只剩链路本地时应保底保留，实际 %v", got)
	}
}

// overlay / circuit 永不复活：即使过滤后为空，也不退回它们。
func TestFilterUnderlayAddrsNeverRestoresOverlayOrCircuit(t *testing.T) {
	got := FilterUnderlayAddrs(maList(t,
		"/ip4/10.7.0.5/tcp/4001",
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",
	))
	if len(got) != 0 {
		t.Fatalf("overlay / circuit 不得复活，实际 %v", maStrings(got))
	}
}

// CleanUnderlayAddrs 要能把「一块网卡 × 多端口 × 多传输」压到上限内。
// 样本取自真机实测：单节点曾累积 110 条地址。
func TestCleanUnderlayAddrsCapsAddressCount(t *testing.T) {
	raw := []string{
		"/ip4/127.0.0.1/tcp/52854",
		"/ip4/169.254.153.138/tcp/52854",
		"/ip4/10.222.222.1/tcp/52854",
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",
	}
	// 同一网卡不同端口 / 传输各来几条（端口不同，CollapseRedundant 折叠不掉）
	for _, port := range []string{"52854", "54453", "54458", "65067", "64137", "56995"} {
		raw = append(raw,
			"/ip4/192.168.50.170/tcp/"+port,
			"/ip4/192.168.50.170/tcp/"+port+"/ws",
			"/ip4/192.168.50.170/udp/"+port+"/quic-v1",
		)
	}
	raw = append(raw, "/ip4/43.136.124.167/tcp/4001")

	got := CleanUnderlayAddrs(maList(t, raw...))
	if len(got) > maxUnderlayAddrsPerPeer {
		t.Fatalf("地址数 %d 超过上限 %d: %v", len(got), maxUnderlayAddrsPerPeer, maStrings(got))
	}
	for _, a := range got {
		if !isDialableUnderlay(a) {
			t.Fatalf("结果里仍有不可达地址: %s", a)
		}
	}
}

// 已被拨通过的地址不应被折叠/截断掉：它排在最前（SortByReachability 让
// 物理网卡私网优先，CollapseRedundant 保首条）。
func TestCleanUnderlayAddrsKeepsBestPerLink(t *testing.T) {
	got := maStrings(CleanUnderlayAddrs(maList(t,
		"/ip4/192.168.50.170/tcp/52854",
		"/ip4/192.168.50.171/tcp/52854", // 同 /24 同传输 → 折叠
	)))
	if !sameStrings(got, []string{"/ip4/192.168.50.170/tcp/52854"}) {
		t.Fatalf("同链路同传输应折叠为一条，实际 %v", got)
	}
}

// peerstore 的存量清理只针对「结构性有害」的地址（overlay / circuit）：
// 回环与链路本地必须留下——同机多实例、同一物理链路等特殊拓扑下它们可能是
// 唯一通路（早期用「不可达」判据清 peerstore 曾切断单测的 DHT 发现链）。
func TestPrunePeerstoreAddrs(t *testing.T) {
	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatalf("new peerstore: %v", err)
	}
	id, err := peer.Decode("12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf")
	if err != nil {
		t.Fatalf("decode peer id: %v", err)
	}
	ps.AddAddrs(id, maList(t,
		"/ip4/127.0.0.1/tcp/4001",           // 回环：保留
		"/ip4/169.254.1.1/tcp/4001",         // 链路本地：保留
		"/ip4/10.7.1.1/tcp/4001",            // overlay：清
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit", // circuit：清
		"/ip4/43.136.124.167/tcp/4001",
	), time.Hour)

	if n := PrunePeerstoreAddrs(ps, id); n != 2 {
		t.Fatalf("清理条数 = %d, want 2（仅 overlay 与 circuit）", n)
	}
	got := map[string]bool{}
	for _, a := range maStrings(ps.Addrs(id)) {
		got[a] = true
	}
	for _, want := range []string{
		"/ip4/127.0.0.1/tcp/4001",
		"/ip4/169.254.1.1/tcp/4001",
		"/ip4/43.136.124.167/tcp/4001",
	} {
		if !got[want] {
			t.Fatalf("应保留 %s，实际剩余 %v", want, maStrings(ps.Addrs(id)))
		}
	}
	for _, gone := range []string{
		"/ip4/10.7.1.1/tcp/4001",
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",
	} {
		if got[gone] {
			t.Fatalf("应清掉 %s，实际剩余 %v", gone, maStrings(ps.Addrs(id)))
		}
	}
	// 幂等：再清一次不应再删。
	if n := PrunePeerstoreAddrs(ps, id); n != 0 {
		t.Fatalf("重复清理应为 0，实际 %d", n)
	}
}

// CleanUnderlayAddrs 直接挂在 libp2p.AddrsFactory 上，**绝不能返回空**：返回空
// 等于 host 零地址，引导种子 / 连接码 / DHT provider 记录全部失效。而它内部
// 复用的 SortByReachability 会剔除回环地址，单测与同机多实例场景下必须保底。
// 回归背景：缺这段保底曾让 TestDualDHTPrivateDiscovery 的 seed 列表变空，
// 私有 DHT 起不来。
func TestCleanUnderlayAddrsNeverEmptyForLoopbackHost(t *testing.T) {
	got := CleanUnderlayAddrs(maList(t,
		"/ip4/127.0.0.1/tcp/4001",
		"/ip4/127.0.0.1/udp/4001/quic-v1",
	))
	if len(got) == 0 {
		t.Fatal("只剩回环地址时不得返回空")
	}
	for _, a := range got {
		if isCircuitAddr(a) || IsLanetOverlayAddr(a) {
			t.Fatalf("保底不得让 overlay / circuit 复活: %s", a)
		}
	}
}
