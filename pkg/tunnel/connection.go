package tunnel

// This adapter deliberately owns ONLY connections returned by the authenticated
// relay transport. Swarm.DialPeer has no address-specific public API in v0.49;
// it returns an old usable connection before dialing. TransportForDialing+Dial
// is public, performs the circuit security/mux upgrade and resource accounting,
// and avoids changing peerstore or closing control/DHT connections. These
// private conns are not registered with swarm/identify; reject inbound streams
// rather than accidentally exposing handlers without the host's middleware.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/transport"
	ma "github.com/multiformats/go-multiaddr"
	multistream "github.com/multiformats/go-multistream"
)

const maxPrivateConnections = 64
const maxStreamsPerPrivateConn = 64

var privateSequence atomic.Uint64

type transportSelector interface {
	TransportForDialing(ma.Multiaddr) transport.Transport
}

type privateConn struct {
	transport.CapableConn
	id      string
	rm      network.ResourceManager
	mu      sync.Mutex
	streams map[string]*privateStream
	opened  time.Time
	pending int
	closed  bool
}

func (c *privateConn) ID() string { return c.id }
func (c *privateConn) Stat() network.ConnStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return network.ConnStats{Stats: network.Stats{Direction: network.DirOutbound, Opened: c.opened, Limited: true}, NumStreams: len(c.streams)}
}
func (c *privateConn) GetStreams() []network.Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]network.Stream, 0, len(c.streams))
	for _, s := range c.streams {
		out = append(out, s)
	}
	return out
}
func (c *privateConn) NewStream(ctx context.Context) (network.Stream, error) {
	c.mu.Lock()
	if c.closed || len(c.streams)+c.pending >= maxStreamsPerPrivateConn {
		c.mu.Unlock()
		return nil, ErrRetryLimited
	}
	c.pending++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.pending--; c.mu.Unlock() }()
	scope, err := c.rm.OpenStream(c.RemotePeer(), network.DirOutbound)
	if err != nil {
		return nil, err
	}
	raw, err := c.OpenStream(ctx)
	if err != nil {
		scope.Done()
		return nil, err
	}
	s := &privateStream{MuxedStream: raw, conn: c, scope: scope, id: fmt.Sprintf("%s/s%d", c.id, privateSequence.Add(1)), at: time.Now()}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = raw.Reset()
		scope.Done()
		return nil, errors.New("private connection closed")
	}
	c.streams[s.id] = s
	c.mu.Unlock()
	return s, nil
}
func (c *privateConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	err := c.CapableConn.Close()
	for _, s := range c.GetStreams() {
		_ = s.Reset()
	}
	return err
}
func (c *privateConn) CloseWithError(code network.ConnErrorCode) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	err := c.CapableConn.CloseWithError(code)
	for _, s := range c.GetStreams() {
		_ = s.Reset()
	}
	return err
}

type privateStream struct {
	network.MuxedStream
	conn  *privateConn
	scope network.StreamManagementScope
	id    string
	proto libprotocol.ID
	at    time.Time
	once  sync.Once
}

func (s *privateStream) ID() string               { return s.id }
func (s *privateStream) Protocol() libprotocol.ID { return s.proto }
func (s *privateStream) SetProtocol(p libprotocol.ID) error {
	if err := s.scope.SetProtocol(p); err != nil {
		return err
	}
	s.proto = p
	return nil
}
func (s *privateStream) Conn() network.Conn         { return s.conn }
func (s *privateStream) Scope() network.StreamScope { return s.scope }
func (s *privateStream) Stat() network.Stats {
	return network.Stats{Direction: network.DirOutbound, Opened: s.at, Limited: true}
}
func (s *privateStream) release() {
	s.once.Do(func() { s.scope.Done(); s.conn.mu.Lock(); delete(s.conn.streams, s.id); s.conn.mu.Unlock() })
}
func (s *privateStream) Close() error { err := s.MuxedStream.Close(); s.release(); return err }
func (s *privateStream) Reset() error { err := s.MuxedStream.Reset(); s.release(); return err }
func (s *privateStream) ResetWithError(code network.StreamErrorCode) error {
	err := s.MuxedStream.ResetWithError(code)
	s.release()
	return err
}

