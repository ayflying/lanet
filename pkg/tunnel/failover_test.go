package tunnel

import (
	"context"
	"errors"
	"fmt"
	"github.com/libp2p/go-libp2p/core/host"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

type orderedTunnelNetwork struct {
	network.Network
	list []network.Conn
}

func (n orderedTunnelNetwork) ConnsToPeer(peer.ID) []network.Conn { return n.list }

type orderedTunnelHost struct {
	host.Host
	net network.Network
}

func (h orderedTunnelHost) Network() network.Network { return h.net }

type blackholeTunnelConn struct {
	network.Conn
	id    string
	calls int
}

func (c *blackholeTunnelConn) ID() string     { return c.id }
func (c *blackholeTunnelConn) IsClosed() bool { return false }
func (c *blackholeTunnelConn) RemoteMultiaddr() ma.Multiaddr {
	return ma.StringCast("/ip4/192.0.2.1/tcp/1234")
}
func (c *blackholeTunnelConn) NewStream(ctx context.Context) (network.Stream, error) {
	c.calls++
	a, b := net.Pipe()
	return &blackholeTunnelStream{MuxedStream: &pipeTunnelMux{Conn: a, other: b}, conn: c}, nil
}

type pipeTunnelMux struct {
	net.Conn
	other net.Conn
}

func (p *pipeTunnelMux) Close() error                                 { _ = p.other.Close(); return p.Conn.Close() }
func (p *pipeTunnelMux) CloseRead() error                             { return p.Close() }
func (p *pipeTunnelMux) CloseWrite() error                            { return p.Close() }
func (p *pipeTunnelMux) Reset() error                                 { return p.Close() }
func (p *pipeTunnelMux) ResetWithError(network.StreamErrorCode) error { return p.Close() }

type blackholeTunnelStream struct {
	network.Stream
	network.MuxedStream
	conn network.Conn
}

func (s *blackholeTunnelStream) Conn() network.Conn            { return s.conn }
func (s *blackholeTunnelStream) Reset() error                  { return s.MuxedStream.Reset() }
func (s *blackholeTunnelStream) SetDeadline(t time.Time) error { return s.MuxedStream.SetDeadline(t) }
func (s *blackholeTunnelStream) Read(b []byte) (int, error)    { return s.MuxedStream.Read(b) }
func (s *blackholeTunnelStream) Write(b []byte) (int, error)   { return s.MuxedStream.Write(b) }
func (s *blackholeTunnelStream) Close() error                  { return s.MuxedStream.Close() }
func (s *blackholeTunnelStream) CloseRead() error              { return s.MuxedStream.CloseRead() }
func (s *blackholeTunnelStream) CloseWrite() error             { return s.MuxedStream.CloseWrite() }
func (s *blackholeTunnelStream) ResetWithError(c network.StreamErrorCode) error {
	return s.MuxedStream.ResetWithError(c)
}
func (s *blackholeTunnelStream) SetReadDeadline(t time.Time) error {
	return s.MuxedStream.SetReadDeadline(t)
}
func (s *blackholeTunnelStream) SetWriteDeadline(t time.Time) error {
	return s.MuxedStream.SetWriteDeadline(t)
}
func TestBlackholeConnectionDoesNotPoisonHealthyAlternates(t *testing.T) {
	a, b := relayTestHost(t), relayTestHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const proto = "/lanet/blackhole-isolation/1"
	b.SetStreamHandler(proto, func(st network.Stream) { _ = st.Close() })
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatal(err)
	}
	good := a.Network().ConnsToPeer(b.ID())[0]
	bad := &blackholeTunnelConn{Conn: good, id: "blackhole"}
	svc := New(orderedTunnelHost{Host: a, net: orderedTunnelNetwork{Network: a.Network(), list: []network.Conn{bad, good}}}, nil, nil)
	probeCtx, probeCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer probeCancel()
	st, err := svc.openStream(probeCtx, b.ID(), proto)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if !svc.pathBlocked(b.ID(), pathKey(bad)) || svc.pathBlocked(b.ID(), pathKey(good)) {
		t.Fatal("blackhole/healthy quarantine attribution incorrect")
	}
	expired, stop := context.WithCancel(ctx)
	stop()
	svc2 := New(svc.self, nil, nil)
	if _, err := svc2.openStream(expired, b.ID(), proto); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(svc2.PathHealth(b.ID())) != 0 {
		t.Fatal("expired parent poisoned unprobed conns")
	}
}

