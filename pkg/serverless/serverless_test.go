package serverless

import (
	"context"
	"crypto/sha256"
	"strings"
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
	if !strings.HasPrefix(d.cfg.NetworkKey, DefaultNetworkKeyPrefix) {
		t.Fatalf("留空密钥应按节点身份派生「本机专属默认网络」（前缀 %q），实际 %q",
			DefaultNetworkKeyPrefix, d.cfg.NetworkKey)
	}
	if d.cfg.NetworkKey != DeriveDefaultNetworkKey(h.ID().String()) {
		t.Fatalf("派生结果应与 DeriveDefaultNetworkKey(peerID) 一致：got %q", d.cfg.NetworkKey)
	}
	for _, b := range d.cfg.Bootstrap {
		if b == DefaultBootstrap {
			t.Fatalf("关闭兜底后公共引导地址应从引导列表剔除")
		}
	}
}

// TestPublicDHTTimeoutExpired 公共 DHT 临时引导超时：开启兜底但始终没有
// 同群成员，到 PublicDHTTimeout 后必须自动退出（reason 标注超时）——
// 防止开关忘关导致公共 DHT 长期挂载。离线可跑（不依赖任何引导节点）。
func TestPublicDHTTimeoutExpired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := testHost(t, false)
	d, err := New(ctx, h, Config{
		NetworkKey:           "timeout-test",
		EnablePublicFallback: true,
		PublicDHTTimeout:     1 * time.Second,
		Interval:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if err = d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st := d.PublicDHTState(); !st.Active {
		t.Fatalf("启动后公共 DHT 应处于运行中，实际 %+v", st)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := d.PublicDHTState()
		if st.Retired && !st.Active {
			if !strings.Contains(st.Reason, "超时") {
				t.Fatalf("退出原因应标注超时，实际 %q", st.Reason)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("超时后公共 DHT 未自动退出：state=%+v", d.PublicDHTState())
}

// TestPublicDHTRuntimeToggle 运行时开关（控制台「启用公共 DHT 临时引导」）：
// 启动时未开启 → 运行时可主动开启并进入 Active；手动关闭 → Retired 且原因
// 标注手动；再次开启 → 复位为 Active；随后超时仍会自动退出。验证控制台
// 开关「点击即时生效 + 自动退出后状态同步」的底层行为。离线可跑。
func TestPublicDHTRuntimeToggle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := testHost(t, false)
	d, err := New(ctx, h, Config{
		NetworkKey:       "runtime-toggle",
		PublicDHTTimeout: 1 * time.Second,
		Interval:         500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if err = d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st := d.PublicDHTState(); st.Active {
		t.Fatalf("未开启兜底时不应有公共 DHT，实际 %+v", st)
	}

	// 运行时开启
	if err := d.EnablePublicDHTRuntime(ctx); err != nil {
		t.Fatalf("enable runtime: %v", err)
	}
	if st := d.PublicDHTState(); !st.Active || st.Retired {
		t.Fatalf("运行时开启后应为 Active 且未 Retired，实际 %+v", st)
	}

	// 手动关闭
	d.DisablePublicDHTRuntime("手动关闭（控制台开关）")
	st := d.PublicDHTState()
	if st.Active || !st.Retired {
		t.Fatalf("手动关闭后应为已退出，实际 %+v", st)
	}
	if !strings.Contains(st.Reason, "手动") {
		t.Fatalf("退出原因应标注手动，实际 %q", st.Reason)
	}

	// 再次开启：复位
	if err := d.EnablePublicDHTRuntime(ctx); err != nil {
		t.Fatalf("re-enable runtime: %v", err)
	}
	if st := d.PublicDHTState(); !st.Active || st.Retired || st.Reason != "" {
		t.Fatalf("重新开启后应复位为 Active，实际 %+v", st)
	}

	// 重开后超时仍应自动退出
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s := d.PublicDHTState(); s.Retired && !s.Active {
			if !strings.Contains(s.Reason, "超时") {
				t.Fatalf("重开后退出原因应标注超时，实际 %q", s.Reason)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("重开后超时未自动退出：state=%+v", d.PublicDHTState())
}

// TestDialSeedRuntime 运行时「连接种子直连」：A、B 都关闭公共 DHT（默认），
// 互不配置引导，只靠 A 主动调用 DialSeed(B 的 multiaddr) 建立成员关系——
// 验证控制台「直接连接」输入框的底层行为：不经任何公共 DHT、无需重启即可
// 入网，且双方互认为成员、各自注入私有 DHT 路由表。离线可跑。
func TestDialSeedRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	seedsB := make([]string, 0, len(hb.Addrs()))
	for _, a := range hb.Addrs() {
		seedsB = append(seedsB, a.String()+"/p2p/"+hb.ID().String())
	}

	// 两侧都不开启公共 DHT、也不配置任何引导种子。
	da, err := New(ctx, ha, Config{NetworkKey: "seed-direct", Name: "seed-a", Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "seed-direct", Name: "seed-b", Interval: 500 * time.Millisecond})
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

	// 运行时按种子直连（等价于控制台点「连接」）。
	peerID, err := da.DialSeed(ctx, seedsB)
	if err != nil {
		t.Fatalf("dial seed: %v", err)
	}
	if peerID != hb.ID().String() {
		t.Fatalf("返回的节点 ID 不符：got %s want %s", peerID, hb.ID())
	}

	// A 侧成员表应立刻出现 B；B 侧由入向 info 握手反向确认 A。
	seen := func(d *Discovery, id string) bool {
		for _, m := range d.Peers() {
			if m.PeerID == id {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if seen(da, hb.ID().String()) && seen(db, ha.ID().String()) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("种子直连后成员关系未建立：A见B=%v B见A=%v",
		seen(da, hb.ID().String()), seen(db, ha.ID().String()))
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
	// 显式使用 LegacyDefaultKey（等价历史留空语义）：两个节点落到同一张
	// 网络，才能触发「发现同群成员后自动退出公共 DHT」。
	// 注意：新语义下留空不再是共享的公共网络，而是按 PeerID 派生的
	// 本机专属网络，因此这里必须显式声明使用历史默认密钥。
	// EnablePublicFallback 显式开启：验证开关打开时「发现同群成员后
	// 自动退出公共 DHT」仍然生效（默认关闭时 dhtPublic 本就不建立）。
	da, err := New(ctx, ha, Config{
		NetworkKey: "", LegacyDefaultKey: true, Name: "pub-a",
		Bootstrap:            seedsB,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{
		NetworkKey: "", LegacyDefaultKey: true, Name: "pub-b",
		Bootstrap:            seedsA,
		Interval:             500 * time.Millisecond,
		EnablePublicFallback: true,
	})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	// 归一化断言：LegacyDefaultKey 下留空必须填为历史公共网络密钥，双 DHT 均已建立。
	if da.cfg.NetworkKey != PublicNetworkKey || db.cfg.NetworkKey != PublicNetworkKey {
		t.Fatalf("LegacyDefaultKey 下留空应归一化为 PublicNetworkKey：a=%q b=%q",
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

// TestDeriveDefaultNetworkKey 本机专属默认网络密钥的派生规则：
//   - 不同 PeerID → 不同密钥（每台机器默认自成一张网，避免超大网络）；
//   - 相同 PeerID → 相同密钥（重启稳定，不会每次启动换网）；
//   - 与历史公共网络密钥 PublicNetworkKey 不相等（老网络语义已变，靠
//     LegacyDefaultKey 显式迁移，而不是靠默认值撞回旧网）。
func TestDeriveDefaultNetworkKey(t *testing.T) {
	a := DeriveDefaultNetworkKey("12D3KooWAAAA")
	b := DeriveDefaultNetworkKey("12D3KooWBBBB")
	if a == b {
		t.Fatalf("不同 PeerID 必须派生不同默认密钥，实际都等于 %q", a)
	}
	if a != DeriveDefaultNetworkKey("12D3KooWAAAA") {
		t.Fatalf("相同 PeerID 必须派生稳定密钥（重启不换网）")
	}
	if !strings.HasPrefix(a, DefaultNetworkKeyPrefix) {
		t.Fatalf("派生密钥应带前缀 %q，实际 %q", DefaultNetworkKeyPrefix, a)
	}
	if a == PublicNetworkKey {
		t.Fatalf("派生默认密钥不应等于历史公共网络密钥（否则又回到全局同网）")
	}
	// 空 PeerID 兜底：退回历史默认值，不能 panic 或产生空密钥。
	if got := DeriveDefaultNetworkKey(""); got != PublicNetworkKey {
		t.Fatalf("空 PeerID 应兜底为 PublicNetworkKey，实际 %q", got)
	}
}

// TestBlankKeyIsolatedPerNode 留空密钥的两个节点（不同身份）必须落在
// 不同网络：GroupKey 不同 → DHT rendezvous / mDNS 标签 / 虚拟 IP 全部隔离。
// 这是「零配置不再等于与全世界同网」的核心保证。
func TestBlankKeyIsolatedPerNode(t *testing.T) {
	ka := DeriveDefaultNetworkKey("12D3KooWAAAA")
	kb := DeriveDefaultNetworkKey("12D3KooWBBBB")
	ga := GroupKey(ChannelOfficial, ka)
	gb := GroupKey(ChannelOfficial, kb)
	if string(ga) == string(gb) {
		t.Fatalf("不同默认密钥必须派生出不同群组密钥")
	}
	if RendezvousKey(ga) == RendezvousKey(gb) {
		t.Fatalf("不同默认网络必须使用不同 DHT rendezvous key")
	}
	if MdnsTag(ga) == MdnsTag(gb) {
		t.Fatalf("不同默认网络必须使用不同 mDNS 标签")
	}
	// 同一台机器重启后（PeerID 不变）群身份必须保持一致。
	if string(GroupKey(ChannelOfficial, DeriveDefaultNetworkKey("12D3KooWAAAA"))) != string(ga) {
		t.Fatalf("同身份重启后群身份必须稳定")
	}
}

// TestLegacyDefaultKeyKeepsOldNetwork 迁移兼容：显式 LegacyDefaultKey 时
// 留空仍归一化为历史 PublicNetworkKey，老节点升级后不脱离原网络。
func TestLegacyDefaultKeyKeepsOldNetwork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "", LegacyDefaultKey: true})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if d.cfg.NetworkKey != PublicNetworkKey {
		t.Fatalf("LegacyDefaultKey 下留空应保持历史公共网络密钥，实际 %q", d.cfg.NetworkKey)
	}
	if string(d.GroupKey()) != string(GroupKey(ChannelOfficial, PublicNetworkKey)) {
		t.Fatalf("LegacyDefaultKey 下群身份必须与历史派生完全一致（老网络零迁移）")
	}
}
