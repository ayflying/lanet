package group

import (
	"context"
	"net/netip"
	"testing"
)

// TestNetMapExposesVirtualIPv6 控制面必须把 IPv6 一并下发：每个成员都在本群 /64 内、
// 互不相同，NetMap 同时给出组前缀 cidr_v6。
func TestNetMapExposesVirtualIPv6(t *testing.T) {
	ctx := context.Background()
	registry, err := NewRegistry()
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	grp, creator, err := registry.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if creator.VirtualIPv6 != "fd00:6c61:6e65::2" {
		t.Fatalf("创建者 IPv6 = %s，期望 fd00:6c61:6e65::2", creator.VirtualIPv6)
	}
	if grp.CIDRv6 != "fd00:6c61:6e65::/64" {
		t.Fatalf("群组 cidr_v6 = %s，期望 fd00:6c61:6e65::/64", grp.CIDRv6)
	}
	if _, _, err = registry.Join(ctx, JoinInput{InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"}); err != nil {
		t.Fatalf("join: %v", err)
	}

	netmap, err := registry.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap: %v", err)
	}
	if netmap.CIDRv6 != "fd00:6c61:6e65::/64" {
		t.Fatalf("netmap cidr_v6 = %s", netmap.CIDRv6)
	}
	prefix := netip.MustParsePrefix(netmap.CIDRv6)
	seen := map[string]bool{}
	for _, member := range netmap.Members {
		if member.VirtualIPv6 == "" {
			t.Fatalf("成员 %s 缺少 virtual_ipv6", member.PeerID)
		}
		addr, err := netip.ParseAddr(member.VirtualIPv6)
		if err != nil {
			t.Fatalf("解析 %s: %v", member.VirtualIPv6, err)
		}
		if !prefix.Contains(addr) {
			t.Errorf("成员 %s 的 IPv6 %s 不在 %s 内", member.PeerID, addr, prefix)
		}
		if seen[member.VirtualIPv6] {
			t.Errorf("IPv6 %s 重复", member.VirtualIPv6)
		}
		seen[member.VirtualIPv6] = true
	}
	if len(seen) != 2 {
		t.Fatalf("成员 IPv6 数量 = %d，期望 2", len(seen))
	}
}

// TestKickReclaimsIPv6ForNextMember 踢人回收后，下一个入网成员应复用同一对地址。
func TestKickReclaimsIPv6ForNextMember(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()
	grp, _, err := registry.Create(ctx, CreateInput{PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, memberB, err := registry.Join(ctx, JoinInput{InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	kicked, err := registry.Kick(ctx, KickInput{OperatorPeerID: "peer-a", GroupID: grp.ID, TargetPeerID: "peer-b"})
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	if kicked.VirtualIPv6 != memberB.VirtualIPv6 {
		t.Fatalf("踢出返回的 IPv6 = %s，期望 %s", kicked.VirtualIPv6, memberB.VirtualIPv6)
	}
	_, memberC, err := registry.Join(ctx, JoinInput{InviteCode: grp.InviteCode, PeerID: "peer-c", Name: "c", OS: "linux"})
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if memberC.VirtualIP != memberB.VirtualIP || memberC.VirtualIPv6 != memberB.VirtualIPv6 {
		t.Fatalf("新成员地址 = %s / %s，期望复用 %s / %s",
			memberC.VirtualIP, memberC.VirtualIPv6, memberB.VirtualIP, memberB.VirtualIPv6)
	}
}

// TestPersistentRegistryKeepsIPv6AcrossRestart 重启（重开库）后 IPv4/IPv6 都不能漂移。
func TestPersistentRegistryKeepsIPv6AcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	registry, err := NewPersistentRegistry(ctx, path)
	if err != nil {
		t.Fatalf("open persistent registry: %v", err)
	}
	grp, creator, err := registry.Create(ctx, CreateInput{PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, memberB, err := registry.Join(ctx, JoinInput{InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	before, err := registry.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap before restart: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := NewPersistentRegistry(ctx, path)
	if err != nil {
		t.Fatalf("reopen persistent registry: %v", err)
	}
	defer reopened.Close()
	after, err := reopened.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap after restart: %v", err)
	}
	if after.CIDR != before.CIDR || after.CIDRv6 != before.CIDRv6 {
		t.Fatalf("重启后群组网段变化: %s/%s → %s/%s", before.CIDR, before.CIDRv6, after.CIDR, after.CIDRv6)
	}
	if len(after.Members) != len(before.Members) {
		t.Fatalf("重启后成员数变化: %d → %d", len(before.Members), len(after.Members))
	}
	want := map[string]string{creator.PeerID: creator.VirtualIPv6, memberB.PeerID: memberB.VirtualIPv6}
	for _, member := range after.Members {
		if want[member.PeerID] != member.VirtualIPv6 {
			t.Errorf("重启后成员 %s 的 IPv6 = %s，期望 %s", member.PeerID, member.VirtualIPv6, want[member.PeerID])
		}
	}
}
