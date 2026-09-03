package node

import (
	"context"
	"testing"
)

func TestEnrollAllocatesStableVirtualIP(t *testing.T) {
	registry, err := NewRegistry("100.64.10.0/24", []string{"valid-token"})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}

	first, err := registry.Enroll(context.Background(), EnrollRequest{Token: "valid-token", PeerID: "peer-a", Name: "windows-a", OS: "windows"})
	if err != nil {
		t.Fatalf("enroll first node: %v", err)
	}
	if first.VirtualIP != "100.64.10.2" {
		t.Fatalf("first virtual IP = %s, want 100.64.10.2", first.VirtualIP)
	}

	retry, err := registry.Enroll(context.Background(), EnrollRequest{Token: "valid-token", PeerID: "peer-a", Name: "windows-a", OS: "windows"})
	if err != nil {
		t.Fatalf("repeat enroll: %v", err)
	}
	if retry.VirtualIP != first.VirtualIP {
		t.Fatalf("repeat virtual IP = %s, want %s", retry.VirtualIP, first.VirtualIP)
	}

	second, err := registry.Enroll(context.Background(), EnrollRequest{Token: "valid-token", PeerID: "peer-b", Name: "linux-b", OS: "linux"})
	if err != nil {
		t.Fatalf("enroll second node: %v", err)
	}
	if second.VirtualIP != "100.64.10.3" {
		t.Fatalf("second virtual IP = %s, want 100.64.10.3", second.VirtualIP)
	}
}

func TestEnrollRejectsInvalidToken(t *testing.T) {
	registry, err := NewRegistry("100.64.10.0/24", []string{"valid-token"})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	if _, err = registry.Enroll(context.Background(), EnrollRequest{Token: "invalid", PeerID: "peer-a", Name: "windows-a"}); err == nil {
		t.Fatal("expected invalid enroll token to be rejected")
	}
}

func TestNewRegistryRejectsNon24CIDR(t *testing.T) {
	if _, err := NewRegistry("100.64.10.0/16", []string{"token"}); err == nil {
		t.Fatal("expected non-/24 CIDR to be rejected for MVP")
	}
}
