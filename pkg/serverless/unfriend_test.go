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
	db, err := New(ctx, hb, Config{
		NetworkKey: "grp-uf",
		Name:       "node-b",
		IsTrusted:  func(string) bool { return true },
		OnUnfriendReceived: func(peerID string) {
			mu.Lock()
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
	defer mu.Unlock()
	if len(unfriended) == 0 || unfriended[0] != ha.ID().String() {
		t.Fatalf("B 应收到删除好友通知并回调本机 ID，实际 %v", unfriended)
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
func TestNearbyReportOnPassiveDiscover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	var mu sync.Mutex
	var seen []string
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-nearby",
		Name:       "node-a",
		IsTrusted:  func(string) bool { return false }, // 谁都不信任
		OnPending:  func(string, []string, string) {},  // 被动发现不上报待审批，提供空实现避免 nil 差异
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
	if _, ok := memberOf(da, hb.ID().String()); ok {
		t.Fatalf("未信任节点不得进入成员表")
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
