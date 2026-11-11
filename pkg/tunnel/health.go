package tunnel

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	maxPeerStates = 4096
	maxPeerPaths  = 16
	retryBase     = 2 * time.Second
	retryMax      = 2 * time.Minute
	globalBurst   = 16
	globalRate    = 4.0 // new path attempts/second; healthy reuse never consumes tokens
)

var ErrRetryLimited = errors.New("tunnel retry limit or cooldown reached")

// PathQuality is an application/probe observation of this exact stream's path.
// The caller owns thresholds (RTT/loss are diagnostic only). Three consecutive
// Degraded samples spaced at least 10 seconds quarantine the path for at least
// 60 seconds; one accepted healthy sample clears the streak. Bursts are ignored.
// Report samples only from completed probes, not from local cancellation.
// A successful Open selects the active connection generation; late observations
// from previous connections are ignored. No existing stream is reset/migrated.
type PathQuality struct {
	Degraded bool
	RTT      time.Duration
	Loss     float64
}

// PathHealthRecord is a snapshot, not a migration command. Relay paths are keyed
// by relay PeerID, so changing a relay IP cannot bypass its cooldown.
type PathHealthRecord struct {
	Path            string
	Failures        uint
	DegradedSamples int
	RetryAfter      time.Time
	Updated         time.Time
	RTT             time.Duration
	Loss            float64
	LastError       string
	ConnID          string
	LastSample      time.Time
}
type peerState struct {
	failures    uint
	next        time.Time
	at          time.Time
	paths       map[string]*PathHealthRecord
	activeConn  string
	relayCursor int // bounded rotating index, shared by every IP alias of this peer
}

type retryState struct {
	peers  map[peer.ID]*peerState
	tokens float64
	refill time.Time
}

func pathKey(c network.Conn) string {
	if c == nil || c.RemoteMultiaddr() == nil {
		return ""
	}
	return addressKey(c.RemoteMultiaddr())
}
func addressKey(a ma.Multiaddr) string {
	if a == nil {
		return ""
	}
	if hasCircuit(a) {
		// The P2P component preceding the circuit is the relay, not the target.
		prefix, _ := ma.SplitFunc(a, func(c ma.Component) bool { return c.Protocol().Code == ma.P_CIRCUIT })
		if prefix != nil {
			if id, err := prefix.ValueForProtocol(ma.P_P2P); err == nil {
				return "relay:" + id
			}
		}
	}
	return "direct:" + a.String()
}
func backoff(n uint) time.Duration {
	d := retryBase
	for i := uint(1); i < n && d < retryMax; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

// stateLocked never evicts live cooldowns to admit fresh identities: when full,
// new attempts fail closed. All state is bounded and expires lazily.
func (s *Service) stateLocked(id peer.ID, now time.Time) *peerState {
	if s.retry.peers == nil {
		s.retry.peers = make(map[peer.ID]*peerState)
		s.retry.tokens = globalBurst
		s.retry.refill = now
	}
	if st := s.retry.peers[id]; st != nil {
		st.at = now
		return st
	}
	if len(s.retry.peers) >= maxPeerStates {
		for p, st := range s.retry.peers {
			if now.Sub(st.at) > pathRecordTTL && s.dials[p] == nil {
				delete(s.retry.peers, p)
			}
		}
	}
	if len(s.retry.peers) >= maxPeerStates {
		return nil
	}
	st := &peerState{at: now, paths: make(map[string]*PathHealthRecord)}
	s.retry.peers[id] = st
	return st
}
func (s *Service) admitAttempt(id peer.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	st := s.stateLocked(id, now)
	if st == nil || now.Before(st.next) {
		return ErrRetryLimited
	}
	s.retry.tokens += now.Sub(s.retry.refill).Seconds() * globalRate
	if s.retry.tokens > globalBurst {
		s.retry.tokens = globalBurst
	}
	s.retry.refill = now
	if s.retry.tokens < 1 {
		return ErrRetryLimited
	}
	s.retry.tokens--
	return nil
}
func (s *Service) finishAttempt(id peer.ID, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	st := s.stateLocked(id, now)
	if st == nil {
		return
	}
	if ok {
		st.failures = 0
		st.next = time.Time{}
	} else {
		st.failures++
		st.next = now.Add(backoff(st.failures))
	}
}
func (s *Service) pathBlocked(id peer.ID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.retry.peers[id]
	if st == nil {
		return len(s.retry.peers) >= maxPeerStates
	}
	r := st.paths[key]
	if r == nil && len(st.paths) >= maxPeerPaths {
		return true
	}
	return r != nil && time.Now().Before(r.RetryAfter)
}
func (s *Service) recordFailure(id peer.ID, key string, cause error) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordFailureLocked(id, key, cause)
}
func (s *Service) recordFailureLocked(id peer.ID, key string, cause error) bool {
	now := time.Now()
	st := s.stateLocked(id, now)
	if st == nil {
		return true
	}
	r := st.paths[key]
	if r == nil {
		if len(st.paths) >= maxPeerPaths {
			st.failures++
			st.next = now.Add(backoff(st.failures))
			return true
		}
		r = &PathHealthRecord{Path: key}
		st.paths[key] = r
	}
	r.Failures++
	r.RetryAfter = now.Add(backoff(r.Failures))
	r.Updated = now
	if cause != nil {
		r.LastError = compactError(cause)
	}
	return true
}

