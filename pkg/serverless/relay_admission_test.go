package serverless

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

func TestRelayRealControlProofDoesNotPromoteBusiness(t *testing.T) {
	a, b := testHost(t, false), testHost(t, false)
	cfg := Config{IsTrusted: func(string) bool { return false }}
	da := controlTestDiscovery(t, a, "relay-proof-integration", cfg)
	db := controlTestDiscovery(t, b, "relay-proof-integration", cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := da.exchangeControl(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}, false); err != nil {
		t.Fatal(err)
	}
	if !da.CanRelayPeer(b.ID()) || !db.CanRelayPeer(a.ID()) {
		t.Fatal("mutual control proof did not authorize relay")
	}
	if len(da.Peers()) != 0 || len(db.Peers()) != 0 || da.trustedNow(b.ID().String()) {
		t.Fatal("relay cooperation promoted business membership")
	}
	wrong := controlTestDiscovery(t, testHost(t, false), "other-group", cfg)
	if err := wrong.exchangeControl(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}, false); err == nil {
		t.Fatal("cross-group proof accepted")
	}
	if db.CanRelayPeer(wrong.host.ID()) {
		t.Fatal("cross-group relay admitted")
	}
}

func TestRelayProofExpiresWithoutPromotingFriend(t *testing.T) {
	now := time.Now()
	id := mustPeerID(t, testPeerA)
	d := &Discovery{serviceCtx: context.Background(), members: make(map[string]*Member)}
	d.cfg.IsTrusted = func(string) bool { return false }
	if d.CanRelayPeer(id) {
		t.Fatal("unverified peer admitted")
	}
	d.control.remember(peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/192.168.1.20/tcp/4001")}}, now)
	if !d.CanRelayPeer(id) {
		t.Fatal("fresh direct proof not admitted")
	}
	if d.trustedNow(id.String()) || len(d.members) != 0 {
		t.Fatal("relay proof promoted business trust")
	}
	if d.control.verified(id, now.Add(controlCollabTTL)) {
		t.Fatal("expired proof admitted")
	}
	if d.CanRelayPeer(id) {
		t.Fatal("expired proof revived")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.serviceCtx = ctx
	d.control.remember(peer.AddrInfo{ID: id}, now)
	if d.CanRelayPeer(id) {
		t.Fatal("stopped discovery admitted relay")
	}
}

func TestRelayCandidatesIncludeVerifiedNonFriendOnly(t *testing.T) {
	h := testHost(t, false)
	id := mustPeerID(t, testPeerA)
	addr := ma.StringCast("/ip4/192.168.1.20/tcp/4001")
	d := &Discovery{host: h, serviceCtx: context.Background(), memberTTL: time.Hour, members: make(map[string]*Member)}
	d.cfg.IsTrusted = func(string) bool { return false }
	d.control.remember(peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{addr}}, time.Now())
	got, err := d.Candidates(context.Background(), 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown hop admitted: %v %v", got, err)
	}
	if err = h.Peerstore().AddProtocols(id, libprotocol.ID("/libp2p/circuit/relay/0.2.0/hop")); err != nil {
		t.Fatal(err)
	}
	got, err = d.Candidates(context.Background(), 10)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("verified nonfriend hop missing: %v %v", got, err)
	}
	if len(d.members) != 0 {
		t.Fatal("cooperation leaked into business members")
	}
	d.control.mu.Lock()
	e := d.control.peers[id]
	e.expires = time.Now().Add(-time.Second)
	d.control.peers[id] = e
	d.control.mu.Unlock()
	got, err = d.Candidates(context.Background(), 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("expired cohort returned: %v %v", got, err)
	}
}
