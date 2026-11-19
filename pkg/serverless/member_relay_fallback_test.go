package serverless

import (
	"context"
	"strings"
	"testing"
	"time"

	lproto "github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// newMemberDialTestDiscovery 构造一个只装「成员建连 + info 握手」所需零件的
// Discovery：不启动调度器、不建 DHT，避免测试互相干扰。
func newMemberDialTestDiscovery(t *testing.T, h host.Host, key []byte, name string) *Discovery {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &Discovery{
		host: h, groupKey: key, serviceCtx: ctx, serviceCancel: cancel,
		memberTTL: time.Hour, members: make(map[string]*Member),
		pexLast: make(map[string]time.Time),
		vaddrs:  newVirtualIPTable(),
		cfg:     Config{Name: name, IsTrusted: func(string) bool { return true }},
	}
	d.protoInfo = lproto.GroupProtoID(lproto.BaseInfo, key)
	h.SetStreamHandler(d.protoInfo, d.gateControl("info", d.handleInfo))
	t.Cleanup(cancel)
	return d
}

// TestConnectMemberFallsBackToRelay 复现并验证本机实测到的缺陷：
// 两端直连地址全部失效（NAT 后、地址过期），目标只在公网中继上持有预约。
// 修复前：控制面只拨 peerstore 里的直连地址 → 建连失败 → 成员永远离线，
// 数据面的中继兜底也因为拿不到 netmap 路由而永远触发不了。
// 修复后：直连失败后现拼 /p2p-circuit 路径，经中继完成握手并进入成员表。
//
// 注意：测试节点都监听 127.0.0.1，而生产中继候选由 d.Candidates 提供、
// 会剔掉回环地址（relayUsableAddrs），因此这里直接把中继作为候选注入，
// 覆盖「兜底拨号 + 中继上握手」这段真正出问题的逻辑。
func TestConnectMemberFallsBackToRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	relayHost := testHost(t, true)
	targetHost := testHost(t, false)
	callerHost := testHost(t, false)

	// 目标节点在唯一可用中继上持有预约：standalone 节点的必备动作。
	if err := reserveRelayCandidates(ctx, targetHost, []peer.AddrInfo{
		{ID: relayHost.ID(), Addrs: relayHost.Addrs()},
	}); err != nil {
		t.Fatalf("目标预约中继失败: %v", err)
	}

	groupKey := GroupKey(ChannelSDK, "relay-fallback-test")
	_ = newMemberDialTestDiscovery(t, targetHost, groupKey, "relay-target")
	callerD := newMemberDialTestDiscovery(t, callerHost, groupKey, "relay-caller")

	// 模拟 NAT 后地址全失效：peerstore 里只留一条必然被拒的直连地址。
	callerHost.Peerstore().AddAddr(targetHost.ID(), ma.StringCast("/ip4/127.0.0.1/tcp/1"), time.Hour)

	// 复现缺陷本身：修复前的控制面只走这一条路（只拨 peerstore 地址），
	// 必然失败——这就是「对端一直离线、ping 不通虚拟 IP」的根因。
	if err := callerHost.Connect(ctx, peer.AddrInfo{ID: targetHost.ID()}); err == nil {
		t.Fatal("直连本应失败，测试前提不成立")
	}

	// 第一步：直连必然失败，改走中继兜底（生产里由 connectMember 自动切换）。
	candidates := []peer.AddrInfo{{ID: relayHost.ID(), Addrs: relayHost.Addrs()}}
	if err := callerD.connectMemberViaCandidates(ctx, targetHost.ID(), candidates); err != nil {
		t.Fatalf("中继兜底拨号失败: %v", err)
	}

	// 必须真的是中继路径，否则「兜底」可能悄悄退化成了直连。
	relayed := false
	for _, conn := range callerHost.Network().ConnsToPeer(targetHost.ID()) {
		if _, err := conn.RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT); err == nil {
			relayed = true
		}
	}
	if !relayed {
		t.Fatal("连接不是经中继建立的，兜底路径没生效")
	}

	// 第二步：在已建立的中继连接上完成成员信息交换（修复前这一步永远失败）。
	if err := callerD.connectAndIdentifyContext(ctx, targetHost.ID()); err != nil {
		t.Fatalf("中继连接上的成员信息交换失败: %v", err)
	}

	callerD.mu.RLock()
	m := callerD.members[targetHost.ID().String()]
	callerD.mu.RUnlock()
	if m == nil {
		t.Fatal("信息交换成功后成员没有进入成员表")
	}
	if m.Name != "relay-target" {
		t.Fatalf("成员名称未从握手结果更新: %q", m.Name)
	}

	// 二次调用不应重拨：已连（limited）直接复用连接握手。
	start := time.Now()
	if err := callerD.connectAndIdentifyContext(ctx, targetHost.ID()); err != nil {
		t.Fatalf("复用中继连接再次握手失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("二次握手异常缓慢（%s），疑似重复拨号", elapsed)
	}
}

// TestConnectMemberWithoutRelayCandidateStillFails 回归保护：
// 没有中继候选时必须如实失败，不能静默「假装连上」，并且错误里要看得出
// 「直连失败 → 中继兜底也失败」这个先后顺序。
func TestConnectMemberWithoutRelayCandidateStillFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	targetHost := testHost(t, false)
	callerHost := testHost(t, false)

	groupKey := GroupKey(ChannelSDK, "relay-fallback-none")
	_ = newMemberDialTestDiscovery(t, targetHost, groupKey, "unreachable-target")
	callerD := newMemberDialTestDiscovery(t, callerHost, groupKey, "caller")

	callerHost.Peerstore().AddAddr(targetHost.ID(), ma.StringCast("/ip4/127.0.0.1/tcp/1"), time.Hour)

	err := callerD.connectAndIdentifyContext(ctx, targetHost.ID())
	if err == nil {
		t.Fatal("没有中继候选时不应报告建连成功")
	}
	if !strings.Contains(err.Error(), "直连失败") || !strings.Contains(err.Error(), "中继兜底失败") {
		t.Fatalf("错误未体现实直连优先、中继兜底的两段语义: %v", err)
	}
	callerD.mu.RLock()
	_, exists := callerD.members[targetHost.ID().String()]
	callerD.mu.RUnlock()
	if exists {
		t.Fatal("建连失败时不应把对端写入成员表")
	}
}

// TestConnectMemberViaRelayWithoutCandidates 直接覆盖兜底函数本身：
// 候选为空时返回明确错误，而不是空指针或静默成功。
func TestConnectMemberViaRelayWithoutCandidates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h := testHost(t, false)
	d := newMemberDialTestDiscovery(t, h, GroupKey(ChannelSDK, "no-candidates"), "no-candidates")
	target, err := peer.Decode(testPeerB)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if err := d.connectMemberViaRelay(ctx, target); err == nil {
		t.Fatal("没有中继候选时应返回错误")
	}
}
