package lanet

import (
	"testing"

	"github.com/ayflying/pvn/pkg/peersdb"
	ma "github.com/multiformats/go-multiaddr"
)

// TestPickWarmupTargetsFiltersAndCaps 预热候选筛选：只保留「已信任 + 有历史
// 地址」，顺序沿用 ListPeers 的 last_seen DESC（输入顺序），并受 max 截断。
func TestPickWarmupTargetsFiltersAndCaps(t *testing.T) {
	peers := []peersdb.Peer{
		{PeerID: "peerA", Trusted: true, Name: "a"},
		{PeerID: "peerB", Trusted: false}, // 未信任：跳过（拨过去只给对方制造待审批噪音）
		{PeerID: "peerC", Trusted: true},  // 无历史地址：跳过（交给 DHT 发现）
		{PeerID: "", Trusted: true},       // 空 ID：跳过
		{PeerID: "peerD", Trusted: true},
		{PeerID: "peerE", Trusted: true},
	}
	addrs := map[string][]string{
		"peerA": {"/ip4/192.168.1.10/tcp/4001"},
		"peerB": {"/ip4/192.168.1.11/tcp/4001"},
		"peerD": {"/ip4/192.168.1.12/tcp/4001", "/ip4/192.168.1.13/tcp/4001"},
		"peerE": {"/ip4/192.168.1.14/tcp/4001"},
	}
	got := pickWarmupTargets(peers, func(id string) []string { return addrs[id] }, 2)
	if len(got) != 2 {
		t.Fatalf("应取 2 个候选，实际 %d 个: %+v", len(got), got)
	}
	if got[0].PeerID != "peerA" || got[1].PeerID != "peerD" {
		t.Fatalf("候选顺序/对象不符：%s, %s", got[0].PeerID, got[1].PeerID)
	}
	if len(got[1].Addrs) != 2 {
		t.Fatalf("peerD 应带上全部历史地址，实际 %d 条", len(got[1].Addrs))
	}
	if got[0].Name != "a" {
		t.Fatalf("候选应带上节点名便于日志辨认，实际 %q", got[0].Name)
	}
}

// TestPickWarmupTargetsGuards 参数退化：max<=0 或取地址函数缺失时一律不预热
// （避免误配后变成「全量重连整个地址簿」）。
func TestPickWarmupTargetsGuards(t *testing.T) {
	peers := []peersdb.Peer{{PeerID: "peerA", Trusted: true}}
	of := func(string) []string { return []string{"/ip4/192.168.1.10/tcp/4001"} }

	if got := pickWarmupTargets(peers, of, 0); got != nil {
		t.Fatalf("max=0 应不预热，实际 %+v", got)
	}
	if got := pickWarmupTargets(peers, of, -1); got != nil {
		t.Fatalf("max<0 应不预热，实际 %+v", got)
	}
	if got := pickWarmupTargets(peers, nil, 5); got != nil {
		t.Fatalf("addrsOf=nil 应不预热，实际 %+v", got)
	}
	// 有节点但都没地址：返回空（调用方据此不启动预热）。
	if got := pickWarmupTargets(peers, func(string) []string { return nil }, 5); len(got) != 0 {
		t.Fatalf("无地址时不应产出候选，实际 %+v", got)
	}
}

// TestStripPeerSuffix 去掉尾部 /p2p/<id> 只留传输地址本体（与地址簿既有条目
// 格式一致，避免同一地址两种写法各存一份）；只有 /p2p 段时不产出地址。
func TestStripPeerSuffix(t *testing.T) {
	// 用真实的 base58 节点 ID：StringCast 会校验 p2p 段，占位串会 panic。
	const pid = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
	cases := []struct{ in, want string }{
		{"/ip4/192.168.1.10/tcp/4001/p2p/" + pid, "/ip4/192.168.1.10/tcp/4001"},
		{"/ip4/192.168.1.10/tcp/4001", "/ip4/192.168.1.10/tcp/4001"},
		{"/ip6/2408:824e:1592:7d80::2b1/udp/4001/quic-v1/p2p/" + pid,
			"/ip6/2408:824e:1592:7d80::2b1/udp/4001/quic-v1"},
	}
	for _, c := range cases {
		got := stripPeerSuffix(ma.StringCast(c.in))
		if got == nil || got.String() != c.want {
			t.Fatalf("stripPeerSuffix(%s) = %v，期望 %s", c.in, got, c.want)
		}
	}
	if got := stripPeerSuffix(ma.StringCast("/p2p/" + pid)); got != nil {
		t.Fatalf("只剩 /p2p 段时应返回 nil（无可拨传输地址），实际 %v", got)
	}
}