// ReportPathFailure quarantines only the actual path of stream. It never closes
// a shared connection or resets the caller's stream. Cancellation, protocol
// rejection and intentional stream arbitration are NOT path failures. Nil,
// context.Canceled, EOF and multistream unsupported errors are ignored here;
// callers MUST filter retired flows, arbitration and parent-context expiration.
// Real stream deadline/timeouts ARE eligible black-hole evidence.
func (s *Service) ReportPathFailure(stream network.Stream, cause error) bool {
	if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, io.EOF) || protocolRejected(cause) {
		return false
	}
	if stream == nil || stream.Conn() == nil {
		return false
	}
	id, key := stream.Conn().RemotePeer(), pathKey(stream.Conn())
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.retry.peers[id]; st != nil && st.activeConn != "" && st.activeConn != stream.Conn().ID() {
		return false
	}
	return s.recordFailureLocked(id, key, cause)
}

// ReportPathQuality returns whether this path is quarantined. Callers may open a
// replacement stream and arbitrate safely; existing streams are not migrated.
func (s *Service) ReportPathQuality(stream network.Stream, sample PathQuality) bool {
	if stream == nil || stream.Conn() == nil {
		return false
	}
	id, key := stream.Conn().RemotePeer(), pathKey(stream.Conn())
	if key == "" {
		return false
	}
	s.mu.Lock()
	now := time.Now()
	st := s.stateLocked(id, now)
	if st == nil {
		s.mu.Unlock()
		return true
	}
	r := st.paths[key]
	if r == nil {
		if len(st.paths) >= maxPeerPaths {
			s.mu.Unlock()
			return true
		}
		r = &PathHealthRecord{Path: key}
		st.paths[key] = r
	}
	// New successful streams select the observation generation. Late probes
	// from an old connection cannot quarantine its replacement.
	if st.activeConn != "" && st.activeConn != stream.Conn().ID() {
		s.mu.Unlock()
		return false
	}
	if r.ConnID != stream.Conn().ID() {
		r.ConnID = stream.Conn().ID()
		r.DegradedSamples = 0
		r.LastSample = time.Time{}
	}
	if !r.LastSample.IsZero() && now.Sub(r.LastSample) < 10*time.Second {
		blocked := now.Before(r.RetryAfter)
		s.mu.Unlock()
		return blocked
	}
	r.LastSample = now
	r.Updated = now
	r.RTT = sample.RTT
	r.Loss = sample.Loss
	if sample.Degraded {
		r.DegradedSamples++
	} else {
		r.DegradedSamples = 0
	}
	trigger := r.DegradedSamples >= 3 && !now.Before(r.RetryAfter)
	blocked := now.Before(r.RetryAfter)
	if trigger {
		s.recordFailureLocked(id, key, errors.New("sustained path degradation"))
		if r.RetryAfter.Before(now.Add(time.Minute)) {
			r.RetryAfter = now.Add(time.Minute)
		}
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()
	return blocked
}
func (s *Service) selectObservationConn(id peer.ID, connID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stateLocked(id, time.Now()); st != nil {
		st.activeConn = connID
	}
}

func (s *Service) PathHealth(target peer.ID) []PathHealthRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PathHealthRecord
	if st := s.retry.peers[target]; st != nil {
		for _, r := range st.paths {
			out = append(out, *r)
		}
	}
	return out
}

// ownedMu serializes the small private-connection registry separately from
// health state. No private connection is installed in host.Network().
type ownedRegistry struct {
	sync.Mutex
	conns   map[peer.ID]*privateConn
	closed  bool
	pending int
	workers sync.WaitGroup
}
