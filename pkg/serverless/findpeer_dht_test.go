package serverless

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peerstore"
	ma "github.com/multiformats/go-multiaddr"
)

// TestFindPeerInDHTBypassesStalePeerstore 连接码回退私有 DHT（0.5.75）所依赖的
// 关键契约：peerstore 里只剩失效地址时，普通按 ID 查找会被缓存地址遮住，
// FindPeerInDHT 必须绕开它、从私有 DHT 取回对端当前可用地址。
//
// 三节点拓扑还原真机场景（A 与 B 从未直连、B 的地址已经变了）：
//
//	B（目标，地址已变）←引导← C（持有 B 的新鲜地址）←引导← A（只有失效地址）
//
// A 只能通过 C 问出 B 的当前地址——这正是「连接码地址拨不通」时唯一的活路。
func TestFindPeerInDHTBypassesStalePeerstore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ha, hb, hc := testHost(t, false), testHost(t, false), testHost(t, false)

	seeds := func(h host.Host) []string {
		out := make([]string, 0, len(h.Addrs()))
		for _, a := range h.Addrs() {
			out = append(out, a.String()+"/p2p/"+h.ID().String())
		}
		return out
	}

	newDisc := func(h host.Host, name string, bootstrap []string) *Discovery {
		d, err := New(ctx, h, Config{
			NetworkKey:           "findpeer-dht",
			Name:                 name,
			Bootstrap:            bootstrap,
			Interval:             500 * time.Millisecond,
			EnablePublicFallback: false,
		})
		if err != nil {
			t.Fatalf("new discovery %s: %v", name, err)
		}
		if err := d.Start(ctx); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		go d.Run(ctx)
		return d
	}

	// C 从 B 引导（因此 C 持有 B 的当前地址）；A 从 C 引导（A 的路由表里只有 C）。
	newDisc(hb, "node-b", nil)
	newDisc(hc, "node-c", seeds(hb))
	da := newDisc(ha, "node-a", seeds(hc))

	// 等 C 真正与 B 建立私有 DHT 连接并记住其地址。
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && len(hc.Peerstore().Addrs(hb.ID())) == 0 {
		time.Sleep(200 * time.Millisecond)
	}
	if len(hc.Peerstore().Addrs(hb.ID())) == 0 {
		t.Fatalf("C 未取得 B 的地址，私有 DHT 种子链没建起来")
	}
	// A 必须始终不认识 B：否则本用例退化成「直拨本来就能成」。
	if addrs := ha.Peerstore().Addrs(hb.ID()); len(addrs) > 0 {
		t.Fatalf("A 不应事先知道 B 的地址，实际 %v", addrs)
	}

	// 复刻生产路径：同群成员确认后立即进私有 DHT 路由表
	// （serverless.go connectAndIdentifyContext 里的 TryAddPeer）。
	// 少了这一步路由表是空的，查询只会得到 "failed to find any peer in table"。
	privA := da.dhtPrivate.Load()
	if privA == nil {
		t.Fatal("A 的私有 DHT 未就绪")
	}
	if _, err := privA.RoutingTable().TryAddPeer(hc.ID(), false, false); err != nil {
		t.Fatalf("把 C 加入 A 的私有 DHT 路由表失败: %v", err)
	}

	// A 手里只有一条失效地址（等价于过期连接码刚被写进 peerstore）。
	// 用回环端口：testHost 只监听 127.0.0.1，而 p2pkit 的「可拨性」判据把
	// 回环/链路本地视为不可拨（对远端合理，见 isDialableUnderlay 注释），
	// 因此这里必须让失效地址与真实地址同属回环，才是在测 DHT 回退本身，
	// 而不是在测地址清洗的取舍。
	stale, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/9")
	if err != nil {
		t.Fatalf("构造失效地址失败: %v", err)
	}
	ha.Peerstore().AddAddrs(hb.ID(), []ma.Multiaddr{stale}, peerstore.PermanentAddrTTL)

	// 快速路径：被失效缓存地址遮住——这就是连接码直拨失败的成因。
	got, err := da.FindPeer(ctx, hb.ID().String())
	if err != nil {
		t.Fatalf("FindPeer 应命中 peerstore 缓存地址: %v", err)
	}
	if !hasAddr(got.Addrs, stale) {
		t.Fatalf("FindPeer 应返回失效缓存地址 %s，实际 %v", stale, got.Addrs)
	}
	if overlaps(got.Addrs, hb.Addrs()) {
		t.Fatalf("本用例前提是快速路径拿不到 B 的当前地址，实际 %v", got.Addrs)
	}

	// 回退路径：绕开 peerstore，从私有 DHT 拿到 B 的当前监听地址。
	//
	// 注意 kad-dht 的 FindPeer 返回的是「本地 peerstore 视图」（查询过程中
	// 会把对端报来的地址以 TempAddrTTL 写进 peerstore），所以结果里可能同时
	// 带上那条失效地址——契约只要求它必须包含 B 的当前地址，后续
	// RequestConnect 会拿整批地址去拨号，能拨通其中一条即成功。
	found, err := da.FindPeerInDHT(ctx, hb.ID().String())
	if err != nil {
		t.Fatalf("FindPeerInDHT 应从私有 DHT 找到 B（这是连接码回退的唯一活路）: %v", err)
	}
	if !overlaps(found.Addrs, hb.Addrs()) {
		t.Fatalf("FindPeerInDHT 应返回 B 的当前监听地址，实际 %v（B 监听 %v）", found.Addrs, hb.Addrs())
	}
}

// hasAddr 地址集合里是否有指定的一条。
func hasAddr(addrs []ma.Multiaddr, want ma.Multiaddr) bool {
	for _, a := range addrs {
		if a.String() == want.String() {
			return true
		}
	}
	return false
}

// overlaps 两个地址集合是否有交集（比较字符串形式，忽略顺序与去重差异）。
func overlaps(got, want []ma.Multiaddr) bool {
	for _, a := range got {
		if hasAddr(want, a) {
			return true
		}
	}
	return false
}
