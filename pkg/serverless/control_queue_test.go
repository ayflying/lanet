package serverless

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestControlQueueSimultaneousDiscovery(t *testing.T) {
	for _, mode := range []string{"queue", "addMember", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			a, b := testHost(t, false), testHost(t, false)
			var pending, auto, inbound atomic.Int32
			var trusted atomic.Bool
			trusted.Store(mode == "revoked")
			cfg := Config{
				IsTrusted:  func(string) bool { return trusted.Load() },
				OnPending:  func(string, []string, string) { pending.Add(1) },
				AutoAccept: func(string, []string, string) bool { auto.Add(1); return true },
			}
			da := controlTestDiscovery(t, a, "control-queue-"+mode, cfg)
			db := controlTestDiscovery(t, b, "control-queue-"+mode, cfg)
			// Establish transport only, not a business or control handshake.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
				t.Fatal(err)
			}
			// Hold both inbound streams until BOTH real queue calls have synchronously
			// reserved their outgoing slots. This deterministically reproduces the
			// shared-gate collision, independent of network or goroutine timing.
			start := make(chan struct{})
			for _, d := range []*Discovery{da, db} {
				d.host.SetStreamHandler(d.protoControl, func(s network.Stream) {
					select {
					case <-start:
						inbound.Add(1)
						d.handleControl(s)
					case <-ctx.Done():
						_ = s.Reset()
					}
				})
			}
			// Revocation must not disable group-key cooperation or let it restore
			// business authorization. No member state is manually seeded.
			trusted.Store(false)
			if mode == "queue" {
				da.queueControl(peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()})
				db.queueControl(peer.AddrInfo{ID: a.ID(), Addrs: a.Addrs()})
			} else {
				if da.addMember(b.ID(), b.Addrs(), "dht") || db.addMember(a.ID(), a.Addrs(), "mdns") {
					t.Fatal("passive discovery inserted an unauthorized member")
				}
			}
			for _, d := range []*Discovery{da, db} {
				d.control.controlRate.mu.Lock()
				active := d.control.active
				d.control.controlRate.mu.Unlock()
				if active != 1 {
					t.Fatalf("outbound reservation not acquired before handler: %d", active)
				}
			}
			close(start)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				done := true
				for _, d := range []*Discovery{da, db} {
					d.control.controlRate.mu.Lock()
					outActive := d.control.active
					d.control.controlRate.mu.Unlock()
					d.control.inbound.mu.Lock()
					inActive := d.control.inbound.active
					d.control.inbound.mu.Unlock()
					done = done && outActive == 0 && inActive == 0 && len(d.ControlPeers()) == 1
				}
				if done {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("same-round probes failed: A cache=%v B cache=%v inbound=%d", da.ControlPeers(), db.ControlPeers(), inbound.Load())
				case <-ticker.C:
				}
			}
			if da.ControlPeers()[0].ID != b.ID() || db.ControlPeers()[0].ID != a.ID() || inbound.Load() != 2 {
				t.Fatal("both independently authenticated probes must populate the opposite cache")
			}
			// Successful outgoing attempts retain their own cooldown; a new passive
			// discovery cannot launch another probe or trigger approval callbacks.
			da.addMember(b.ID(), b.Addrs(), "dht")
			db.addMember(a.ID(), a.Addrs(), "mdns")
			if mode == "revoked" {
				resp, err := da.fetchInfo(ctx, b.ID())
				if err != nil || resp.Rejected != rejectionNotApproved {
					t.Fatalf("revoked peer regained business access: response=%+v error=%v", resp, err)
				}
			}
			if len(da.Peers()) != 0 || len(db.Peers()) != 0 || pending.Load() != 0 || auto.Load() != 0 {
				t.Fatalf("control changed business state: A=%v B=%v pending=%d auto=%d", da.Peers(), db.Peers(), pending.Load(), auto.Load())
			}
			if inbound.Load() != 2 {
				t.Fatal("outbound cooldown did not suppress repeated discovery")
			}
		})
	}
}

func TestControlDirectionalRateBounds(t *testing.T) {
	now := time.Now()
	var c controlState
	for i := 0; i < controlCollabConcurrent; i++ {
		id := peer.ID(string(rune(i + 1)))
		if !c.acquire(id, now) || !c.inbound.acquire(id, now) {
			t.Fatal("inbound/outbound capacity or same-peer reservations interfered")
		}
	}
	if c.acquire("extra", now) || c.inbound.acquire("extra", now) {
		t.Fatal("directional concurrency exceeded eight")
	}
	id := peer.ID(string(rune(1)))
	c.release(id)
	if c.acquire(id, now) || c.inbound.acquire(id, now.Add(controlCollabCooldown)) {
		t.Fatal("outbound cooldown or inbound active reservation ignored")
	}
	if !c.acquire(id, now.Add(controlCollabCooldown)) {
		t.Fatal("inbound activity blocked expired outbound cooldown")
	}
	c.inbound.release(id)
	if c.inbound.acquire(id, now) || !c.inbound.acquire(id, now.Add(controlCollabCooldown)) {
		t.Fatal("inbound cooldown boundary incorrect")
	}
	// Both maps stay bounded even when all slots are quickly released; pruning
	// an expired cooldown makes room without changing the separate peer cache.
	for _, rate := range []*controlRate{&c.controlRate, &c.inbound} {
		*rate = controlRate{}
		for i := 0; i < controlCollabCandidates; i++ {
			id := peer.ID(string(rune(i + 1)))
			if !rate.acquire(id, now) {
				t.Fatalf("capacity lost at %d", i)
			}
			rate.release(id)
		}
		if rate.acquire("extra", now) || len(rate.attempts) != controlCollabCandidates {
			t.Fatal("directional cooldown map exceeded bound")
		}
		if !rate.acquire("extra", now.Add(controlCollabCooldown)) || len(rate.attempts) != 1 {
			t.Fatal("expired directional cooldowns not reclaimed")
		}
	}
	if len(c.snapshot(now)) != 0 {
		t.Fatal("rate limiter allocated control peers")
	}
}
