package tunnel

import (
	"context"
	"testing"
	"time"

	"github.com/ayflying/pvn/app/agent/internal/service/netmap"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

type fakeNetmap struct {
	routes []netmap.Route
}

func (f *fakeNetmap) Refresh(ctx context.Context) (netmap.Snapshot, error) {
	return netmap.Snapshot{}, nil
}
func (f *fakeNetmap) Current() netmap.Snapshot { return netmap.Snapshot{} }
func (f *fakeNetmap) Routes() []netmap.Route   { return f.routes }
func (f *fakeNetmap) Resolve(virtualIP string) (netmap.Route, bool) {
	for _, route := range f.routes {
		if route.VirtualIP == virtualIP {
			return route, true
		}
	}
	return netmap.Route{}, false
}
func (f *fakeNetmap) Announce(ctx context.Context, addrs []string) error  { return nil }
func (f *fakeNetmap) RunLoop(ctx context.Context, interval time.Duration) {}

type fakeRelaySource struct{}

func (fakeRelaySource) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	return nil, nil
}

func newTestHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create test host: %v", err)
	}
	return h
}

func TestOpenStreamRejectsUnknownVirtualIP(t *testing.T) {
	self := newTestHost(t)
	defer self.Close()
	service := New(self, &fakeNetmap{}, fakeRelaySource{})
	if _, _, err := service.OpenStreamToVirtualIP(context.Background(), "100.64.0.99"); err == nil {
		t.Fatal("expected unknown virtual IP to be rejected")
	}
}

func TestLastPathUsedDefaultsToUnknown(t *testing.T) {
	service := New(newTestHost(t), &fakeNetmap{}, fakeRelaySource{})
	if got := service.LastPathUsed("peer-x"); got != "unknown" {
		t.Fatalf("last path = %s, want unknown", got)
	}
}

func TestHasCircuitDetectsRelayAddress(t *testing.T) {
	direct, err := ma.NewMultiaddr("/ip4/203.0.113.5/udp/4001/quic-v1")
	if err != nil {
		t.Fatalf("parse direct address: %v", err)
	}
	if hasCircuit(direct) {
		t.Fatal("direct address should not contain circuit")
	}

	relayPeerID := newTestHost(t).ID().String()
	targetPeerID := newTestHost(t).ID().String()
	viaRelay, err := ma.NewMultiaddr("/ip4/203.0.113.5/tcp/443/p2p/" + relayPeerID + "/p2p-circuit/p2p/" + targetPeerID)
	if err != nil {
		t.Fatalf("parse circuit address: %v", err)
	}
	if !hasCircuit(viaRelay) {
		t.Fatal("circuit address should be detected")
	}
}
