package serverless

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	controlCollabTimeout    = 4 * time.Second
	controlCollabTTL        = 5 * time.Minute
	controlCollabCooldown   = 30 * time.Second
	controlCollabConcurrent = 8
	controlCollabCandidates = 512
	controlCollabHints      = 32
	controlCollabAddrs      = 4
	controlCollabMessage    = 64 * 1024
)

type controlEntry struct {
	info    peer.AddrInfo
	expires time.Time
}
type controlAttempt struct {
	until  time.Time
	active bool
}

// A control proof establishes only possession of the group key, never friendship,
// pending approval, relay permission, or authority to initiate a business handshake.
// This cache and its limiter deliberately do not share the members mutex.
type controlState struct {
	mu    sync.Mutex
	peers map[peer.ID]controlEntry
	controlRate
	// Simultaneous probes must not consume each other's per-peer cooldown or
	// capacity. Inbound and outbound each permit eight independent exchanges.
	inbound controlRate
}

type controlRate struct {
	mu       sync.Mutex
	attempts map[peer.ID]controlAttempt
	active   int
}

func (c *controlRate) acquire(id peer.ID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active >= controlCollabConcurrent {
		return false
	}
	if a, ok := c.attempts[id]; ok && (a.active || now.Before(a.until)) {
		return false
	}
	if c.attempts == nil {
		c.attempts = make(map[peer.ID]controlAttempt)
	}
	for id, a := range c.attempts {
		if !a.active && !now.Before(a.until) {
			delete(c.attempts, id)
		}
	}
	if len(c.attempts) >= controlCollabCandidates {
		return false
	}
	c.attempts[id] = controlAttempt{until: now.Add(controlCollabCooldown), active: true}
	c.active++
	return true
}
func (c *controlRate) release(id peer.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempts[id]
	a.active = false
	c.attempts[id] = a
	c.active--
}
func controlClean(ai peer.AddrInfo) peer.AddrInfo {
	filtered := make([]ma.Multiaddr, 0, len(ai.Addrs))
	for _, addr := range ai.Addrs {
		if addr == nil || len(addr.Bytes()) > 2048 {
			continue
		}
		bad := false
		ma.ForEach(addr, func(c ma.Component) bool {
			switch c.Protocol().Code {
			case ma.P_IP4, ma.P_IP6:
				ip := net.ParseIP(c.Value())
				bad = ip == nil || ip.IsUnspecified() || ip.IsMulticast()
			case ma.P_TCP, ma.P_UDP:
				bad = c.Value() == "0"
			}
			return !bad
		})
		if !bad {
			filtered = append(filtered, addr)
		}
	}
	ai.Addrs = p2pkit.CleanUnderlayAddrs(filtered)
	if len(ai.Addrs) > controlCollabAddrs {
		ai.Addrs = ai.Addrs[:controlCollabAddrs]
	}
	return ai
}
func (c *controlState) remember(ai peer.AddrInfo, now time.Time) {
	ai = controlClean(ai)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peers == nil {
		c.peers = make(map[peer.ID]controlEntry)
	}
	for id, e := range c.peers {
		if !now.Before(e.expires) {
			delete(c.peers, id)
		}
	}
	if _, ok := c.peers[ai.ID]; !ok && len(c.peers) >= controlCollabCandidates {
		return
	}
	c.peers[ai.ID] = controlEntry{ai, now.Add(controlCollabTTL)}
}
func (c *controlState) snapshot(now time.Time) []peer.AddrInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]peer.AddrInfo, 0, len(c.peers))
	for id, e := range c.peers {
		if !now.Before(e.expires) {
			delete(c.peers, id)
			continue
		}
		ai := e.info
		ai.Addrs = append([]ma.Multiaddr(nil), ai.Addrs...)
		out = append(out, ai)
	}
	return out
}

// ControlPeers returns only fresh, directly challenge-verified peers, not hints.
func (d *Discovery) ControlPeers() []peer.AddrInfo { return d.control.snapshot(time.Now()) }

// queueControl reserves capacity before spawning: no goroutine queue, no approval
// callbacks. Hint-driven followups cannot recursively fan out into more probes.
func (d *Discovery) queueControl(ai peer.AddrInfo) { d.queueControlMode(ai, true) }
func (d *Discovery) queueControlMode(ai peer.AddrInfo, expand bool) {
	if d == nil || d.host == nil || d.serviceCtx == nil || d.serviceCtx.Err() != nil || ai.ID == "" || ai.ID == d.host.ID() {
		return
	}
	if !d.control.acquire(ai.ID, time.Now()) {
		return
	}
	ai = controlClean(ai)
	go func() { defer d.control.release(ai.ID); _ = d.exchangeControl(d.serviceCtx, ai, expand) }()
}

type controlHint struct {
	ID    string   `json:"id"`
	Addrs []string `json:"addrs"`
}