func TestRelayCandidateSkipsAndRotationReachFourth(t *testing.T) {
	for _, mode := range []string{"invalid", "blocked", "rotation"} {
		t.Run(mode, func(t *testing.T) {
			target := relayTestHost(t)
			initiator := relayTestHost(t, libp2p.ConnectionGater(&directBlockedGate{target: target.ID()}))
			good := relayTestHost(t)
			relay, err := relayv2.New(good)
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := client.Reserve(ctx, target, peer.AddrInfo{ID: good.ID(), Addrs: good.Addrs()}); err != nil {
				t.Fatal(err)
			}
			const proto = "/lanet/fourth-relay/1"
			target.SetStreamHandler(proto, func(st network.Stream) { defer st.Close(); _, _ = io.Copy(st, st) })
			source := &listRelaySource{}
			svc := New(initiator, nil, source)
			defer svc.Close()
			for i := 0; i < 3; i++ {
				bad := relayTestHost(t)
				ai := peer.AddrInfo{ID: bad.ID(), Addrs: bad.Addrs()}
				if mode == "invalid" {
					switch i {
					case 0:
						ai.ID = target.ID()
					case 1:
						ai.ID = initiator.ID()
					case 2:
						ai.Addrs = nil
					}
				}
				if mode == "blocked" {
					svc.recordFailure(target.ID(), "relay:"+bad.ID().String(), errors.New("old failure"))
				}
				source.peers = append(source.peers, ai)
			}
			source.peers = append(source.peers, peer.AddrInfo{ID: good.ID(), Addrs: good.Addrs()})
			if mode == "rotation" {
				st, err := svc.openViaRelay(ctx, target.ID(), proto)
				if err == nil {
					_ = st.Close()
					t.Fatal("first round exceeded three attempted candidates")
				}
				svc.mu.Lock()
				cursor := svc.retry.peers[target.ID()].relayCursor
				// Expire all front cooldowns: cursor, not the cooldown filter, must ensure
				// the fourth candidate is selected first on the next bounded recovery.
				for _, r := range svc.retry.peers[target.ID()].paths {
					r.RetryAfter = time.Time{}
				}
				svc.mu.Unlock()
				if cursor != 3 {
					t.Fatalf("next cursor=%d, want 3", cursor)
				}
			}
			st, err := svc.openViaRelay(ctx, target.ID(), proto)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if pathKey(st.Conn()) != "relay:"+good.ID().String() {
				t.Fatal("wrong actual relay path")
			}
			_ = st.SetDeadline(time.Now().Add(time.Second))
			if _, err := st.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(st, make([]byte, 2)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Old relay remains authenticated, live and in swarm. Only the application
// reports it unhealthy. Connect(target, alternateAddrs) alone cannot pass this.
func TestSwitchAwayFromLiveOldRelayWithoutClosingSharedConn(t *testing.T) {
	target := relayTestHost(t)
	initiator := relayTestHost(t, libp2p.ConnectionGater(&directBlockedGate{target: target.ID()}))
	oldHop, newHop := relayTestHost(t), relayTestHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	oldRelay, err := relayv2.New(oldHop)
	if err != nil {
		t.Fatal(err)
	}
	defer oldRelay.Close()
	newRelay, err := relayv2.New(newHop)
	if err != nil {
		t.Fatal(err)
	}
	defer newRelay.Close()
	for _, ai := range []peer.AddrInfo{{ID: oldHop.ID(), Addrs: oldHop.Addrs()}, {ID: newHop.ID(), Addrs: newHop.Addrs()}} {
		if _, err := client.Reserve(ctx, target, ai); err != nil {
			t.Fatal(err)
		}
	}
	const proto = "/lanet/actual-failover/1"
	target.SetStreamHandler(proto, func(st network.Stream) { defer st.Close(); _, _ = io.Copy(st, st) })
	oldAddr := oldHop.Addrs()[0].Encapsulate(ma.StringCast("/p2p/" + oldHop.ID().String() + "/p2p-circuit/p2p/" + target.ID().String()))
	if err := initiator.Connect(network.WithAllowLimitedConn(ctx, "test"), peer.AddrInfo{ID: target.ID(), Addrs: []ma.Multiaddr{oldAddr}}); err != nil {
		t.Fatal(err)
	}
	source := &listRelaySource{peers: []peer.AddrInfo{{ID: oldHop.ID(), Addrs: oldHop.Addrs()}, {ID: newHop.ID(), Addrs: newHop.Addrs()}}}
	svc := New(initiator, &fakeNetmap{routes: []netmapclient.Route{{VirtualIP: "10.0.0.2", VirtualIPv6: "fd00::2", PeerID: target.ID().String()}}}, source)
	defer svc.Close()
	old, _, err := svc.OpenStreamToVirtualIPProtocol(ctx, "10.0.0.2", proto)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldConn := old.Conn()
	for i := 0; i < 2; i++ {
		if svc.ReportPathQuality(old, PathQuality{Degraded: true}) {
			t.Fatal("quarantine before sustained threshold")
		}
		svc.mu.Lock()
		svc.retry.peers[target.ID()].paths[pathKey(old.Conn())].LastSample = time.Now().Add(-11 * time.Second)
		svc.mu.Unlock()
	}
	if !svc.ReportPathQuality(old, PathQuality{Degraded: true, RTT: time.Second, Loss: .5}) {
		t.Fatal("sustained degradation not quarantined")
	}
	replacement, via, err := svc.OpenStreamToVirtualIPProtocol(ctx, "fd00::2", proto)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	want := "relay:" + newHop.ID().String()
	if !via || pathKey(replacement.Conn()) != want || replacement.Conn().ID() == oldConn.ID() {
		t.Fatalf("not a real alternate path: %s", pathKey(replacement.Conn()))
	}
	if oldConn.IsClosed() {
		t.Fatal("shared old conn was closed")
	}
	_ = replacement.SetDeadline(time.Now().Add(time.Second))
	if _, err := replacement.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(replacement, buf); err != nil {
		t.Fatal(err)
	}
	// Successful private reuse must ignore both exhausted global and peer budget.
	svc.mu.Lock()
	svc.retry.tokens = 0
	svc.retry.refill = time.Now()
	svc.retry.peers[target.ID()].next = time.Now().Add(time.Minute)
	svc.mu.Unlock()
	again, _, err := svc.OpenStreamToVirtualIPProtocol(ctx, "10.0.0.2", proto)
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	if again.Conn().ID() != replacement.Conn().ID() {
		t.Fatal("healthy private connection was not reused")
	}
	if svc.ReportPathQuality(old, PathQuality{Degraded: true}) {
		t.Fatal("late old-conn sample affected replacement")
	}
	if svc.ReportPathFailure(old, errors.New("late old I/O failure")) {
		t.Fatal("late old failure affected replacement")
	}
	if svc.ReportPathFailure(replacement, context.Canceled) || svc.ReportPathFailure(replacement, io.EOF) {
		t.Fatal("intentional cancellation/EOF quarantined")
	}
	// Simultaneous low-quality reports cannot count as three spaced probes.
	for i := 0; i < 100; i++ {
		if svc.ReportPathQuality(replacement, PathQuality{Degraded: true}) {
			t.Fatal("burst samples caused immediate quarantine")
		}
	}
	var closes sync.WaitGroup
	for i := 0; i < 8; i++ {
		closes.Add(1)
		go func() { defer closes.Done(); _ = svc.Close() }()
	}
	closes.Wait()
	if st, _, err := svc.OpenStreamToVirtualIPProtocol(ctx, "10.0.0.2", proto); err == nil {
		_ = st.Close()
		t.Fatal("closed service allowed new stream")
	}
	if oldConn.IsClosed() {
		t.Fatal("service Close closed swarm connection")
	}
}

func TestFastThousandFailuresSharePeerBudgetAcrossIPs(t *testing.T) {
	h := newTestHost(t)
	defer h.Close()
	remote := newTestHost(t)
	defer remote.Close()
	s := New(h, &fakeNetmap{routes: []netmapclient.Route{{VirtualIP: "10.0.0.2", VirtualIPv6: "fd00::2", PeerID: remote.ID().String()}}}, nil)
	defer s.Close()
	for i := 0; i < 1000; i++ {
		ip := "10.0.0.2"
		if i%2 == 1 {
			ip = "fd00::2"
		}
		if st, _, err := s.OpenStreamToVirtualIP(context.Background(), ip); err == nil {
			_ = st.Close()
			t.Fatal("unreachable peer succeeded")
		}
	}
	if len(s.retry.peers) != 1 || s.retry.peers[remote.ID()].failures != 1 {
		t.Fatalf("IP aliases bypassed backoff: %+v", s.retry.peers)
	}
	if s.activeDials != 0 || s.waiters != 0 || len(s.dials) != 0 || len(s.owned.conns) != 0 {
		t.Fatal("resource left after fast failures")
	}
	if time.Until(s.retry.peers[remote.ID()].next) < time.Second {
		t.Fatal("cooldown is not seconds")
	}
}
func TestGlobalAttemptRateAndStateBounds(t *testing.T) {
	s := New(nil, nil, nil)
	start := time.Now()
	n := 0
	for i := 0; i < 1000; i++ {
		if s.admitAttempt(peer.ID(fmt.Sprint(i))) == nil {
			n++
		}
	}
	if n > globalBurst+int(time.Since(start).Seconds()*globalRate) {
		t.Fatalf("global burst %d", n)
	}
	for i := 0; i < maxPeerStates+1000; i++ {
		_ = s.admitAttempt(peer.ID(fmt.Sprint(i)))
	}
	if len(s.retry.peers) > maxPeerStates {
		t.Fatal("state map exceeds cap")
	}
	id := peer.ID("retained")
	s = New(nil, nil, nil)
	for i := 1; i < 10; i++ {
		s.finishAttempt(id, false)
		if d := time.Until(s.retry.peers[id].next); d > retryMax || d < backoff(uint(i))-time.Second {
			t.Fatal(d)
		}
	}
	if backoff(1000) != retryMax {
		t.Fatal("exponential cap missing")
	}
}
func TestConcurrentAdmissionBoundedAndNoUnboundedFlightWaiters(t *testing.T) {
	h := newTestHost(t)
	defer h.Close()
	remote := newTestHost(t)
	defer remote.Close()
	relay := &gatedRelaySource{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(h, &fakeNetmap{routes: []netmapclient.Route{{VirtualIP: "10.0.0.2", PeerID: remote.ID().String()}}}, relay)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _, _ = s.OpenStreamToVirtualIP(context.Background(), "10.0.0.2") }()
	}
	close(start)
	<-relay.entered
	s.mu.Lock()
	if s.waiters > maxDialWaiters || s.activeDials > maxConcurrentDials || len(s.dials) > 1 {
		t.Error("admission bound exceeded")
	}
	s.mu.Unlock()
	close(relay.release)
	wg.Wait()
	if s.waiters != 0 || s.activeDials != 0 || len(s.dials) != 0 {
		t.Fatal("flight leak")
	}
}
func TestRelayPathKeyIgnoresRelayIP(t *testing.T) {
	h := newTestHost(t)
	defer h.Close()
	id := h.ID().String()
	a := ma.StringCast("/ip4/1.2.3.4/tcp/1/p2p/" + id + "/p2p-circuit")
	b := ma.StringCast("/ip4/5.6.7.8/tcp/2/p2p/" + id + "/p2p-circuit")
	s := New(nil, nil, nil)
	target := peer.ID("target")
	s.recordFailure(target, addressKey(a), errors.New("bad"))
	if !s.pathBlocked(target, addressKey(b)) {
		t.Fatal("same relay different IP bypassed cooldown")
	}
	for i := 0; i < 1000; i++ {
		s.recordFailure(target, fmt.Sprint(i), errors.New("bad"))
	}
	if len(s.retry.peers[target].paths) > maxPeerPaths {
		t.Fatal("path map unbounded")
	}
}
