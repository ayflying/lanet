package lanet

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// newStandaloneClient 起一个本地 standalone 节点（回环监听，独立地址簿）。
func newStandaloneClient(t *testing.T, name, key, dbPath string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	c, err := New(ctx, Config{
		Name:         name,
		Standalone:   true,
		NetworkKey:   key,
		DBPath:       dbPath,
		ConsoleAddr:  "-", // 关掉控制台，避免测试环境端口冲突
		IdentityFile: filepath.Join(t.TempDir(), "node.key"),
		ListenAddrs: []string{
			"/ip4/127.0.0.1/tcp/0",
			"/ip4/127.0.0.1/udp/0/quic-v1",
		},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestConnectPeerByIDRequiresMutualApproval 验证「按节点 ID 连接」的完整闭环：
//  1. A 添加 B 的节点 ID → 本机立即信任 B（主动添加 = 我的同意），
//     但 B 还没同意 A → 返回 Pending（申请已送达）；
//  2. B 的控制台看到待审批 → 同意 → 双方连通，A 进入 B 的成员表；
//  3. 永久信任：地址簿里留下 B，后续连接走本地地址簿（零 DHT 查询）。
func TestConnectPeerByIDRequiresMutualApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()
	a := newStandaloneClient(t, "node-a", "grp-approval-e2e", filepath.Join(dir, "a.db"))
	b := newStandaloneClient(t, "node-b", "grp-approval-e2e", filepath.Join(dir, "b.db"))

	// 模拟「网络可达」：先建立传输层连接，让双方 peerstore 有对方地址。
	// 真实场景里这一步由 mDNS / 私有 DHT（同一网络密钥 + 种子节点）完成——
	// 测试环境没有种子节点，故显式建连替代。
	bID, err := peer.Decode(b.Info().PeerID)
	if err != nil {
		t.Fatalf("decode B peer id: %v", err)
	}
	if err = a.Host().Connect(ctx, peer.AddrInfo{ID: bID, Addrs: b.Host().Addrs()}); err != nil {
		t.Fatalf("建立传输层连接失败: %v", err)
	}

	// A 主动添加 B 的节点 ID。
	res, err := a.ConnectPeer(ctx, b.Info().PeerID)
	if err != nil {
		t.Fatalf("A 添加 B 节点 ID 失败: %v", err)
	}
	if res.AlreadyMember {
		t.Fatal("首次添加不能误报为重复成员")
	}
	if !res.Pending {
		t.Fatalf("B 尚未同意，A 应得到 Pending（申请已送达），实际 %+v", res)
	}
	repeated, repeatErr := a.ConnectPeer(ctx, b.Info().PeerID)
	if repeatErr != nil || repeated == nil || !repeated.AlreadyMember {
		t.Fatalf("再次添加应提示重复：结果=%+v，错误=%v", repeated, repeatErr)
	}
	if res.PeerID != b.Info().PeerID {
		t.Fatalf("Pending 结果应带回对端节点 ID，实际 %q", res.PeerID)
	}
	// A 这边已把 B 记为已信任（主动添加 = 我方同意）。
	if !a.isTrustedPeer(b.Info().PeerID) {
		t.Fatalf("A 主动添加后应在本机信任 B")
	}
	// B 那边把 A 记入待审批列表。
	waitFor(t, 10*time.Second, func() bool {
		for _, p := range b.PendingList() {
			if p.PeerID == a.Info().PeerID {
				return true
			}
		}
		return false
	}, "B 应把 A 记入待审批列表")

	// B 同意 A 的连接申请。
	if err = b.ApprovePeer(a.Info().PeerID); err != nil {
		t.Fatalf("B 同意 A 的连接申请失败: %v", err)
	}
	// 同意后待审批清空、信任建立。
	if n := len(b.PendingList()); n != 0 {
		t.Fatalf("同意后待审批列表应清空，实际剩 %d 条", n)
	}
	if !b.isTrustedPeer(a.Info().PeerID) {
		t.Fatalf("B 同意后应信任 A")
	}
	// 双方应最终连通：A 的成员表里出现 B（名称已由 info 交换补齐）。
	waitFor(t, 25*time.Second, func() bool {
		for _, m := range a.NetMap().Members {
			if m.PeerID == b.Info().PeerID && m.Name == "node-b" {
				return true
			}
		}
		return false
	}, "A 的成员表里应出现已互相同意的 B（含名称）")

	// 地址簿持久化：A 的信任列表含 B，且带可用地址。
	peers := a.TrustedPeers()
	found := false
	for _, p := range peers {
		if p.PeerID == b.Info().PeerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("A 的地址簿应持久化 B，实际 %+v", peers)
	}
}

// TestConnectPeerUnknownIDSearches 连接一个当前查不到的节点 ID：不应报
// 终局错误——DHT 发现需要时间（冷启动路由表未建立是常态），正确语义是
// 「已记录、查找中」：节点进地址簿（trusted + manual），返回 Searching=true，
// 由周期发现自动补连。用户不需要反复点击。
func TestConnectPeerUnknownIDSearches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-solo", "grp-unknown", filepath.Join(t.TempDir(), "solo.db"))

	// 只生成身份、不启动节点：关闭过的真实节点可能在 DHT/peerstore 留有可拨的
	// 旧地址，此时测试会进入「历史地址拨号失败」分支，而不是待测的「查不到地址」。
	ghostKey, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("生成不存在的节点身份: %v", err)
	}
	ghostPeer, err := peer.IDFromPrivateKey(ghostKey)
	if err != nil {
		t.Fatalf("生成节点 ID: %v", err)
	}
	ghostID := ghostPeer.String()

	res, err := a.ConnectPeer(ctx, ghostID)
	if err != nil {
		t.Fatalf("查不到地址不应报错（应转入后台查找），实际: %v", err)
	}
	if !res.Searching {
		t.Fatalf("应返回 Searching=true，实际 %+v", res)
	}
	if res.PeerID != ghostID {
		t.Fatalf("PeerID 应为 %s，实际 %s", ghostID, res.PeerID)
	}
	// 已记入地址簿并信任（对方上线后 addMember 过审批门直接建连）。
	if !a.hasKnownPeers() {
		t.Fatalf("查找中的节点应已记入地址簿（放开后续 DHT 查找流量门）")
	}
	if trusted, err := a.peers.IsTrusted(ctx, ghostID); err != nil || !trusted {
		t.Fatalf("用户主动添加 = 本机已同意，应标记 trusted（err=%v）", err)
	}
}

