package p2pkit

import (
	"net/netip"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	relayAdmissionMaxPeers = 1024
	relayAdmissionMaxIPs   = 256
	// ACLFilter has no circuit-close callback. Conservatively retain a slot
	// through ConnectTimeout + HandshakeTimeout + circuit lifetime + margin.
	// Failed/early-closed circuits do NOT refund slots or traffic quota.
	relayCircuitLease = relayCircuitDuration + 3*time.Minute
)

type relayAdmissionState struct {
	reserve []time.Time
	connect []time.Time
	leases  []time.Time
}

type relayAdmission struct {
	mu     sync.Mutex
	acl    relayv2.ACLFilter
	now    func() time.Time
	global relayAdmissionState
	peers  map[peer.ID]*relayAdmissionState
	ips    map[netip.Prefix]*relayAdmissionState
	// Unauthenticated work has a separate short-lived, fixed-size gate. It
	// cannot allocate identity records or consume authorized traffic quotas.
	requests []time.Time
	checking int
}

func newRelayAdmission(acl relayv2.ACLFilter) *relayAdmission {
	return &relayAdmission{acl: acl, now: time.Now,
		peers: make(map[peer.ID]*relayAdmissionState), ips: make(map[netip.Prefix]*relayAdmissionState)}
}

func keepRelayTimes(ts []time.Time, since time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(since) {
		i++
	}
	return ts[i:]
}

func (s *relayAdmissionState) prune(now time.Time) {
	s.reserve = keepRelayTimes(s.reserve, now.Add(-time.Minute))
	s.connect = keepRelayTimes(s.connect, now.Add(-time.Hour))
	s.leases = keepRelayTimes(s.leases, now.Add(-relayCircuitLease))
}
func (s *relayAdmissionState) empty() bool {
	return len(s.reserve)+len(s.connect)+len(s.leases) == 0
}
func relayRecent(ts []time.Time, since time.Time) int {
	return len(keepRelayTimes(ts, since))
}

// Exact IPv4, /64 IPv6: rotating interface IDs cannot reset a host's quotas.
// Non-IP transports share one bucket instead of escaping IP accounting.
func relayIPKey(addr ma.Multiaddr) netip.Prefix {
	ip, ok := AddrIP(addr)
	if !ok {
		return netip.Prefix{}
	}
	ip = ip.Unmap()
	bits := 64
	if ip.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(ip, bits).Masked()
}

func (a *relayAdmission) prune(now time.Time) {
	a.global.prune(now)
	for id, s := range a.peers {
		s.prune(now)
		if s.empty() {
			delete(a.peers, id)
		}
	}
	for ip, s := range a.ips {
		s.prune(now)
		if s.empty() {
			delete(a.ips, ip)
		}
	}
}

// Allocate only after capacity checks. Never evict live quota records: otherwise
// PeerID/IP rotation could both grow memory and reset the traffic budget.
func (a *relayAdmission) states(src, dest peer.ID, ip netip.Prefix) (*relayAdmissionState, *relayAdmissionState, *relayAdmissionState, bool) {
	needed := 0
	if a.peers[src] == nil {
		needed++
	}
	if dest != "" && dest != src && a.peers[dest] == nil {
		needed++
	}
	if len(a.peers)+needed > relayAdmissionMaxPeers || (a.ips[ip] == nil && len(a.ips) >= relayAdmissionMaxIPs) {
		return nil, nil, nil, false
	}
	if a.peers[src] == nil {
		a.peers[src] = &relayAdmissionState{}
	}
	if dest != "" && a.peers[dest] == nil {
		a.peers[dest] = &relayAdmissionState{}
	}
	if a.ips[ip] == nil {
		a.ips[ip] = &relayAdmissionState{}
	}
	return a.peers[src], a.peers[dest], a.ips[ip], true
}

