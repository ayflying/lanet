package serverless

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// ---- 双向删除好友（unfriend 协议） ----

// TestUnfriendNotifyOnline 对端在线时：A 主动 NotifyUnfriend(B)，B 应
// 立即把 A 移出成员表、回调 OnUnfriendReceived，并断开与 A 的连接。
// 这是「删除即双向」的即时路径。
func TestUnfriendNotifyOnline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	// 双方互信任（模拟已是好友）。
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-uf",
		Name:       "node-a",
		IsTrusted:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	var mu sync.Mutex
	var unfriended []string
	// B 侧对 A 的信任状态：初始互信任（模拟已是好友），收到删除通知后立刻
	// 不再信任——与生产一致（上层 OnUnfriendReceived 会把对端移出地址簿，
	// 此后 IsTrusted 对该对端返回 false）。
	//
	// 不补这一步测试就是**竞态**的：B 删除 A 的同时，双方的信息握手可能还在
	// 往返，晚到的那次握手会再次把 A 写回成员表，于是断言时而通过时而失败
	// （实测在整包运行时概率性失败、单跑必过）。生产代码没这个问题，
	// 因为那次握手会被 IsTrusted=false 挡在待审批之外、不会 addMember。
	trustA := true
	db, err := New(ctx, hb, Config{
		NetworkKey: "grp-uf",
		Name:       "node-b",
		IsTrusted: func(peerID string) bool {
			mu.Lock()
			defer mu.Unlock()
			if peerID == ha.ID().String() {
				return trustA
			}
			return true
		},
		OnUnfriendReceived: func(peerID string) {
			mu.Lock()
			if peerID == ha.ID().String() {
				trustA = false
			}
			unfriended = append(unfriended, peerID)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// 建连并把 B 放进 A 的成员表（模拟好友在线）。
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	da.addMember(hb.ID(), hb.Addrs(), "test")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := memberOf(da, hb.ID().String()); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := memberOf(db, ha.ID().String()); !ok {
		// B 侧因 A 主动握手也会把 A 加进成员表。
		db.addMember(ha.ID(), ha.Addrs(), "test")
	}

	// A 删除 B：在线即时通知。
	if err = da.NotifyUnfriend(ctx, hb.ID().String()); err != nil {
		t.Fatalf("notify unfriend: %v", err)
	}

	// B 应收到通知、把 A 移出成员表、触发回调。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(unfriended)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	received := append([]string(nil), unfriended...)
	mu.Unlock()
	if len(received) == 0 || received[0] != ha.ID().String() {
		t.Fatalf("B 应收到删除好友通知并回调本机 ID，实际 %v", received)
	}
	// 轮询期间不能持有信任回调的锁，否则在途握手无法重新检查撤销状态。
	// 等移除生效；再留一小段时间确认「在途握手」不会把 A 写回来（见上面
	// trustA 的注释）。写成有界轮询而不是一次性断言，避免依赖调度时序。
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := memberOf(db, ha.ID().String()); !ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := memberOf(db, ha.ID().String()); ok {
		t.Fatalf("B 收到通知后应立即把 A 移出成员表")
	}
}

// TestUnfriendOfflineSelfHeal 对端离线自愈路径：A 删了 B（持墓碑），
// B 之后主动来握手，应收到 rejection=unfriended；B 据此回调把 A 从自己
// 列表删除。验证「删除发生时对方离线，下次通讯仍能双向收敛」。
func TestUnfriendOfflineSelfHeal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	// A 视 B 为已删除（墓碑），且不信任 B；B 仍信任 A（模拟 A 删了 B 但
	// B 还不知道）。B 主动连 A 握手。
	var cleared []string
	da, err := New(ctx, ha, Config{
		NetworkKey:   "grp-uf-off",
		Name:         "node-a",
		IsTrusted:    func(string) bool { return false },
		OnPending:    func(string, []string, string) { /* 墓碑命中前不该到这里 */ },
		IsUnfriended: func(string) bool { return true }, // A 删过 B
		ClearUnfriended: func(peerID string) {
			cleared = append(cleared, peerID)
		},
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	// B 无审批限制，主动 fetchInfo(A) 模拟握手。
	hbD, err := New(ctx, hb, Config{NetworkKey: "grp-uf-off", Name: "node-b"})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = hbD.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// B 主动出向握手：应拿到 unfriended 拒绝标记（不是普通的 not_approved）。
	resp, err := hbD.fetchInfo(ctx, ha.ID())
	if err != nil {
		t.Fatalf("B 握手应收到明确拒绝响应，实际错误: %v", err)
	}
	if resp.Rejected != rejectionUnfriended {
		t.Fatalf("A 有墓碑时 B 握手应收到 unfriended 拒绝，实际 %q", resp.Rejected)
	}
	// 拒绝响应不得泄漏身份信息。
	if resp.Name != "" || resp.Version != "" || resp.OSHostname != "" || len(resp.LocalIPs) > 0 {
		t.Fatalf("unfriended 拒绝响应泄漏了身份信息: %+v", resp)
	}
}

// TestNearbyReportOnPassiveDiscover 被动发现未信任节点时应触发
// OnSeenUntrusted 上报（附近列表的来源），且不进成员表。
// 同时断言**不得**产生待审批：被动发现 ≠ 对方申请连接。
func TestNearbyReportOnPassiveDiscover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	var mu sync.Mutex
	var seen []string
	var pending []string
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-nearby",
		Name:       "node-a",
		IsTrusted:  func(string) bool { return false }, // 谁都不信任
		OnPending: func(peerID string, _ []string, _ string) {
			mu.Lock()
			pending = append(pending, peerID)
			mu.Unlock()
		},
		OnSeenUntrusted: func(peerID string, _ []string, _ string) {
			mu.Lock()
			seen = append(seen, peerID)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 直接走发现路径：A 收到 B 的 provider 记录（addMember 未信任分支）。
	da.addMember(hb.ID(), hb.Addrs(), "dht-private")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != hb.ID().String() {
		t.Fatalf("被动发现未信任节点应上报附近，实际 %v", seen)
	}
	if len(pending) != 0 {
		t.Fatalf("被动发现不得上报待审批（发现≠申请），实际 %v", pending)
	}
	if _, ok := memberOf(da, hb.ID().String()); ok {
		t.Fatalf("未信任节点不得进入成员表")
	}
}

// TestPassiveDiscoverRepeatedStaysQuiet 被动发现是周期性的：同一节点被反复
// 发现（DHT 每轮 provider 查询）时，既不该重复堆「附近」，更不该每次刷一条
// 假的「连接申请」——这正是 0.5.40 及以前控制台待审批被刷屏、日志每轮
// 一条「收到陌生节点…的连接申请」的成因。
func TestPassiveDiscoverRepeatedStaysQuiet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	var mu sync.Mutex
	var pendingCount, nearbyCount int
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-nearby-rep",
		IsTrusted:  func(string) bool { return false },
		OnPending: func(string, []string, string) {
			mu.Lock()
			pendingCount++
			mu.Unlock()
		},
		OnSeenUntrusted: func(string, []string, string) {
			mu.Lock()
			nearbyCount++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 5; i++ {
		da.addMember(hb.ID(), hb.Addrs(), "dht-private")
	}
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("重复被动发现不得产生任何待审批，实际 %d 条", pendingCount)
	}
	if nearbyCount == 0 {
		t.Fatalf("重复被动发现仍应上报附近（至少一次）")
	}
}

// TestUnfriendGuardBlocksRevival 复活防护的确定性单测：unfriend 通知处理
// 后的防护期内，该节点一律不得重新进入成员表——无论握手/发现的信任检查
// 何时通过（OnUnfriendReceived 是异步回调，IsTrusted 翻转可能滞后，任何
// 「检查时刻还信任」的判断都不可靠）。放行只能走显式恢复路径
// （RequestConnect 清除防护记录，对应「用户重新加好友」）。直接验证
// guard 状态机，不依赖网络时序——集成路径由 TestUnfriendNotifyOnline
// 的概率性失败暴露。
func TestUnfriendGuardBlocksRevival(t *testing.T) {
	d := &Discovery{
		members:      map[string]*Member{},
		unfriendedAt: map[string]time.Time{},
	}
	// 删除好友：与生产同一临界区语义（先删成员、再记防护）。
	d.mu.Lock()
	delete(d.members, "p1")
	d.markUnfriendedLocked("p1")
	d.mu.Unlock()

	if !d.unfriendGuardBlocked("p1") {
		t.Fatal("防护期内应被复活防护拦截")
	}
	// 防护期已过：放行（自愈路径）。
	d.mu.Lock()
	d.unfriendedAt["p2"] = time.Now().Add(-unfriendGuardTTL - time.Second)
	d.mu.Unlock()
	if d.unfriendGuardBlocked("p2") {
		t.Fatal("防护期已过期的节点不应再被拦截")
	}
	// 显式恢复路径：RequestConnect 清除防护记录（锁内 delete，同生产实现）。
	d.mu.Lock()
	delete(d.unfriendedAt, "p1")
	d.mu.Unlock()
	if d.unfriendGuardBlocked("p1") {
		t.Fatal("清除防护记录后应放行（重新加好友）")
	}
	if d.unfriendGuardBlocked("p3") {
		t.Fatal("无删除记录的节点不应被拦截")
	}
}

// TestHandleUnfriendMarksGuard 锁死 handleUnfriend 的临界区语义：收到
// 删除好友通知时，成员表移除与复活防护记录必须在同一临界区内完成。
// 漏记防护正是 TestUnfriendNotifyOnline 在 CI 上概率性失败的根因：
// OnUnfriendReceived 异步回调翻转信任之前，对端的在途握手通过信任检查、
// 且 guard 无记录可拦，刚删除的成员被写回成员表（复活，「删了还在」）。
// 通过真实 handleUnfriend 流（构造合法载荷）验证，防实现回退。
func TestHandleUnfriendMarksGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	db, err := New(ctx, hb, Config{
		NetworkKey: "grp-uf-guard",
		Name:       "node-b",
		IsTrusted:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	// B 先把 A 记为成员（模拟好友在线）。
	db.addMember(ha.ID(), ha.Addrs(), "test")
	if _, ok := memberOf(db, ha.ID().String()); !ok {
		t.Fatalf("前置失败：A 应已在 B 的成员表")
	}

	// A 发送真实 unfriend 流（与 NotifyUnfriend 同一协议路径）。
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	da, err := New(ctx, ha, Config{NetworkKey: "grp-uf-guard", Name: "node-a"})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.NotifyUnfriend(ctx, hb.ID().String()); err != nil {
		t.Fatalf("notify unfriend: %v", err)
	}

	// 成员表移除与 guard 记录必须同时生效：guard 有记录 ⇒ 之后的迟到
	// 握手（即使信任检查通过）也会被 handleInfo 拦下，不复活成员。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		guarded := db.unfriendGuardBlocked(ha.ID().String())
		_, inMembers := db.members[ha.ID().String()]
		db.mu.Unlock()
		if guarded && !inMembers {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	db.mu.Lock()
	guarded := db.unfriendGuardBlocked(ha.ID().String())
	_, inMembers := db.members[ha.ID().String()]
	db.mu.Unlock()
	if inMembers {
		t.Fatalf("收到删除通知后 A 不应留在 B 的成员表")
	}
	if !guarded {
		t.Fatalf("handleUnfriend 必须记录复活防护（与成员表移除同临界区），否则迟到握手会复活成员")
	}
}

// memberOf 查成员表中是否有指定节点。
func memberOf(d *Discovery, peerID string) (Member, bool) {
	for _, m := range d.Peers() {
		if m.PeerID == peerID {
			return m, true
		}
	}
	return Member{}, false
}