// TestConnectPeerRejectsGarbage 输入既不是节点 ID 也不是 multiaddr（如 IP、
// 乱码）时应立刻报错，不能卡住等待。
func TestConnectPeerRejectsGarbage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-x", "grp-garbage", filepath.Join(t.TempDir(), "x.db"))
	for _, bad := range []string{"", "  ", "192.168.1.1", "hello world", "12D3Koo"} {
		if _, err := a.ConnectPeer(ctx, bad); err == nil {
			t.Fatalf("非法输入 %q 应立刻报错", bad)
		}
	}
}

// TestEmptyAddressBookSkipsDHTSearch 流量控制：地址簿为空时不做主动 DHT 查找。
func TestEmptyAddressBookSkipsDHTSearch(t *testing.T) {
	a := newStandaloneClient(t, "node-flow", "grp-flow-e2e", filepath.Join(t.TempDir(), "f.db"))
	if a.hasKnownPeers() {
		t.Fatalf("新节点地址簿为空时 hasKnownPeers 应为 false（跳过主动 DHT 查找省流量）")
	}
}

// TestConnectPeerSelfRejected 输入本机节点 ID 应被立刻挡下。
func TestConnectPeerSelfRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-self", "grp-self", filepath.Join(t.TempDir(), "s.db"))
	if _, err := a.ConnectPeer(ctx, a.Info().PeerID); err == nil {
		t.Fatalf("连接本机节点 ID 应报错")
	}
}

