package lanet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
)

func TestStandaloneNonFriendsCanReserveAfterGroupProof(t *testing.T) {
	a := newStandaloneClient(t, "relay-a", "relay-sdk-e2e", filepath.Join(t.TempDir(), "a.db"))
	b := newStandaloneClient(t, "relay-b", "relay-sdk-e2e", filepath.Join(t.TempDir(), "b.db"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := b.node.Connect(ctx, peer.AddrInfo{ID: a.node.ID(), Addrs: a.node.Addrs()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !a.disc.CanRelayPeer(b.node.ID()) || !b.disc.CanRelayPeer(a.node.ID()) {
		if time.Now().After(deadline) {
			t.Fatal("same-group control proof did not converge")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := client.Reserve(ctx, b.node, peer.AddrInfo{ID: a.node.ID(), Addrs: a.node.Addrs()}); err != nil {
		t.Fatalf("nonfriend verified-group reservation rejected: %v", err)
	}
	if a.isTrustedPeer(b.node.ID().String()) || b.isTrustedPeer(a.node.ID().String()) {
		t.Fatal("relay cooperation promoted friend")
	}
}

func TestRelayACLIsFailClosedBeforeDiscovery(t *testing.T) {
	a := &relayGroupACL{}
	if a.AllowReserve(peer.ID("source"), nil) || a.AllowConnect(peer.ID("source"), nil, peer.ID("target")) {
		t.Fatal("uninitialized discovery allowed relay traffic")
	}
}