// checkMembership limits callback work independently of authenticated quotas.
// Callbacks must be nonblocking local lookups; no network I/O or reentry. The
// gate admits at most 1024 checks/second and 16 concurrently, keeps no attacker
// IDs, and recovers one second after a flood stops (not an hour-long outage).
func (a *relayAdmission) checkMembership(check func() bool) bool {
	a.mu.Lock()
	now := a.now()
	a.requests = keepRelayTimes(a.requests, now.Add(-time.Second))
	if len(a.requests) >= 1024 || a.checking >= 16 {
		a.mu.Unlock()
		return false
	}
	a.requests = append(a.requests, now)
	a.checking++
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.checking--; a.mu.Unlock() }()
	return check()
}

func (a *relayAdmission) AllowReserve(p peer.ID, addr ma.Multiaddr) bool {
	if p == "" || !a.checkMembership(func() bool { return a.acl == nil || a.acl.AllowReserve(p, addr) }) {
		return false
	}
	a.mu.Lock()
	now := a.now()
	a.prune(now)
	s, _, ip, ok := a.states(p, "", relayIPKey(addr))
	if ok {
		ok = len(a.global.reserve) < 128 && len(s.reserve) < 6 && len(ip.reserve) < 24
	}
	if ok {
		a.global.reserve = append(a.global.reserve, now)
		s.reserve = append(s.reserve, now)
		ip.reserve = append(ip.reserve, now)
	}
	a.mu.Unlock()
	// Membership was checked for this renewal, never cached by admission.
	return ok
}

func (a *relayAdmission) AllowConnect(src peer.ID, addr ma.Multiaddr, dest peer.ID) bool {
	started := a.now()
	if src == "" || dest == "" || src == dest || !a.checkMembership(func() bool { return a.acl == nil || a.acl.AllowConnect(src, addr, dest) }) {
		return false
	}
	// Expire slow decisions; only verified requests consume circuit quotas.
	if a.now().Sub(started) >= time.Minute || !a.admitConnect(src, addr, dest) {
		return false
	}
	// Start the circuit lease AFTER the external ACL returns: even a slow
	// caller must not shorten the lifetime of the concurrency accounting.
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.prune(now)
	s, d, ip, ok := a.states(src, dest, relayIPKey(addr))
	if !ok || len(a.global.leases) >= 64 || len(s.leases) >= 4 || len(d.leases) >= 4 || len(ip.leases) >= 8 {
		return false
	}
	for _, state := range []*relayAdmissionState{&a.global, s, d, ip} {
		state.leases = append(state.leases, now)
	}
	return true
}

func (a *relayAdmission) admitConnect(src peer.ID, addr ma.Multiaddr, dest peer.ID) bool {
	if src == "" || dest == "" || src == dest {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.prune(now)
	s, d, ip, ok := a.states(src, dest, relayIPKey(addr))
	if !ok {
		return false
	}
	// Sliding windows, not resettable fixed buckets. Every attempt precharges
	// the full 128 MiB bidirectional circuit allowance, with no early refund.
	// Global 32/minute and 128/hour => <=4 GiB/16 GiB of newly admitted
	// circuit allowances; each peer (source OR destination) <=4/min, 12/hour.
	// Existing circuits can carry their prior allowance: this is a traffic
	// volume/admission budget, NOT a bandwidth-peak shaper.
	if len(a.global.connect) >= 128 || relayRecent(a.global.connect, now.Add(-time.Minute)) >= 32 ||
		len(s.connect) >= 12 || len(d.connect) >= 12 ||
		relayRecent(s.connect, now.Add(-time.Minute)) >= 4 || relayRecent(d.connect, now.Add(-time.Minute)) >= 4 ||
		len(ip.connect) >= 32 || relayRecent(ip.connect, now.Add(-time.Minute)) >= 8 {
		return false
	}
	for _, state := range []*relayAdmissionState{&a.global, s, d, ip} {
		state.connect = append(state.connect, now)
	}
	return true
}
