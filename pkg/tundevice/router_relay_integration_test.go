package tundevice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

type routerRelayGate struct{ target peer.ID }

func (g *routerRelayGate) InterceptPeerDial(peer.ID) bool { return true }
func (g *routerRelayGate) InterceptAddrDial(id peer.ID, a ma.Multiaddr) bool {
	return id != g.target || strings.Contains(a.String(), "/p2p-circuit")
}
func (*routerRelayGate) InterceptAccept(network.ConnMultiaddrs) bool { return true }
func (g *routerRelayGate) InterceptSecured(_ network.Direction, id peer.ID, a network.ConnMultiaddrs) bool {
	return id != g.target || strings.Contains(a.RemoteMultiaddr().String(), "/p2p-circuit")
}
func (*routerRelayGate) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) {
	return true, 0
}

type routerRelaySource struct{ peers []peer.AddrInfo }

func (s *routerRelaySource) Candidates(context.Context, int) ([]peer.AddrInfo, error) {
	return s.peers, nil
}
func routerRelayHost(t *testing.T, opts ...libp2p.Option) host.Host {
	t.Helper()
	opts = append([]libp2p.Option{libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.EnableRelay()}, opts...)
	h, err := libp2p.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// End-to-end coverage across real circuit transports, stream negotiation,
// Router flow replacement, IPv4/IPv6 framing and memory-backed TUN delivery.
// The old relay remains live and continues serving another application stream.
func TestRouterRealRelayFailoverPreservesOtherProtocolAndDualStack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	target := routerRelayHost(t)
	source := routerRelayHost(t, libp2p.ConnectionGater(&routerRelayGate{target: target.ID()}))
	hopA, hopB := routerRelayHost(t), routerRelayHost(t)
	for _, h := range []host.Host{hopA, hopB} {
		relay, err := relayv2.New(h)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = relay.Close() })
		if _, err := client.Reserve(ctx, target, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
			t.Fatal(err)
		}
	}
	const otherProto = "/lanet/router-relay-test/other/1"
	target.SetStreamHandler(otherProto, func(s network.Stream) { defer s.Close(); _, _ = io.Copy(s, s) })
	oldAddr := hopA.Addrs()[0].Encapsulate(ma.StringCast("/p2p/" + hopA.ID().String() + "/p2p-circuit/p2p/" + target.ID().String()))
	if err := source.Connect(network.WithAllowLimitedConn(ctx, "test"), peer.AddrInfo{ID: target.ID(), Addrs: []ma.Multiaddr{oldAddr}}); err != nil {
		t.Fatal(err)
	}
	relays := &routerRelaySource{[]peer.AddrInfo{{ID: hopA.ID(), Addrs: hopA.Addrs()}, {ID: hopB.ID(), Addrs: hopB.Addrs()}}}
	svc := tunnel.New(source, &dualStackNetmap{v4: testPeerV4, v6: testPeerV6, id: target.ID()}, relays)
	defer svc.Close()
	other, _, err := svc.OpenStreamToVirtualIPProtocol(ctx, testPeerV4, otherProto)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	oldConn := other.Conn()
	assertHop := func(c network.Conn, want peer.ID) {
		t.Helper()
		if !strings.Contains(c.RemoteMultiaddr().String(), "/p2p/"+want.String()+"/p2p-circuit") {
			t.Fatalf("actual relay path %s, want %s", c.RemoteMultiaddr(), want)
		}
	}
	assertHop(oldConn, hopA.ID())
	memS, _, err := NewMemory(1400)
	if err != nil {
		t.Fatal(err)
	}
	memT, _, err := NewMemory(1400)
	if err != nil {
		t.Fatal(err)
	}
	ds, dt := &recordingDevice{Device: memS}, &recordingDevice{Device: memT}
	rs, rt := New(ds, svc), New(dt, nil)
	rt.selfID = target.ID()
	rs.retireGrace, rt.retireGrace = 10*time.Millisecond, 10*time.Millisecond
	defer rs.Close()
	defer rt.Close()
	target.SetStreamHandler(protocol.Tunnel, func(s network.Stream) { rt.ServeInboundStreamAliases("10.7.0.2", "fd00:6c61:6e65::2", s) })
	waitPacket := func(d *recordingDevice, p []byte) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			for _, got := range d.snapshot() {
				if bytes.Equal(got, p) {
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("packet not delivered: %x", p)
	}
	first := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("relay-a"))
	if err := rs.forwardPacket(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitPacket(dt, first)
	rs.mu.Lock()
	oldFlow := rs.flows[target.ID()]
	rs.mu.Unlock()
	if oldFlow.stream.Conn().ID() != oldConn.ID() {
		t.Fatal("TUN did not share the actual probe/application connection")
	}
	assertHop(oldFlow.stream.Conn(), hopA.ID())
	// Feedback carries the actual (still usable) stream connection, never a
	// peer-level cached label. Only the matching Router flow is invalidated.
	if !svc.ReportPathFailure(other, errors.New("probe transport timeout")) {
		t.Fatal("feedback did not quarantine old path")
	}
	if !rs.InvalidatePath(target.ID(), other.Conn().ID()) {
		t.Fatal("matching TUN flow not invalidated")
	}
	second := buildIPv6Packet("fd00:6c61:6e65::2", testPeerV6, []byte("relay-b-v6"))
	if err := rs.forwardPacket(ctx, second); err != nil {
		t.Fatal(err)
	}
	waitPacket(dt, second)
	rs.mu.Lock()
	next := rs.flows[target.ID()]
	rs.mu.Unlock()
	assertHop(next.stream.Conn(), hopB.ID())
	if next == oldFlow || next.stream.Conn().ID() == oldConn.ID() {
		t.Fatal("replacement did not change actual transport")
	}
	rs.retireFlow(oldFlow, "delayed old pump cleanup")
	if rs.InvalidatePath(target.ID(), oldConn.ID()) {
		t.Fatal("late old probe retired alternate flow")
	}
	rs.mu.Lock()
	assertCurrentDualStackFlow(t, rs, next)
	rs.mu.Unlock()
	// The target's actual inbound handler must accept the next same-initiator
	// flow even when the previous read pump's EOF/cleanup arrived late.
	third := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("relay-b-v4"))
	if err := rs.forwardPacket(ctx, third); err != nil {
		t.Fatal(err)
	}
	waitPacket(dt, third)
	back := buildIPv6Packet(testPeerV6, "fd00:6c61:6e65::2", []byte("return-v6"))
	if err := rt.forwardPacket(ctx, back); err != nil {
		t.Fatal(err)
	}
	waitPacket(ds, back)
	if oldConn.IsClosed() {
		t.Fatal("shared old connection was closed")
	}
	_ = other.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := other.Write([]byte("still-alive")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("still-alive"))
	if _, err := io.ReadFull(other, buf); err != nil || string(buf) != "still-alive" {
		t.Fatalf("unrelated protocol damaged: %q %v", buf, err)
	}
}
