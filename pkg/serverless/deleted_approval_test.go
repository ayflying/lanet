package serverless

import "testing"

func TestDeletedPeerCannotAutoApprove(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		calls := 0
		d := &Discovery{cfg: Config{
			IsTrusted:    func(string) bool { return trusted },
			IsUnfriended: func(string) bool { return true },
			AutoAccept:   func(string, []string, string) bool { calls++; return true },
			OnPending:    func(string, []string, string) { calls++ },
		}}
		if ok, pending := d.trustPolicy(testPeerA, nil, ""); ok || pending {
			t.Fatal("deleted peer admitted")
		}
		if d.trustPolicyPassive(testPeerA, nil, "") {
			t.Fatal("deleted peer passively admitted")
		}
		if calls != 0 {
			t.Fatal("deleted peer triggered automatic approval or pending callback")
		}
	}
}

// TestDeletedPeerCanReapplyAfterNotice 删除不是拉黑的两阶段语义：
// 未告知 → 完全拒绝（对方先收到一次 unfriended 告知）；
// 告知已送达 → 重新申请进入待审批（仅显式批准可恢复，auto_accept 永不生效）。
func TestDeletedPeerCanReapplyAfterNotice(t *testing.T) {
	notified := map[string]bool{}
	pendingCalls := 0
	d := &Discovery{cfg: Config{
		IsUnfriended:           func(string) bool { return true },
		IsUnfriendedNotified:   func(peerID string) bool { return notified[peerID] },
		MarkUnfriendedNotified: func(peerID string) { notified[peerID] = true },
		AutoAccept: func(string, []string, string) bool {
			t.Fatal("autoaccept must not run for tombstoned peer")
			return false
		},
		OnPending: func(string, []string, string) { pendingCalls++ },
	}}
	// 阶段一：未告知——完全拒绝，不进待审批。
	if ok, pending := d.trustPolicy(testPeerA, nil, ""); ok || pending {
		t.Fatal("un-notified deleted peer admitted")
	}
	if d.trustPolicyPassive(testPeerA, nil, "") {
		t.Fatal("un-notified deleted peer passively admitted")
	}
	if pendingCalls != 0 {
		t.Fatalf("un-notified peer pending calls: %d", pendingCalls)
	}
	// 阶段二：告知已送达——重新申请进入待审批；被动发现仍不放行、不上报。
	d.cfg.MarkUnfriendedNotified(testPeerA)
	ok, pending := d.trustPolicy(testPeerA, nil, "")
	if ok || !pending {
		t.Fatalf("reapply after notice: trusted=%v pending=%v", ok, pending)
	}
	if pendingCalls != 1 {
		t.Fatalf("pending calls after reapply: %d", pendingCalls)
	}
	if d.trustPolicyPassive(testPeerA, nil, "") {
		t.Fatal("notified deleted peer passively admitted")
	}
	if pendingCalls != 1 {
		t.Fatalf("passive discovery must not add pending: %d", pendingCalls)
	}
}
