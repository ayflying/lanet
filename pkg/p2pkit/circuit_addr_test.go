package p2pkit

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	circuitTestRelay  = "12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf"
	circuitTestTarget = "12D3KooWEFxB66u9Je8EyFjjg4sYF3MzcGFpX9Y4NzAQKWVy2YdK"
	circuitTestOther  = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
)

func circuitTestIDs(t *testing.T) (peer.ID, peer.ID, peer.ID) {
	t.Helper()
	relay, err := peer.Decode(circuitTestRelay)
	if err != nil {
		t.Fatalf("decode relay: %v", err)
	}
	target, err := peer.Decode(circuitTestTarget)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	other, err := peer.Decode(circuitTestOther)
	if err != nil {
		t.Fatalf("decode other: %v", err)
	}
	return relay, target, other
}

// 只保留可用的 underlay 承载地址；circuit / overlay / 空地址一律跳过。
func TestCircuitAddrsSkipsUnusableUnderlay(t *testing.T) {
	relay, target, _ := circuitTestIDs(t)
	candidate := peer.AddrInfo{ID: relay, Addrs: []ma.Multiaddr{
		nil,
		ma.StringCast("/ip4/10.7.207.102/tcp/4001"),                                            // overlay：成环
		ma.StringCast("/ip4/43.136.124.167/udp/4001/quic-v1/p2p/" + circuitTestRelay + "/p2p-circuit"), // 已带 circuit：成环
		ma.StringCast("/ip4/43.136.124.167/udp/4001/quic-v1"),                                  // 唯一可用
	}}
	got := CircuitAddrs(candidate, target, 2)
	if len(got) != 1 {
		t.Fatalf("期望 1 条可用路径，实际 %d: %v", len(got), got)
	}
	want := "/ip4/43.136.124.167/udp/4001/quic-v1/p2p/" + circuitTestRelay +
		"/p2p-circuit/p2p/" + circuitTestTarget
	if got[0].String() != want {
		t.Fatalf("路径错误:\n got %s\nwant %s", got[0], want)
	}
}

// 候选地址自带 /p2p/<候选> 时要剥掉重复段，且指向别人的 /p2p 段必须跳过。
func TestCircuitAddrsNormalizesPeerSegment(t *testing.T) {
	relay, target, other := circuitTestIDs(t)
	candidate := peer.AddrInfo{ID: relay, Addrs: []ma.Multiaddr{
		ma.StringCast("/ip4/1.2.3.4/tcp/4001/p2p/" + circuitTestRelay), // 自带正确段 → 剥掉
		ma.StringCast("/ip4/5.6.7.8/tcp/4001/p2p/" + other.String()),   // 段指向别人 → 跳过
	}}
	got := CircuitAddrs(candidate, target, 2)
	if len(got) != 1 {
		t.Fatalf("期望 1 条路径，实际 %d: %v", len(got), got)
	}
	want := "/ip4/1.2.3.4/tcp/4001/p2p/" + circuitTestRelay + "/p2p-circuit/p2p/" + circuitTestTarget
	if got[0].String() != want {
		t.Fatalf("路径错误:\n got %s\nwant %s", got[0], want)
	}
}

// 上限生效：地址再多也只返回 maxAddrs 条。
func TestCircuitAddrsRespectsLimit(t *testing.T) {
	relay, target, _ := circuitTestIDs(t)
	addrs := make([]ma.Multiaddr, 0, 8)
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		addrs = append(addrs, ma.StringCast("/ip4/"+ip+"/tcp/4001"), ma.StringCast("/ip4/"+ip+"/udp/4001/quic-v1"))
	}
	got := CircuitAddrs(peer.AddrInfo{ID: relay, Addrs: addrs}, target, 2)
	if len(got) != 2 {
		t.Fatalf("上限未生效：得到 %d 条", len(got))
	}
}

// 拿目标自己当中继没有意义（成环），必须返回空；参数非法同样返回空。
func TestCircuitAddrsRejectsSelfAndEmptyInput(t *testing.T) {
	relay, target, _ := circuitTestIDs(t)
	if got := CircuitAddrs(peer.AddrInfo{ID: target, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/1.1.1.1/tcp/4001")}}, target, 2); len(got) != 0 {
		t.Fatalf("目标自己当中继应被拒绝: %v", got)
	}
	if got := CircuitAddrs(peer.AddrInfo{ID: relay}, target, 2); len(got) != 0 {
		t.Fatalf("空地址应返回空: %v", got)
	}
	if got := CircuitAddrs(peer.AddrInfo{ID: relay, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/1.1.1.1/tcp/4001")}}, target, 0); len(got) != 0 {
		t.Fatalf("maxAddrs=0 应返回空: %v", got)
	}
}
