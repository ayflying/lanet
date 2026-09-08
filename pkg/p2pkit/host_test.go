package p2pkit

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

func TestFilterUnderlayAddrsRemovesLanetOverlay(t *testing.T) {
	input := []ma.Multiaddr{
		ma.StringCast("/ip4/10.7.9.215/tcp/4001"),
		ma.StringCast("/ip4/192.168.50.217/tcp/4001"),
		ma.StringCast("/ip6/::1/tcp/4001"),
		ma.StringCast("/ip4/10.8.9.215/udp/4001/quic-v1"),
	}
	got := FilterUnderlayAddrs(input)
	want := []string{
		"/ip4/192.168.50.217/tcp/4001",
		"/ip6/::1/tcp/4001",
		"/ip4/10.8.9.215/udp/4001/quic-v1",
	}
	if len(got) != len(want) {
		t.Fatalf("filtered addresses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("filtered address %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestLanetOverlayGaterBlocksOverlayDial(t *testing.T) {
	gater := lanetOverlayGater{}
	tests := []struct {
		addr  string
		allow bool
	}{
		{addr: "/ip4/10.7.0.1/tcp/4001", allow: false},
		{addr: "/ip4/10.7.255.254/udp/4001/quic-v1", allow: false},
		{addr: "/ip4/10.8.0.1/tcp/4001", allow: true},
		{addr: "/ip4/192.168.50.217/tcp/4001", allow: true},
		{addr: "/ip6/::1/tcp/4001", allow: true},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := gater.InterceptAddrDial(peer.ID("peer"), ma.StringCast(tt.addr))
			if got != tt.allow {
				t.Fatalf("InterceptAddrDial(%s) = %v, want %v", tt.addr, got, tt.allow)
			}
		})
	}
}