func TestNearbyListRequiresFreshProbeAndPrefersReportedName(t *testing.T) {
	c := newStandaloneClient(t, "local", "nearby-list-filter", filepath.Join(t.TempDir(), "nearby.db"))
	ctx := context.Background()
	for _, n := range []peersdb.Nearby{
		{PeerID: "offline", Name: "旧名字", Source: "dht-private"},
		{PeerID: "online", Name: "历史名字", Source: "dht-private"},
		{PeerID: "old-version", Source: "dht-private"},
		{PeerID: c.peerID, Source: "dht-private"},
		{PeerID: "trusted", Source: "dht-private"},
	} {
		if err := c.peers.UpsertNearby(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.peers.SetTrusted(ctx, "trusted", true); err != nil {
		t.Fatal(err)
	}
	if err := c.peers.UpsertPeer(ctx, peersdb.Peer{PeerID: "online", Name: "本地旧名"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.peers.SetNotes(ctx, "online", "我的备注"); err != nil {
		t.Fatal(err)
	}
	c.nearbyMu.Lock()
	c.nearbyLive["offline"] = nearbyProbeState{name: "离线设备", expires: time.Now().Add(-time.Second)}
	c.nearbyLive["online"] = nearbyProbeState{name: "刚自报的名字", expires: time.Now().Add(time.Minute)}
	c.nearbyLive["trusted"] = nearbyProbeState{name: "好友", expires: time.Now().Add(time.Minute)}
	c.nearbyLive[c.peerID] = nearbyProbeState{name: "本机", expires: time.Now().Add(time.Minute)}
	c.nearbyMu.Unlock()
	list := c.NearbyList()
	if len(list) != 1 || list[0].PeerID != "online" || list[0].Name != "刚自报的名字" || list[0].Notes != "我的备注" {
		t.Fatalf("只展示已验证且未信任的在线设备，自报名优先，实际 %+v", list)
	}
	c.nearbyMu.Lock()
	state := c.nearbyLive["online"]
	state.expires = time.Now().Add(-time.Second)
	c.nearbyLive["online"] = state
	c.nearbyMu.Unlock()
	if list = c.NearbyList(); len(list) != 0 {
		t.Fatalf("TTL 到期应立即隐藏而不删除附近数据库行，实际 %+v", list)
	}
	if stored, err := c.peers.ListNearby(ctx); err != nil || len(stored) < 3 {
		t.Fatalf("离线节点仍应保留在数据库供下次重验: %+v, %v", stored, err)
	}
	c.nearbyMu.Lock()
	state = c.nearbyLive["online"]
	state.expires = time.Now().Add(time.Minute)
	state.name = ""
	c.nearbyLive["online"] = state
	c.nearbyMu.Unlock()
	if list = c.NearbyList(); len(list) != 1 || list[0].Name != "" || list[0].Notes != "我的备注" {
		t.Fatalf("空自报名不能回填历史名称，备注仍须保留: %+v", list)
	}
}

func TestNearbyProbeScheduleIsBoundedAndBacksOff(t *testing.T) {
	if nearbyProbeWorkers > 2 || nearbyProbeQueue > 12 || nearbyProbeSuccess < time.Minute {
		t.Fatalf("后台资源预算失效：workers=%d queue=%d success=%s", nearbyProbeWorkers, nearbyProbeQueue, nearbyProbeSuccess)
	}
	now := time.Now()
	state := nearbyProbeState{}
	for i := 0; i < 6; i++ {
		state = nearbyNextAttempt(now, state, errors.New("离线"))
		if state.nextAttempt.Sub(now) > nearbyProbeMaxDelay {
			t.Fatal("退避超过上限")
		}
		if i == 0 && state.nextAttempt.Sub(now) != nearbyProbeFailure {
			t.Fatal("首次失败应低频重试")
		}
	}
	if state.nextAttempt.Sub(now) != nearbyProbeMaxDelay {
		t.Fatalf("连续失败应达到最大退避，实际 %s", state.nextAttempt.Sub(now))
	}
	state = nearbyNextAttempt(now, state, serverless.ErrNearbyUnsupported)
	if state.nextAttempt.Sub(now) < time.Hour {
		t.Fatal("旧版本协议不支持应至少冷却一小时")
	}
	state = nearbyNextAttempt(now, state, nil)
	if state.failures != 0 || state.nextAttempt.Sub(now) < time.Minute {
		t.Fatalf("成功后应清除失败次数并延迟重验：%+v", state)
	}
}

func TestNearbyScanPrioritizesVerifiedAndNewCandidates(t *testing.T) {
	owner := newStandaloneClient(t, "观察端", "nearby-scan-priority", filepath.Join(t.TempDir(), "scan.db"))
	ctx := context.Background()
	addrs := []string{"/ip4/192.0.2.1/tcp/4001"}
	verified := "verified"
	if err := owner.peers.UpsertNearby(ctx, peersdb.Nearby{PeerID: verified, Addrs: addrs}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("stale-%02d", i)
		if err := owner.peers.UpsertNearby(ctx, peersdb.Nearby{PeerID: id, Addrs: addrs}); err != nil {
			t.Fatal(err)
		}
	}
	latest := "fresh-new"
	if err := owner.peers.UpsertNearby(ctx, peersdb.Nearby{PeerID: latest, Addrs: addrs}); err != nil {
		t.Fatal(err)
	}
	candidate := &Client{rootCtx: owner.rootCtx, peers: owner.peers, node: owner.node, peerID: owner.peerID, nearbyLive: map[string]nearbyProbeState{}}
	for i := 0; i < 20; i++ {
		candidate.nearbyLive[fmt.Sprintf("stale-%02d", i)] = nearbyProbeState{failures: 1}
	}
	candidate.nearbyLive[verified] = nearbyProbeState{name: "在线", expires: time.Now().Add(time.Minute)}
	// 即使旧失败行有 20 条，容量只有 2 的队列仍优先安排已在线重验和最新发现。
	jobs := make(chan peersdb.Nearby, 2)
	candidate.scanNearbyProbes(jobs)
	if len(jobs) != 2 {
		t.Fatalf("期望两个高优先级候选，实际 %d", len(jobs))
	}
	first, second := (<-jobs).PeerID, (<-jobs).PeerID
	if first != verified || second != latest {
		t.Fatalf("历史离线节点不应挤占在线和新发现：%q, %q", first, second)
	}
}

func TestNearbyProbeFailureHidesVerifiedDevice(t *testing.T) {
	a := newStandaloneClient(t, "观察端", "nearby-probe-failure", filepath.Join(t.TempDir(), "a.db"))
	// 无效 ID 可确定性触发探测失败，不依赖对端的网络关闭时序和冷却预算。
	n := peersdb.Nearby{PeerID: "invalid-peer-id", Source: "dht-private"}
	if err := a.peers.UpsertNearby(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	a.wakeNearbyProbes()
	waitFor(t, 3*time.Second, func() bool {
		a.nearbyMu.Lock()
		defer a.nearbyMu.Unlock()
		return a.nearbyLive[n.PeerID].noAddress
	}, "等待无地址候选被跳过")
	a.nearbyMu.Lock()
	a.nearbyLive[n.PeerID] = nearbyProbeState{name: "旧在线证据", expires: time.Now().Add(time.Minute), nextAttempt: time.Now().Add(time.Minute)}
	a.nearbyMu.Unlock()
	if got := a.NearbyList(); len(got) != 1 {
		t.Fatalf("测试预设的旧在线证据未生效: %+v", got)
	}
	a.probeNearby(n)
	if got := a.NearbyList(); len(got) != 0 {
		t.Fatalf("重验失败应立即隐藏旧在线结果: %+v", got)
	}
}

func TestNearbyProbeKeepsApprovalBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	a := newStandaloneClient(t, "观察端", "nearby-probe-approval", filepath.Join(dir, "a.db"))
	b := newStandaloneClient(t, "对端设备", "nearby-probe-approval", filepath.Join(dir, "b.db"))
	if err := a.Host().Connect(ctx, peer.AddrInfo{ID: b.Host().ID(), Addrs: b.Host().Addrs()}); err != nil {
		t.Fatalf("建立传输层连接失败: %v", err)
	}
	a.onSeenUntrusted(b.peerID, nil, "dht-private")
	waitFor(t, 12*time.Second, func() bool {
		for _, n := range a.NearbyList() {
			if n.PeerID == b.peerID && n.Name == "对端设备" {
				return true
			}
		}
		return false
	}, "未审批节点应可受限探测设备名")
	if trusted, err := a.peers.IsTrusted(ctx, b.peerID); err != nil || trusted {
		t.Fatalf("读取设备名不能自动信任节点: trusted=%v err=%v", trusted, err)
	}
	if len(a.PendingList()) != 0 || len(b.PendingList()) != 0 {
		t.Fatal("设备名探测不能创建待审批连接申请")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got := a.NearbyList(); len(got) != 0 {
		t.Fatalf("关闭后在线缓存必须清理: %+v", got)
	}
}

// waitFor 轮询等待条件成立，超时即失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, msg)
}