func (d *Discovery) controlHints() []controlHint {
	infos := []peer.AddrInfo{{ID: d.host.ID(), Addrs: d.host.Addrs()}}
	// Copy members before consulting user-provided trust policy (no callbacks under locks).
	for _, m := range d.Peers() {
		if !d.trustPolicyPassive(m.PeerID, nil, "") {
			continue
		}
		id, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		ai := peer.AddrInfo{ID: id, Addrs: d.host.Peerstore().Addrs(id)}
		for _, text := range m.Addrs {
			if a, err := ma.NewMultiaddr(text); err == nil {
				ai.Addrs = append(ai.Addrs, a)
			}
		}
		infos = append(infos, ai)
	}
	infos = append(infos, d.ControlPeers()...)
	out := make([]controlHint, 0, controlCollabHints)
	seen := make(map[peer.ID]bool)
	for _, ai := range infos {
		if seen[ai.ID] {
			continue
		}
		ai = controlClean(ai)
		if len(ai.Addrs) == 0 {
			continue
		}
		seen[ai.ID] = true
		out = append(out, controlHint{ai.ID.String(), toStrings(ai.Addrs)})
		if len(out) == controlCollabHints {
			break
		}
	}
	return out
}
func (d *Discovery) acceptControlHints(payload []byte, expand bool) error {
	var hints []controlHint
	if err := json.Unmarshal(payload, &hints); err != nil {
		return err
	}
	if len(hints) > controlCollabHints {
		return errors.New("control: too many hints")
	}
	// Validate the whole envelope before any side effects.
	infos := make([]peer.AddrInfo, 0, len(hints))
	seen := make(map[peer.ID]bool)
	for _, h := range hints {
		if len(h.Addrs) > controlCollabAddrs {
			return errors.New("control: too many addresses")
		}
		id, err := peer.Decode(h.ID)
		if err != nil {
			return err
		}
		ai := peer.AddrInfo{ID: id}
		for _, text := range h.Addrs {
			if len(text) > 2048 {
				continue
			}
			if a, err := ma.NewMultiaddr(text); err == nil {
				ai.Addrs = append(ai.Addrs, a)
			}
		}
		ai = controlClean(ai)
		if id == d.host.ID() || seen[id] || len(ai.Addrs) == 0 {
			continue
		}
		seen[id] = true
		infos = append(infos, ai)
	}
	probes := 0
	for _, ai := range infos {
		d.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)
		if d.trustPolicyPassive(ai.ID.String(), nil, "") {
			// Scheduler is bounded/single-flight and performs an ordinary (non-request)
			// info exchange. Unlike Reconnect it does not promote hints to permanent addresses.
			d.scheduleIdentify(ai.ID)
		} else if expand && probes < 4 {
			probes++
			d.queueControlMode(ai, false)
		}
	}
	return nil
}

func (d *Discovery) exchangeControl(ctx context.Context, ai peer.AddrInfo, expand bool) error {
	ctx, cancel := context.WithTimeout(ctx, controlCollabTimeout)
	defer cancel()
	ai = controlClean(ai)
	d.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)
	s, err := d.host.NewStream(ctx, ai.ID, d.protoControl)
	if err != nil {
		return err
	}
	defer bindControlIO(ctx, s)()
	_ = s.SetDeadline(time.Now().Add(controlCollabTimeout))
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	if err = controlWrite(s, nonce); err != nil {
		return err
	}
	response, err := controlRead(s)
	if err != nil {
		return err
	}
	if len(response) != 64 {
		return errors.New("control: invalid challenge")
	}
	transcript := append(nonce, response[:32]...)
	want := d.controlMAC("responder-challenge", d.host.ID(), ai.ID, transcript, nil)
	if subtle.ConstantTimeCompare(response[32:], want[:]) != 1 {
		return errors.New("control: invalid challenge proof")
	}
	finish, err := d.controlPayload("requester-finish", d.host.ID(), ai.ID, transcript)
	if err != nil {
		return err
	}
	if err = controlWrite(s, finish); err != nil {
		return err
	}
	final, err := controlRead(s)
	if err != nil {
		return err
	}
	if err = d.verifyControlPayload(final, "responder-finish", d.host.ID(), ai.ID, transcript, expand); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	ai.Addrs = append(ai.Addrs, d.host.Peerstore().Addrs(ai.ID)...)
	d.control.remember(ai, time.Now())
	d.maybeRetirePublicDHT()
	return nil
}
func (d *Discovery) handleControl(s network.Stream) {
	id := s.Conn().RemotePeer()
	if d.serviceCtx == nil || d.serviceCtx.Err() != nil || !d.control.inbound.acquire(id, time.Now()) {
		_ = s.Reset()
		return
	}
	defer d.control.inbound.release(id)
	ctx, cancel := context.WithTimeout(d.serviceCtx, controlCollabTimeout)
	defer cancel()
	defer bindControlIO(ctx, s)()
	nonce, err := controlRead(s)
	if err != nil || len(nonce) != 32 {
		_ = s.Reset()
		return
	}
	responderNonce := make([]byte, 32)
	if _, err = rand.Read(responderNonce); err != nil {
		_ = s.Reset()
		return
	}
	transcript := append(nonce, responderNonce...)
	proof := d.controlMAC("responder-challenge", id, d.host.ID(), transcript, nil)
	if controlWrite(s, append(responderNonce, proof[:]...)) != nil {
		_ = s.Reset()
		return
	}
	finish, err := controlRead(s)
	if err != nil {
		_ = s.Reset()
		return
	}
	if d.verifyControlPayload(finish, "requester-finish", id, d.host.ID(), transcript, false) != nil || ctx.Err() != nil {
		_ = s.Reset()
		return
	}
	ai := peer.AddrInfo{ID: id, Addrs: append([]ma.Multiaddr{s.Conn().RemoteMultiaddr()}, d.host.Peerstore().Addrs(id)...)}
	d.control.remember(ai, time.Now())
	d.maybeRetirePublicDHT()
	final, err := d.controlPayload("responder-finish", id, d.host.ID(), transcript)
	if err != nil || controlWrite(s, final) != nil {
		_ = s.Reset()
	}
}
