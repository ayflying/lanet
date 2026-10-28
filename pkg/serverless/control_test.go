package serverless

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	lproto "github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

func controlTestDiscovery(t *testing.T, h host.Host, key string, cfg Config) *Discovery {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &Discovery{host: h, cfg: cfg, groupKey: GroupKey("", key), serviceCtx: ctx, serviceCancel: cancel, members: make(map[string]*Member), unfriendedAt: make(map[string]time.Time)}
	d.pexLast = make(map[string]time.Time)
	d.nearbySeen = make(map[string]time.Time)
	d.protoControl = lproto.GroupProtoID("control", d.groupKey)
	d.protoInfo = lproto.GroupProtoID(lproto.BaseInfo, d.groupKey)
	d.selfIP = DeriveVirtualIP(d.groupKey, h.ID().String())
	d.selfIPv6 = DeriveVirtualIPv6(d.groupKey, h.ID().String())
	d.scheduler = newDiscoveryScheduler(ctx, time.Minute, d.connectAndIdentifyContext)
	h.SetStreamHandler(d.protoControl, d.handleControl)
	h.SetStreamHandler(d.protoInfo, d.handleInfo)
	t.Cleanup(func() { cancel(); d.scheduler.close() })
	return d
}

func TestControlThreeNodesRestoresOnlyAuthorizedInfo(t *testing.T) {
	a, b, c := testHost(t, false), testHost(t, false), testHost(t, false)
	var pending, auto atomic.Int32
	var revoked atomic.Bool
	cfg := func(trusted peer.ID) Config {
		return Config{Name: "friend", IsTrusted: func(id string) bool { return !revoked.Load() && id == trusted.String() }, OnPending: func(string, []string, string) { pending.Add(1) }, AutoAccept: func(string, []string, string) bool { auto.Add(1); return true }}
	}
	da := controlTestDiscovery(t, a, "control-three", cfg(b.ID()))
	db := controlTestDiscovery(t, b, "control-three", cfg(a.ID()))
	dc := controlTestDiscovery(t, c, "control-three", cfg(""))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	eventually := func(description string, ready func() bool) {
		t.Helper()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: A members=%+v A control=%+v C control=%+v", description, da.Peers(), da.ControlPeers(), dc.ControlPeers())
			case <-ticker.C:
			}
		}
	}
	// Passive discovery must run C's background control exchange with B, without
	// creating a friendship. Wait for the verified cache before using C as a hint source.
	if dc.addMember(b.ID(), b.Addrs(), "dht") {
		t.Fatal("C inserted unauthorized B as a member")
	}
	eventually("C must have verified B", func() bool {
		for _, ai := range dc.ControlPeers() {
			if ai.ID == b.ID() {
				return true
			}
		}
		return false
	})
	// A has no B address. Discovering only unauthorized C must recover B through
	// the queued control exchange and its automatic background info scheduling.
	a.Peerstore().ClearAddrs(b.ID())
	if da.addMember(c.ID(), c.Addrs(), "mdns") {
		t.Fatal("A inserted unauthorized C as a member")
	}
	eventually("B info not restored", func() bool {
		for _, m := range da.Peers() {
			if m.PeerID == b.ID().String() && m.Name == "friend" {
				return true
			}
		}
		return false
	})
	eventually("A must have verified C", func() bool {
		for _, ai := range da.ControlPeers() {
			if ai.ID == c.ID() {
				return true
			}
		}
		return false
	})
	for _, m := range da.Peers() {
		if m.PeerID == c.ID().String() {
			t.Fatal("C became A member")
		}
	}
	for _, d := range []*Discovery{db, dc} {
		for _, m := range d.Peers() {
			if d == dc || m.PeerID == c.ID().String() {
				t.Fatalf("untrusted C entered members: %+v", d.Peers())
			}
		}
	}
	// Revoke business authorization while leaving verified transport cooperation alive.
	// The same C may still provide addresses, but cannot override a receiver's ACL.
	revoked.Store(true)
	// Rediscovery uses the normal queue and may be suppressed by its cooldown;
	// do not wait for or bypass the 30-second limit to refresh an already verified C.
	if da.addMember(c.ID(), c.Addrs(), "mdns") {
		t.Fatal("revocation allowed C into members")
	}
	foundC := false
	for _, ai := range da.ControlPeers() {
		foundC = foundC || ai.ID == c.ID()
	}
	if !foundC {
		t.Fatal("business revocation removed independent cooperation state")
	}
	resp, err := da.fetchInfo(ctx, b.ID())
	if err != nil || resp.Rejected != rejectionNotApproved {
		t.Fatalf("revoked business peer accepted: response=%+v error=%v", resp, err)
	}
	if pending.Load() != 0 || auto.Load() != 0 {
		t.Fatalf("control caused approval effects: pending=%d auto=%d", pending.Load(), auto.Load())
	}
}

