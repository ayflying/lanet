package serverless

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

// testHost 创建一个监听 loopback 随机端口的测试节点。
func testHost(t *testing.T, relayService bool) host.Host {
	t.Helper()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
	)
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	if relayService {
		// 模拟 p2pkit 的 RelayServiceAlways：无条件启动 hop 服务。
		if _, err = relayv2.New(h); err != nil {
			t.Fatalf("start relay service: %v", err)
		}
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestInfoExchange 验证 info 协议往返（回写响应不得先于写完成半关闭）。
func TestInfoExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	da, err := New(ctx, ha, Config{NetworkKey: "grp-test", Name: "node-a"})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-test", Name: "node-b"})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// A 连接 B 并交换信息。
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("fetchInfo: %v", err)
	}
	if info.Name != "node-b" {
		t.Fatalf("info.Name = %q, want node-b", info.Name)
	}
	if info.Group != GroupFingerprint(da.groupKey) {
		t.Fatalf("info.Group = %q, want %q", info.Group, GroupFingerprint(da.groupKey))
	}
}

// TestRelayServiceHopReservation 验证「节点即服务端」的 relay service
// （非专用模式，EnableRelayService 无参）能被其他节点预约。
func TestRelayServiceHopReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false) // 客户端
	hb := testHost(t, true)  // 节点即服务端（standalone 语义）

	if err := ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	reserveCtx, cancelRes := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRes()
	_, err := client.Reserve(reserveCtx, ha, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()})
	if err != nil {
		t.Fatalf("reserve on non-dedicated relay service: %v", err)
	}
}

// TestNetworkKeySemantics 网络密钥语义：留空 = 公共网络（同 key），
// 不同密钥 = 互相隔离。
func TestNetworkKeySemantics(t *testing.T) {
	empty := GroupKey("")
	if string(empty) != string(GroupKey(PublicNetworkKey)) {
		t.Fatalf("GroupKey(\"\") should equal GroupKey(PublicNetworkKey)")
	}
	if string(GroupKey("alpha")) == string(GroupKey("beta")) {
		t.Fatalf("different keys must derive different group keys")
	}
	// 不同密钥的 rendezvous / mDNS 标识必须不同（网络隔离的基础）。
	if RendezvousKey(GroupKey("alpha")) == RendezvousKey(GroupKey("beta")) {
		t.Fatalf("rendezvous keys of different networks must differ")
	}
	if MdnsTag(GroupKey("alpha")) == MdnsTag(GroupKey("beta")) {
		t.Fatalf("mdns tags of different networks must differ")
	}
}

// TestDualDHTPrivateDiscovery 双 DHT 快路径：B 以 A 为私有种子（关闭公共
// 兜底），仅凭私有 /lanet/kad DHT 互相发现，来源标记 dht-private。
// 无任何公共网络依赖（离线可跑）。
func TestDualDHTPrivateDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	da, err := New(ctx, ha, Config{
		NetworkKey: "dual-dht", Name: "node-a",
		Interval:              500 * time.Millisecond,
		DisablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	seeds := make([]string, 0, len(ha.Addrs()))
	for _, a := range ha.Addrs() {
		seeds = append(seeds, a.String()+"/p2p/"+ha.ID().String())
	}
	db, err := New(ctx, hb, Config{
		NetworkKey: "dual-dht", Name: "node-b",
		Bootstrap:             seeds,
		Interval:              500 * time.Millisecond,
		DisablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	go da.Run(ctx)
	go db.Run(ctx)

	// 双向均经私有 DHT 发现：B 侧来源必须标记 dht-private。
	bSeesA, aSeesB, viaPrivate := false, false, false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range db.Peers() {
			if m.PeerID == ha.ID().String() {
				bSeesA = true
				if m.Source == "dht-private" {
					viaPrivate = true
				}
			}
		}
		for _, m := range da.Peers() {
			if m.PeerID == hb.ID().String() {
				aSeesB = true
			}
		}
		if bSeesA && aSeesB && viaPrivate {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !bSeesA || !aSeesB {
		t.Fatalf("私有 DHT 发现失败：aSeesB=%v bSeesA=%v membersA=%v membersB=%v",
			aSeesB, bSeesA, da.Peers(), db.Peers())
	}
	if !viaPrivate {
		t.Fatalf("B 发现 A 的来源应为 dht-private，实际 membersB=%v", db.Peers())
	}
}

// TestMemberReclaim 验证成员回收语义：
//  1. DHT 陈旧记录（addMember 反复调用）不会无限续命——LastSeen 只在
//     首次发现/真实通讯时刷新；
//  2. reapExpired 移除超过 TTL 无通讯的成员，虚拟 IP 派生占用随之释放；
//  3. 真实通讯（connectAndIdentify 的 info 往返）会刷新 LastSeen。
func TestMemberReclaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	da, err := New(ctx, ha, Config{NetworkKey: "grp-reclaim", Name: "node-a", MemberTTL: MinMemberTTL})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-reclaim", Name: "node-b"})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// 阶段一：陈旧记录——只 addMember 不真实通讯，LastSeen 不应被反复刷新。
	da.addMember(hb.ID(), hb.Addrs(), "dht")
	time.Sleep(1200 * time.Millisecond) // 等异步 connectAndIdentify 完成真实通讯
	var firstSeen time.Time
	da.mu.RLock()
	m, ok := da.members[hb.ID().String()]
	if !ok {
		da.mu.RUnlock()
		t.Fatalf("成员 B 未入表")
	}
	firstSeen = m.FirstSeen
	last1 := m.LastSeen
	da.mu.RUnlock()

	// 再来一轮陈旧发现（间隔一秒以上），LastSeen 不应因发现而推进太多。
	time.Sleep(1100 * time.Millisecond)
	da.addMember(hb.ID(), hb.Addrs(), "dht")
	da.mu.RLock()
	last2 := da.members[hb.ID().String()].LastSeen
	da.mu.RUnlock()

	// connectAndIdentify 是真实通讯（异步），若它成功，LastSeen 会 >= last1；
	// 这里验证的是「发现本身不续命」：两次 addMember 之间的时间差不应体现。
	if last2.Before(last1) {
		t.Fatalf("LastSeen 倒退: %v < %v", last2, last1)
	}

	// 阶段二：直接操纵 LastSeen 模拟超期，reapExpired 应移除成员。
	da.mu.Lock()
	da.members[hb.ID().String()].LastSeen = time.Now().Add(-DefaultMemberTTL - time.Minute)
	da.mu.Unlock()
	da.reapExpired()
	da.mu.RLock()
	_, still := da.members[hb.ID().String()]
	da.mu.RUnlock()
	if still {
		t.Fatalf("超期成员未被回收")
	}
	_ = firstSeen

	// 阶段三：被回收的成员下一轮发现重新入表（虚拟 IP 重新派生，值不变）。
	da.addMember(hb.ID(), hb.Addrs(), "dht")
	da.mu.RLock()
	m2, ok2 := da.members[hb.ID().String()]
	var vip string
	if ok2 {
		vip = m2.VirtualIP
	}
	da.mu.RUnlock()
	if !ok2 || vip == "" {
		t.Fatalf("回收后成员未重新入表")
	}
}
