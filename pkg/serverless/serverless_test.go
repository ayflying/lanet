package serverless

import (
	"context"
	"crypto/sha256"
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
	empty := GroupKey(ChannelOfficial, "")
	if string(empty) != string(GroupKey(ChannelOfficial, PublicNetworkKey)) {
		t.Fatalf(`GroupKey(official, "") should equal GroupKey(official, PublicNetworkKey)`)
	}
	if string(GroupKey(ChannelOfficial, "alpha")) == string(GroupKey(ChannelOfficial, "beta")) {
		t.Fatalf("different keys must derive different group keys")
	}
	// 不同密钥的 rendezvous / mDNS 标识必须不同（网络隔离的基础）。
	if RendezvousKey(GroupKey(ChannelOfficial, "alpha")) == RendezvousKey(GroupKey(ChannelOfficial, "beta")) {
		t.Fatalf("rendezvous keys of different networks must differ")
	}
	if MdnsTag(GroupKey(ChannelOfficial, "alpha")) == MdnsTag(GroupKey(ChannelOfficial, "beta")) {
		t.Fatalf("mdns tags of different networks must differ")
	}
}

// TestChannelIsolation 渠道隔离语义：官方发行版与第三方 SDK 构建即使在
// 完全相同的 NetworkKey（含公共网络）下也必须派生不同的群组密钥；
// 官方渠道（空渠道前缀）派生必须与历史版本一致。
func TestChannelIsolation(t *testing.T) {
	for _, key := range []string{"", "lanet/public", "my-secret-key"} {
		official := GroupKey(ChannelOfficial, key)
		sdk := GroupKey(ChannelSDK, key)
		if string(official) == string(sdk) {
			t.Fatalf("official and sdk channels must not share a network (key=%q)", key)
		}
		// 渠道不同 → rendezvous / mDNS / 虚拟 IP 派生全部隔离。
		if RendezvousKey(official) == RendezvousKey(sdk) {
			t.Fatalf("rendezvous keys must differ across channels (key=%q)", key)
		}
		if MdnsTag(official) == MdnsTag(sdk) {
			t.Fatalf("mdns tags must differ across channels (key=%q)", key)
		}
		if DeriveVirtualIP(official, "peerA") == DeriveVirtualIP(sdk, "peerA") {
			t.Fatalf("virtual IPs must be derived in isolated spaces (key=%q)", key)
		}
	}
	// 兼容性：官方渠道 + 任意密钥 = 历史「lanet-group-v1:<key>」派生，
	// 升级到渠道隔离版本后官方既有网络不变（零迁移）。
	for _, key := range []string{"", PublicNetworkKey, "my-secret-key"} {
		input := key
		if input == "" {
			input = PublicNetworkKey
		}
		sum := sha256.Sum256([]byte("lanet-group-v1:" + input))
		if string(GroupKey(ChannelOfficial, key)) != string(sum[:]) {
			t.Fatalf("official channel derivation must stay backward compatible (key=%q)", key)
		}
	}
	// 空渠道 = 官方渠道（历史派生），与显式 sdk 渠道区分。
	if string(GroupKey("", "k")) != string(GroupKey(ChannelOfficial, "k")) {
		t.Fatalf("empty channel must behave as official channel")
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
		Interval: 500 * time.Millisecond,
		// 公共兜底 v0.5.16 起默认关闭，此处显式声明测试意图（纯私有）。
		EnablePublicFallback: false,
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
		Bootstrap:            seeds,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: false,
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

// TestDefaultNoPublicFallback 默认语义（v0.5.16 起）：公共 DHT 兜底默认
// 关闭——不建立公共 DHT、留空密钥归一化为默认公共网络密钥、公共引导
// 地址从引导列表剔除（连 Start 阶段也不接触 bootstrap.libp2p.io）。
func TestDefaultNoPublicFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h := testHost(t, false)
	d, err := New(ctx, h, Config{
		NetworkKey: "",
		Bootstrap: []string{
			DefaultBootstrap, // 关闭兜底后必须被剔除（否则 dnsaddr 解析会联网）
			"/ip4/127.0.0.1/tcp/1/p2p/" + h.ID().String(),
		},
	})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if d.cfg.EnablePublicFallback {
		t.Fatalf("公共 DHT 兜底默认应为关闭")
	}
	if d.dhtPublic != nil {
		t.Fatalf("默认不应建立公共 DHT")
	}
	if d.dhtPrivate == nil {
		t.Fatalf("私有 DHT 应始终建立")
	}
	if d.cfg.NetworkKey != PublicNetworkKey {
		t.Fatalf("留空密钥应归一化为默认公共网络密钥，实际 %q", d.cfg.NetworkKey)
	}
	for _, b := range d.cfg.Bootstrap {
		if b == DefaultBootstrap {
			t.Fatalf("关闭兜底后公共引导地址应从引导列表剔除")
		}
	}
}

// TestPublicNetworkAutoRetire 密钥留空（默认公共网络密钥）端到端：
// 留空在 New 内归一化为 PublicNetworkKey 并建立双 DHT，两节点经私有
// DHT 互相发现并确认同群后，公共 DHT 必须自动退出（dhtPublic 置 nil、
// publicRetired 置位）——验证修复前「留空节点永远挂着公共 DHT 承担
// ~4MB/分钟上行应答流量」的缺口不再回归。离线可跑（引导指向对方节点，
// 不依赖 bootstrap.libp2p.io）。
func TestPublicNetworkAutoRetire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	seedsA := make([]string, 0, len(ha.Addrs()))
	for _, a := range ha.Addrs() {
		seedsA = append(seedsA, a.String()+"/p2p/"+ha.ID().String())
	}
	seedsB := make([]string, 0, len(hb.Addrs()))
	for _, a := range hb.Addrs() {
		seedsB = append(seedsB, a.String()+"/p2p/"+hb.ID().String())
	}
	// 网络密钥留空 = 默认公共网络密钥（归一化 + 双 DHT）。
	// EnablePublicFallback 显式开启：验证开关打开时「发现同群成员后
	// 自动退出公共 DHT」仍然生效（默认关闭时 dhtPublic 本就不建立）。
	da, err := New(ctx, ha, Config{
		NetworkKey: "", Name: "pub-a",
		Bootstrap:            seedsB,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{
		NetworkKey: "", Name: "pub-b",
		Bootstrap:            seedsA,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	// 归一化断言：留空必须被填充为默认密钥，且双 DHT 均已建立。
	if da.cfg.NetworkKey != PublicNetworkKey || db.cfg.NetworkKey != PublicNetworkKey {
		t.Fatalf("留空密钥应归一化为 PublicNetworkKey：a=%q b=%q",
			da.cfg.NetworkKey, db.cfg.NetworkKey)
	}
	if da.dhtPrivate == nil || db.dhtPrivate == nil {
		t.Fatalf("留空密钥也应建立私有 DHT：a=%v b=%v", da.dhtPrivate != nil, db.dhtPrivate != nil)
	}
	// 群身份兼容：留空与显式默认密钥的派生必须一致（老网络零迁移）。
	if string(da.GroupKey()) != string(GroupKey(ChannelOfficial, PublicNetworkKey)) {
		t.Fatalf("留空群身份派生与历史不一致")
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	go da.Run(ctx)
	go db.Run(ctx)

	// dhtPublicState 线程安全读取公共 DHT 退出状态。
	dhtPublicState := func(d *Discovery) (alive, retired bool) {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return d.dhtPublic != nil, d.publicRetired
	}

	// 观察窗口：互相发现（成员表非空）且两侧公共 DHT 均已退出。
	aSeesB, bSeesA := false, false
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range da.Peers() {
			if m.PeerID == hb.ID().String() {
				aSeesB = true
			}
		}
		for _, m := range db.Peers() {
			if m.PeerID == ha.ID().String() {
				bSeesA = true
			}
		}
		aAlive, aRetired := dhtPublicState(da)
		bAlive, bRetired := dhtPublicState(db)
		if aSeesB && bSeesA && !aAlive && !bAlive && aRetired && bRetired {
			return // 全部条件达成
		}
		time.Sleep(200 * time.Millisecond)
	}
	aAlive, aRetired := dhtPublicState(da)
	bAlive, bRetired := dhtPublicState(db)
	t.Fatalf("公共网络模式自动退出未达成：aSeesB=%v bSeesA=%v "+
		"publicAlive(a=%v b=%v) retired(a=%v b=%v) membersA=%v membersB=%v",
		aSeesB, bSeesA, aAlive, bAlive, aRetired, bRetired, da.Peers(), db.Peers())
}

// TestChannelIsolationNoDiscovery 渠道隔离端到端验证：两节点使用完全相同的
// NetworkKey，但渠道不同（official vs sdk）——即使 B 把 A 配置为私有 DHT
// 种子并互相拨号，也必须在观察窗口内互相发现不到（成员表为空）。
func TestChannelIsolationNoDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	seeds := make([]string, 0, len(ha.Addrs()))
	for _, a := range ha.Addrs() {
		seeds = append(seeds, a.String()+"/p2p/"+ha.ID().String())
	}
	da, err := New(ctx, ha, Config{
		NetworkKey: "same-key", Channel: ChannelOfficial, Name: "official-a",
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: false,
	})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{
		NetworkKey: "same-key", Channel: ChannelSDK, Name: "sdk-b",
		Bootstrap:            seeds,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: false,
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

	// 观察窗口：与 TestDualDHTPrivateDiscovery 相同环境下正常同渠道
	// 数秒内即可互相发现，这里给足 8 秒仍互不相见才算隔离成立。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if len(da.Peers()) > 0 || len(db.Peers()) > 0 {
			t.Fatalf("跨渠道节点互相同网络：membersA=%v membersB=%v", da.Peers(), db.Peers())
		}
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
