package serverless

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// ---- 连接审批（加好友式）与流量控制 ----

// TestApprovalRejectsUntrusted 未审批的陌生节点必须被完全隔离：
// 拿不到 info 响应（不回写任何标识信息），且不进成员表。
func TestApprovalRejectsUntrusted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	var pending []string
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-approval",
		Name:       "node-a",
		IsTrusted:  func(string) bool { return false }, // 谁都不信任
		OnPending: func(peerID string, _ []string, _ string) {
			pending = append(pending, peerID)
		},
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-approval", Name: "node-b"})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// B 主动连 A 并请求 info —— A 应明确回「未同意」，且不泄漏任何身份信息。
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	rej, err := db.fetchInfo(ctx, ha.ID())
	if err != nil {
		// 拒答也算合格（老语义）；只要拿不到身份信息即可。
		t.Logf("对端直接断开（可接受）: %v", err)
	} else {
		if rej.Rejected == "" {
			t.Fatalf("未审批节点不应拿到正常 info 响应（应先经用户同意）")
		}
		// 关键：拒绝响应里绝不能带名称/版本/主机名/本机 IP 等身份信息。
		if rej.Name != "" || rej.Version != "" || rej.Platform != "" ||
			rej.OSHostname != "" || len(rej.LocalIPs) > 0 {
			t.Fatalf("拒绝响应泄漏了身份信息: %+v", rej)
		}
	}
	// A 的成员表里不应出现 B（完全隔离：没有 NetMap、没有虚拟 IP）。
	for _, m := range da.Peers() {
		if m.PeerID == hb.ID().String() {
			t.Fatalf("未审批节点不得进入成员表")
		}
	}
	// A 应把 B 记入待审批列表。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pending) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pending) == 0 || pending[0] != hb.ID().String() {
		t.Fatalf("陌生节点申请连接后应上报待审批，实际 %v", pending)
	}
}

// TestApprovalAllowsTrusted 已审批节点可正常握手 + 进成员表 + 互通。
func TestApprovalAllowsTrusted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	// 双方互相信任（模拟已互加好友）。
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-trusted",
		Name:       "node-a",
		IsTrusted:  func(peerID string) bool { return peerID == hb.ID().String() },
		OnPending:  func(string, []string, string) { t.Fatalf("已信任节点不该落入待审批") },
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	db, err := New(ctx, hb, Config{
		NetworkKey: "grp-trusted",
		Name:       "node-b",
		IsTrusted:  func(peerID string) bool { return peerID == ha.ID().String() },
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

	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("已信任节点应能交换 info: %v", err)
	}
	if info.Name != "node-b" {
		t.Fatalf("info.Name = %q, want node-b", info.Name)
	}
	// 走一次 addMember（发现路径），节点应进成员表并完成信息交换。
	da.addMember(hb.ID(), hb.Addrs(), "test")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range da.Peers() {
			if m.PeerID == hb.ID().String() && m.Name == "node-b" {
				return // 已进成员表且名称已由 info 交换补齐
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("已信任节点建连后应进入成员表并完成信息交换")
}

// TestAutoAcceptUnattended auto_accept 开启（无人值守中央服务器）时，
// 陌生节点无需人工审批即自动放行，且不产生待审批记录。
func TestAutoAcceptUnattended(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	var autoAccepted []string
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-auto",
		Name:       "central",
		IsTrusted:  func(string) bool { return false },
		AutoAccept: func(peerID string, _ []string, _ string) bool {
			autoAccepted = append(autoAccepted, peerID)
			return true
		},
		OnPending: func(string, []string, string) { t.Fatalf("自动放行后不应再记待审批") },
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-auto", Name: "node-b"})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}

	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, err := db.fetchInfo(ctx, ha.ID())
	if err != nil {
		t.Fatalf("auto_accept 开启时陌生节点应能直接握手: %v", err)
	}
	if info.Name != "central" {
		t.Fatalf("info.Name = %q, want central", info.Name)
	}
	if len(autoAccepted) == 0 || autoAccepted[0] != hb.ID().String() {
		t.Fatalf("auto_accept 应记录被自动放行的节点，实际 %v", autoAccepted)
	}
}

// TestNoTrustCallbackKeepsLegacyBehavior 不传 IsTrusted 时保持历史行为：
// 同群节点直接互通（向后兼容，SDK 简单用法与既有测试不受影响）。
func TestNoTrustCallbackKeepsLegacyBehavior(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)
	da, err := New(ctx, ha, Config{NetworkKey: "grp-legacy", Name: "node-a"})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-legacy", Name: "node-b"})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err = db.fetchInfo(ctx, ha.ID()); err != nil {
		t.Fatalf("未启用审批时同群节点必须可直接握手: %v", err)
	}
}

// TestEmptyAddressBookSkipsFindProviders 流量控制：地址簿为空（首次启动）
// 时不做主动 DHT 查找（只保留自身 Provide 广播，让别人能找到我）。
func TestEmptyAddressBookSkipsFindProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	empty := true
	h := testHost(t, false)
	d, err := New(ctx, h, Config{
		NetworkKey:    "grp-flow",
		Name:          "solo",
		HasKnownPeers: func() bool { return !empty },
	})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if err = d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if d.knownPeers() {
		t.Fatalf("地址簿为空时 knownPeers 应为 false")
	}
	// 地址簿有内容后恢复查找。
	empty = false
	if !d.knownPeers() {
		t.Fatalf("地址簿非空时 knownPeers 应为 true")
	}
	// 未传 HasKnownPeers 时保持历史行为（每轮查找）。
	d2, err := New(ctx, testHost(t, false), Config{NetworkKey: "grp-flow2"})
	if err != nil {
		t.Fatalf("new discovery 2: %v", err)
	}
	if !d2.knownPeers() {
		t.Fatalf("未配置 HasKnownPeers 时应保持历史行为（返回 true）")
	}
}

// TestRequestConnectPendingUntrusted 通过 RequestConnect 申请连接陌生节点：
// 应提交待审批并给出明确提示，而不是静默失败。
func TestRequestConnectPendingUntrusted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)
	var pendingCount int
	da, err := New(ctx, ha, Config{
		NetworkKey: "grp-req",
		IsTrusted:  func(string) bool { return false },
		OnPending:  func(string, []string, string) { pendingCount++ },
	})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	_, err = da.RequestConnect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()})
	if err == nil {
		t.Fatalf("申请连接陌生节点应返回「等待对方同意」提示")
	}
	if !strings.Contains(err.Error(), "等待对方同意") {
		t.Fatalf("错误信息应说明已提交申请，实际: %v", err)
	}
	if pendingCount == 0 {
		t.Fatalf("申请连接陌生节点应记入待审批")
	}
}
