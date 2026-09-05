package group

import (
	"context"
	"strings"
	"testing"
)

func TestCreateGroupAllocatesSubnetAndEnrollsCreator(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}

	grp, creator, err := registry.Create(context.Background(), CreateInput{
		PeerID: "peer-a", Name: "windows-a", OS: "windows", GroupName: "home-lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if creator.VirtualIP != "10.7.0.2" {
		t.Fatalf("creator virtual IP = %s, want 10.7.0.2", creator.VirtualIP)
	}
	if grp.CIDR != "10.7.0.0/24" {
		t.Fatalf("group CIDR = %s, want 10.7.0.0/24", grp.CIDR)
	}
	if len(grp.InviteCode) != inviteCodeLength {
		t.Fatalf("invite code length = %d, want %d", len(grp.InviteCode), inviteCodeLength)
	}
}

func TestMembersJoinViaInviteAndShareSubnet(t *testing.T) {
	registry, _ := NewRegistry()
	grp, _, err := registry.Create(context.Background(), CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	_, memberB, err := registry.Join(context.Background(), JoinInput{
		InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	})
	if err != nil {
		t.Fatalf("join group: %v", err)
	}
	if memberB.VirtualIP != "10.7.0.3" {
		t.Fatalf("member virtual IP = %s, want 10.7.0.3", memberB.VirtualIP)
	}
	if !strings.HasPrefix(memberB.VirtualIP, "10.7.0.") {
		t.Fatalf("member not in group subnet: %s", memberB.VirtualIP)
	}
}

func TestNetMapOnlyShowsSameGroupMembers(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grpA, _, err := registry.Create(ctx, CreateInput{PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "alpha"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{InviteCode: grpA.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"}); err != nil {
		t.Fatalf("join alpha: %v", err)
	}
	if _, _, err = registry.Create(ctx, CreateInput{PeerID: "peer-c", Name: "c", OS: "linux", GroupName: "beta"}); err != nil {
		t.Fatalf("create beta: %v", err)
	}

	netmapA, err := registry.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap for peer-a: %v", err)
	}
	if len(netmapA.Members) != 2 {
		t.Fatalf("alpha members = %d, want 2", len(netmapA.Members))
	}
	for _, member := range netmapA.Members {
		if member.PeerID == "peer-c" {
			t.Fatal("beta member leaked into alpha netmap")
		}
	}

	netmapC, err := registry.NetMapFor(ctx, "peer-c")
	if err != nil {
		t.Fatalf("netmap for peer-c: %v", err)
	}
	if len(netmapC.Members) != 1 || netmapC.Members[0].PeerID != "peer-c" {
		t.Fatalf("beta should only see itself, got %+v", netmapC.Members)
	}
	if netmapC.CIDR == netmapA.CIDR {
		t.Fatal("distinct groups must have distinct subnets")
	}
}

func TestJoinRejectsInvalidInviteCode(t *testing.T) {
	registry, _ := NewRegistry()
	if _, _, err := registry.Create(context.Background(), CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab",
	}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err := registry.Join(context.Background(), JoinInput{
		InviteCode: "WRONGCODE1", PeerID: "peer-x", Name: "x", OS: "linux",
	}); err == nil {
		t.Fatal("expected invalid invite code to be rejected")
	}
}

func TestPeerCannotJoinTwoGroups(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grpA, _, err := registry.Create(ctx, CreateInput{PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "alpha"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, _, err = registry.Create(ctx, CreateInput{PeerID: "peer-b", Name: "b", OS: "linux", GroupName: "beta"}); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{InviteCode: grpA.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"}); err == nil {
		t.Fatal("expected peer-b joining second group to be rejected")
	}
}

func TestAnnounceAndNetMapExposeReachableAddrs(t *testing.T) {
	ctx := context.Background()
	registry, _ := NewRegistry()

	grp, _, err := registry.Create(ctx, CreateInput{PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "lab"})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, _, err = registry.Join(ctx, JoinInput{InviteCode: grp.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux"}); err != nil {
		t.Fatalf("join group: %v", err)
	}
	if err = registry.Announce(ctx, AnnounceInput{PeerID: "peer-b", Addrs: []string{"/ip4/203.0.113.5/udp/4001/quic-v1"}}); err != nil {
		t.Fatalf("announce: %v", err)
	}

	netmapA, err := registry.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap: %v", err)
	}
	found := false
	for _, member := range netmapA.Members {
		if member.PeerID == "peer-b" && len(member.Addrs) == 1 &&
			member.Addrs[0] == "/ip4/203.0.113.5/udp/4001/quic-v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("announced address missing from netmap")
	}
}

func TestAnnounceRejectsUnknownPeer(t *testing.T) {
	registry, _ := NewRegistry()
	if err := registry.Announce(context.Background(), AnnounceInput{
		PeerID: "ghost", Addrs: []string{"/ip4/203.0.113.5/tcp/4001"},
	}); err == nil {
		t.Fatal("expected unknown peer announce to be rejected")
	}
}