// negotiateOnConn opens on one exact connection and performs eager, synchronous
// multistream negotiation. Unlike host.NewStream's cached lazy selector, success
// means the remote has ACKed one of protos, not merely that a local stream exists.
// Deadline + AfterFunc reset terminate negotiation on cancellation without an
// unbounded detached negotiation goroutine. Resource protocol tagging is retained.
func negotiateOnConn(ctx context.Context, c network.Conn, protos ...libprotocol.ID) (network.Stream, error) {
	if len(protos) == 0 {
		return nil, errors.New("no tunnel protocol supplied")
	}
	st, err := c.NewStream(network.WithAllowLimitedConn(ctx, "tunnel exact path"))
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(d)
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = st.Reset(); close(stopped) })
	selected, err := multistream.SelectOneOf(protos, st)
	if !stop() {
		<-stopped
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		err = st.SetProtocol(selected)
	}
	if err != nil {
		_ = st.Reset()
		return nil, err
	}
	_ = st.SetDeadline(time.Time{})
	// A conn-specific API must not silently hand back a stream on a different conn.
	if st.Conn().ID() != c.ID() || st.Conn().RemotePeer() != c.RemotePeer() {
		_ = st.Reset()
		return nil, errors.New("stream escaped selected tunnel connection")
	}
	return st, nil
}
func protocolRejected(err error) bool {
	var rejected multistream.ErrNotSupported[libprotocol.ID]
	return errors.As(err, &rejected)
}

func (s *Service) privateConnections(target peer.ID) []network.Conn {
	s.owned.Lock()
	defer s.owned.Unlock()
	var out []network.Conn
	for id, c := range s.owned.conns {
		if c.IsClosed() {
			delete(s.owned.conns, id)
			continue
		}
		if c.RemotePeer() == target {
			out = append(out, c)
		}
	}
	return out
}
func (s *Service) dialRelayAddress(ctx context.Context, target peer.ID, addr ma.Multiaddr) (network.Conn, error) {
	selector, ok := s.self.Network().(transportSelector)
	if !ok {
		return nil, errors.New("network does not expose authenticated address transport dialing")
	}
	t := selector.TransportForDialing(addr)
	if t == nil {
		return nil, errors.New("circuit relay transport unavailable")
	}
	// Reserve a slot across Dial so simultaneous peers cannot exceed the cap.
	s.owned.Lock()
	if s.owned.closed {
		s.owned.Unlock()
		return nil, errors.New("tunnel service closed")
	}
	if s.owned.conns == nil {
		s.owned.conns = make(map[peer.ID]*privateConn)
	}
	for id, c := range s.owned.conns {
		if c.IsClosed() {
			delete(s.owned.conns, id)
		}
	}
	if len(s.owned.conns)+s.owned.pending >= maxPrivateConnections {
		s.owned.Unlock()
		return nil, ErrRetryLimited
	}
	s.owned.pending++
	s.owned.Unlock()
	defer func() { s.owned.Lock(); s.owned.pending--; s.owned.Unlock() }()
	cc, err := t.Dial(ctx, addr, target)
	if err != nil {
		return nil, err
	}
	if cc.RemotePeer() != target || addressKey(cc.RemoteMultiaddr()) != addressKey(addr) {
		_ = cc.Close()
		return nil, errors.New("relay transport returned unexpected peer or path")
	}
	c := &privateConn{CapableConn: cc, id: fmt.Sprintf("tunnel/%d", privateSequence.Add(1)), rm: s.self.Network().ResourceManager(), streams: make(map[string]*privateStream), opened: time.Now()}
	s.owned.Lock()
	if s.owned.closed {
		s.owned.Unlock()
		_ = cc.Close()
		return nil, errors.New("tunnel service closed")
	}
	s.owned.conns[peer.ID(c.id)] = c
	s.owned.workers.Add(1)
	s.owned.Unlock()
	// One bounded goroutine per owned conn, not per request. This also services the
	// remote host's identify attempt promptly by rejecting unsupported inbound use.
	go func() {
		defer s.owned.workers.Done()
		defer c.Close()
		for {
			st, err := cc.AcceptStream()
			if err != nil {
				return
			}
			_ = st.Reset()
		}
	}()
	return c, nil
}

// Close releases tunnel-owned relay connections only. It is idempotent and never
// closes host.Network connections. Call it during service shutdown.
func (s *Service) Close() error {
	s.owned.Lock()
	s.owned.closed = true
	conns := s.owned.conns
	s.owned.conns = nil
	s.owned.Unlock()
	var result error
	for _, c := range conns {
		if err := c.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	s.owned.workers.Wait()
	return result
}
