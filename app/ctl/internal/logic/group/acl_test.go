package group

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 权限语义验证：群主重置邀请码（旧码作废 + 有效期生效）、群主踢人（IP 回收 + 通告清理）、
// 非群主操作被拒、群主不能踢自己。

func TestOwnerResetsInviteAndOldCodeIsInvalid(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	oldCode := grp.InviteCode // Reset 会就地修改 grp，先留档旧码

	newCode, expires, err := registry.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID,
	})
	if err != nil {
		t.Fatalf("reset invite: %v", err)
	}
	if expires != nil {
		t.Fatalf("default reset should never expire, got %v", expires)
	}
	if len(newCode) != inviteCodeLength {
		t.Fatalf("new invite code length = %d", len(newCode))
	}
	if newCode == oldCode {
		t.Fatal("new invite code must differ from old one")
	}

	// 旧码加入必须失败，新码加入必须成功。
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: oldCode, PeerID: "peer-old", Name: "old", OS: "linux",
	}); err == nil {
		t.Fatal("old invite code should be invalid after reset")
	}
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: newCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join with new invite code: %v", err)
	}
}

func TestResetInviteRejectsNonOwner(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, _, err = registry.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-b", GroupID: grp.ID,
	}); err == nil {
		t.Fatal("non-owner reset should be rejected")
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Fatalf("error should mention owner, got: %v", err)
	}
}

func TestResetInviteWithExpiry(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	newCode, expires, err := registry.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, ValidSeconds: 60,
	})
	if err != nil {
		t.Fatalf("reset invite: %v", err)
	}
	if expires == nil || !expires.After(time.Now().Add(55*time.Second)) {
		t.Fatalf("expires = %v, want ~60s in future", expires)
	}
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: newCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join before expiry should succeed: %v", err)
	}
}

func TestOwnerKicksMemberAndIPIsRecycled(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err = registry.Announce(ctx, AnnounceInput{
		PeerID: "peer-b", Addrs: []string{"/ip4/203.0.113.5/tcp/4001"},
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}

	removed, err := registry.Kick(ctx, KickInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, TargetPeerID: "peer-b",
	})
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	if removed.VirtualIP != "10.7.0.3" {
		t.Fatalf("kicked member virtual IP = %s, want 10.7.0.3", removed.VirtualIP)
	}

	// 被踢成员立即从 NetMap 消失。
	netmap, err := registry.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap: %v", err)
	}
	if len(netmap.Members) != 1 || netmap.Members[0].PeerID != "peer-a" {
		t.Fatalf("kicked member still in netmap: %+v", netmap.Members)
	}
	// 通告地址已清空。
	if _, ok := registry.GroupOf("peer-b"); ok {
		t.Fatal("kicked peer should not belong to any group")
	}

	// 回收的 IP 被下一个新成员复用（10.7.0.3 是最小可用位）。
	if _, memberC, err := registry.Join(ctx, JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-c", Name: "c", OS: "linux",
	}); err != nil || memberC.VirtualIP != "10.7.0.3" {
		t.Fatalf("recycled IP = %s err = %v, want 10.7.0.3", memberC.VirtualIP, err)
	}
}

func TestKickRejectsNonOwnerAndSelfKick(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join: %v", err)
	}

	// 普通成员不能踢人。
	if _, err = registry.Kick(ctx, KickInput{
		OperatorPeerID: "peer-b", GroupID: grp.ID, TargetPeerID: "peer-a",
	}); err == nil {
		t.Fatal("non-owner kick should be rejected")
	}
	// 群主不能踢自己。
	if _, err = registry.Kick(ctx, KickInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, TargetPeerID: "peer-a",
	}); err == nil {
		t.Fatal("owner self-kick should be rejected")
	}
	// 踢一个不属于本群的人必须失败。
	if _, err = registry.Kick(ctx, KickInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, TargetPeerID: "peer-ghost",
	}); err == nil {
		t.Fatal("kick of non-member should be rejected")
	}
}

func TestExpiredInviteIsRejected(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	newCode, _, err := registry.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, ValidSeconds: -10, // 立即过期
	})
	if err != nil {
		t.Fatalf("reset invite: %v", err)
	}
	_ = newCode
	if _, _, err = registry.Join(ctx, JoinInput{
		InviteCode: newCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err == nil {
		t.Fatal("expired invite code should be rejected")
	}
}

func TestOwnerRoleSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := tempDBPath(t)

	first, err := NewPersistentRegistry(ctx, dbPath)
	if err != nil {
		t.Fatalf("open persistent registry: %v", err)
	}
	grp, _, err := first.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err = first.Join(ctx, JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := NewPersistentRegistry(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen persistent registry: %v", err)
	}
	defer func() { _ = second.Close() }()

	// 重启后群主权限仍生效：peer-a 能重置邀请码，peer-b 不能。
	if _, _, err = second.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, ValidSeconds: 60,
	}); err != nil {
		t.Fatalf("owner reset after restore: %v", err)
	}
	if _, _, err = second.ResetInvite(ctx, ResetInviteInput{
		OperatorPeerID: "peer-b", GroupID: grp.ID,
	}); err == nil {
		t.Fatal("non-owner reset after restore should be rejected")
	}
	// 重启后踢人仍可用。
	if _, err = second.Kick(ctx, KickInput{
		OperatorPeerID: "peer-a", GroupID: grp.ID, TargetPeerID: "peer-b",
	}); err != nil {
		t.Fatalf("owner kick after restore: %v", err)
	}
}
