package tunnel

import (
	"context"
	"io"
	"testing"
	"time"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

// This gate models a failed direct/NAT traversal path, while retaining the
// authenticated circuit transport. No mock host or fake circuit connection.
type directBlockedGate struct{ target peer.ID }

func (g *directBlockedGate) InterceptPeerDial(peer.ID) bool { return true }
func (g *directBlockedGate) InterceptAddrDial(id peer.ID, addr ma.Multiaddr) bool {
	return id != g.target || hasCircuit(addr)
}
func (g *directBlockedGate) InterceptAccept(network.ConnMultiaddrs) bool { return true }
func (g *directBlockedGate) InterceptSecured(_ network.Direction, id peer.ID, addr network.ConnMultiaddrs) bool {
	return id != g.target || hasCircuit(addr.RemoteMultiaddr())
}
func (g *directBlockedGate) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

type listRelaySource struct {
	peers     []peer.AddrInfo
	requested int
}

func (r *listRelaySource) Candidates(_ context.Context, number int) ([]peer.AddrInfo, error) {
	r.requested = number
	return r.peers, nil
}

func relayTestHost(t *testing.T, opts ...libp2p.Option) host.Host {
	t.Helper()
	opts = append([]libp2p.Option{libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.EnableRelay()}, opts...)
	h, err := libp2p.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestRelayFallbackBlockedDirectAndSwitchCandidates(t *testing.T) {
	for _, failures := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failures))+" failed candidates", func(t *testing.T) {
			target := relayTestHost(t)
			gate := &directBlockedGate{target: target.ID()}
			initiator := relayTestHost(t, libp2p.ConnectionGater(gate))
			good := relayTestHost(t)
			relay, err := relayv2.New(good)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = relay.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.Reserve(ctx, target, peer.AddrInfo{ID: good.ID(), Addrs: good.Addrs()}); err != nil {
				t.Fatal(err)
			}
			const proto libprotocol.ID = "/lanet/relay-regression/1"
			target.SetStreamHandler(proto, func(st network.Stream) { defer st.Close(); _, _ = io.Copy(st, st) })
			source := &listRelaySource{}
			for i := 0; i < failures; i++ {
				bad := relayTestHost(t)
				if i == 0 {
					badRelay, err := relayv2.New(bad)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = badRelay.Close() })
				}
				source.peers = append(source.peers, peer.AddrInfo{ID: bad.ID(), Addrs: bad.Addrs()})
			}
			source.peers = append(source.peers, peer.AddrInfo{ID: good.ID(), Addrs: good.Addrs()})
			initiator.Peerstore().AddAddrs(target.ID(), target.Addrs(), peerstore.TempAddrTTL)
			if err := initiator.Connect(ctx, peer.AddrInfo{ID: target.ID()}); err == nil {
				t.Fatal("direct path unexpectedly connected")
			}
			svc := New(initiator, &fakeNetmap{routes: []netmapclient.Route{{VirtualIP: "10.7.0.2", PeerID: target.ID().String()}}}, source)
			st, viaRelay, err := svc.OpenStreamToVirtualIPProtocol(ctx, "10.7.0.2", proto)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if !viaRelay || !hasCircuit(st.Conn().RemoteMultiaddr()) {
				t.Fatal("expected actual circuit transport")
			}
			if source.requested <= 2 {
				t.Fatalf("candidate request still truncated: %d", source.requested)
			}
			_ = st.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := st.Write([]byte("relay payload")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len("relay payload"))
			if _, err := io.ReadFull(st, buf); err != nil {
				t.Fatal(err)
			}
			if string(buf) != "relay payload" {
				t.Fatalf("wrong echo: %q", buf)
			}
		})
	}
}

func TestRelayFallbackRequiresTargetReservation(t *testing.T) {
	target := relayTestHost(t)
	initiator := relayTestHost(t, libp2p.ConnectionGater(&directBlockedGate{target: target.ID()}))
	hop := relayTestHost(t)
	relay, err := relayv2.New(hop)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ai := peer.AddrInfo{ID: hop.ID(), Addrs: hop.Addrs()}
	// Only the initiator reserves: that is NOT enough to reach the target.
	if _, err := client.Reserve(ctx, initiator, ai); err != nil {
		t.Fatal(err)
	}
	svc := New(initiator, nil, &listRelaySource{peers: []peer.AddrInfo{ai}})
	if st, err := svc.openViaRelay(ctx, target.ID(), "/lanet/relay-regression/1"); err == nil {
		_ = st.Close()
		t.Fatal("initiator reservation falsely proved target reachability")
	}
}