func TestControlTTLBoundsAndSnapshotIsolation(t *testing.T) {
	now := time.Now()
	var c controlState
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1234")
	c.remember(peer.AddrInfo{ID: "one", Addrs: []ma.Multiaddr{addr}}, now)
	got := c.snapshot(now.Add(controlCollabTTL - time.Nanosecond))
	if len(got) != 1 {
		t.Fatal("expired early")
	}
	got[0].Addrs[0] = nil
	if c.snapshot(now)[0].Addrs[0] == nil {
		t.Fatal("snapshot aliases cache")
	}
	if len(c.snapshot(now.Add(controlCollabTTL))) != 0 {
		t.Fatal("TTL equality must expire")
	}
	for i := 0; i < controlCollabCandidates+1; i++ {
		c.remember(peer.AddrInfo{ID: peer.ID(string(rune(i + 1)))}, now)
	}
	if len(c.snapshot(now)) != controlCollabCandidates {
		t.Fatal("cache unbounded")
	}
	var gate controlState
	for i := 0; i < controlCollabConcurrent; i++ {
		if !gate.acquire(peer.ID(string(rune(i+1))), now) {
			t.Fatal("capacity lost")
		}
	}
	if gate.acquire("extra", now) {
		t.Fatal("concurrency cap ignored")
	}
	gate.release(peer.ID(string(rune(1))))
	if gate.acquire(peer.ID(string(rune(1))), now) {
		t.Fatal("cooldown ignored")
	}
	if !gate.acquire(peer.ID(string(rune(1))), now.Add(controlCollabCooldown)) {
		t.Fatal("cooldown equality should allow")
	}
}

func TestControlMessageBoundAndTamper(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], controlCollabMessage+1)
	if _, err := controlRead(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversize accepted")
	}
	if err := controlWrite(io.Discard, make([]byte, controlCollabMessage+1)); err == nil {
		t.Fatal("oversize emitted")
	}
	h, other := testHost(t, false), testHost(t, false)
	d := controlTestDiscovery(t, h, "control-mac", Config{IsTrusted: func(string) bool { return false }})
	transcript := make([]byte, 64)
	_, _ = rand.Read(transcript)
	payload := []byte(`[]`)
	proof := d.controlMAC("requester-finish", other.ID(), h.ID(), transcript, payload)
	body := append(proof[:], payload...)
	if err := d.verifyControlPayload(body, "requester-finish", other.ID(), h.ID(), transcript, false); err != nil {
		t.Fatal(err)
	}
	body[len(body)-1] ^= 1
	if err := d.verifyControlPayload(body, "requester-finish", other.ID(), h.ID(), transcript, false); err == nil {
		t.Fatal("tamper accepted")
	}
	body[len(body)-1] ^= 1
	if err := d.verifyControlPayload(body, "responder-finish", other.ID(), h.ID(), transcript, false); err == nil {
		t.Fatal("reflected role accepted")
	}
	if err := d.verifyControlPayload(body, "requester-finish", h.ID(), other.ID(), transcript, false); err == nil {
		t.Fatal("swapped peer IDs accepted")
	}
	transcript[0] ^= 1
	if err := d.verifyControlPayload(body, "requester-finish", other.ID(), h.ID(), transcript, false); err == nil {
		t.Fatal("replayed transcript accepted")
	}
}

func TestControlRejectsBadKeyAndRequesterProof(t *testing.T) {
	for _, badKey := range []bool{true, false} {
		t.Run(map[bool]string{true: "bad-key", false: "bad-proof"}[badKey], func(t *testing.T) {
			a, b := testHost(t, false), testHost(t, false)
			var effects atomic.Int32
			cfg := Config{IsTrusted: func(string) bool { return false }, OnPending: func(string, []string, string) { effects.Add(1) }}
			da := controlTestDiscovery(t, a, "control-good", cfg)
			db := controlTestDiscovery(t, b, "control-good", cfg)
			if badKey {
				da.groupKey = GroupKey("", "control-wrong")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			if badKey {
				if err := da.exchangeControl(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}, false); err == nil {
					t.Fatal("bad key accepted")
				}
			} else {
				if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
					t.Fatal(err)
				}
				s, err := a.NewStream(ctx, b.ID(), db.protoControl)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				_ = s.SetDeadline(time.Now().Add(controlCollabTimeout))
				if err = controlWrite(s, make([]byte, 32)); err != nil {
					t.Fatal(err)
				}
				challenge, err := controlRead(s)
				if err != nil || len(challenge) != 64 {
					t.Fatalf("challenge: %x %v", challenge, err)
				}
				if err = controlWrite(s, append(make([]byte, 32), []byte(`[]`)...)); err != nil {
					t.Fatal(err)
				}
				if body, err := controlRead(s); err == nil {
					t.Fatalf("bad requester got hints: %s", body)
				}
			}
			if len(da.ControlPeers()) != 0 || len(db.ControlPeers()) != 0 || len(da.Peers()) != 0 || len(db.Peers()) != 0 || effects.Load() != 0 {
				t.Fatal("invalid auth had side effects")
			}
		})
	}
}

func TestControlHintsCleanAndAtomicValidation(t *testing.T) {
	a, b := testHost(t, false), testHost(t, false)
	d := controlTestDiscovery(t, a, "control-clean", Config{IsTrusted: func(string) bool { return false }})
	hints := []controlHint{{ID: b.ID().String(), Addrs: []string{"/ip4/0.0.0.0/tcp/0", "not-an-address"}}, {ID: "invalid", Addrs: []string{"/ip4/127.0.0.1/tcp/3210"}}}
	raw, _ := json.Marshal(hints)
	if err := d.acceptControlHints(raw, false); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if len(a.Peerstore().Addrs(b.ID())) != 0 {
		t.Fatal("partially applied malformed envelope")
	}
	hints = []controlHint{{ID: b.ID().String(), Addrs: []string{"/ip4/0.0.0.0/tcp/0", "/ip4/127.0.0.1/tcp/3210"}}}
	raw, _ = json.Marshal(hints)
	if err := d.acceptControlHints(raw, false); err != nil {
		t.Fatal(err)
	}
	if len(a.Peerstore().Addrs(b.ID())) != 1 {
		t.Fatalf("bad cleaning: %v", a.Peerstore().Addrs(b.ID()))
	}
	if len(d.ControlPeers()) != 0 || len(d.Peers()) != 0 {
		t.Fatal("hint became verified or authorized")
	}
}
