package serverless

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestPendingExplicitHandshake uses real info streams between two nodes: an
// ordinary/background handshake is not a connection application, even when
// AutoAccept is enabled. Only the explicit marker permits approval callbacks.
func TestPendingExplicitHandshake(t *testing.T) {
	for _, autoAccept := range []bool{false, true} {
		name := "manual_approval"
		if autoAccept {
			name = "auto_accept"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ha := testHost(t, false)
			hb := testHost(t, false)

			var mu sync.Mutex
			var trusted bool
			var autoCalls, pendingCalls int
			var pendingPeer, pendingName string
			da, err := New(ctx, ha, Config{
				NetworkKey:    "grp-pending-explicit-" + name,
				Name:          "receiver",
				HasKnownPeers: func() bool { return false },
				IsTrusted: func(id string) bool {
					mu.Lock()
					defer mu.Unlock()
					return id == hb.ID().String() && trusted
				},
				AutoAccept: func(id string, _ []string, _ string) bool {
					mu.Lock()
					defer mu.Unlock()
					autoCalls++
					if autoAccept && id == hb.ID().String() {
						trusted = true // Persist acceptance as the real address book does.
						return true
					}
					return false
				},
				OnPending: func(id string, _ []string, nodeName string) {
					mu.Lock()
					defer mu.Unlock()
					pendingCalls++
					pendingPeer, pendingName = id, nodeName
				},
			})
			if err != nil {
				t.Fatalf("new A: %v", err)
			}
			db, err := New(ctx, hb, Config{
				NetworkKey:    "grp-pending-explicit-" + name,
				Name:          "applicant",
				HasKnownPeers: func() bool { return false },
			})
			if err != nil {
				t.Fatalf("new B: %v", err)
			}
			if err = da.Start(ctx); err != nil {
				t.Fatalf("start A: %v", err)
			}
			if err = db.Start(ctx); err != nil {
				t.Fatalf("start B: %v", err)
			}
			if err = hb.Connect(ctx, peer.AddrInfo{ID: ha.ID(), Addrs: ha.Addrs()}); err != nil {
				t.Fatalf("connect: %v", err)
			}

			// Repeat unmarked handshakes to model periodic identity/reconnect work.
			// A response synchronizes each assertion with the remote stream handler.
			for i := 0; i < 3; i++ {
				resp, err := db.fetchInfo(ctx, ha.ID())
				if err != nil {
					t.Fatalf("ordinary handshake %d: %v", i, err)
				}
				if resp.Rejected != rejectionNotApproved {
					t.Fatalf("ordinary handshake must reject untrusted peer: %+v", resp)
				}
				if resp.Name != "" || resp.Version != "" || resp.Platform != "" || resp.OSHostname != "" || len(resp.LocalIPs) != 0 || len(resp.Members) != 0 {
					t.Fatalf("ordinary rejection leaked identity or members: %+v", resp)
				}
				mu.Lock()
				auto, pending, isTrusted := autoCalls, pendingCalls, trusted
				mu.Unlock()
				if auto != 0 || pending != 0 || isTrusted {
					t.Fatalf("ordinary handshake caused approval side effects: auto=%d pending=%d trusted=%v", auto, pending, isTrusted)
				}
				if _, ok := memberOf(da, hb.ID().String()); ok {
					t.Fatal("ordinary untrusted handshake must not enter receiver members")
				}
			}

			resp, err := db.fetchInfo(context.WithValue(ctx, connectRequestKey{}, true), ha.ID())
			if err != nil {
				t.Fatalf("explicit handshake: %v", err)
			}
			mu.Lock()
			auto, pending, isTrusted := autoCalls, pendingCalls, trusted
			gotPeer, gotName := pendingPeer, pendingName
			mu.Unlock()
			if auto != 1 {
				t.Fatalf("explicit handshake must invoke AutoAccept once, got %d", auto)
			}
			if autoAccept {
				if !isTrusted || pending != 0 || resp.Rejected != "" || resp.Name != "receiver" {
					t.Fatalf("explicit auto-accept must trust without pending: trusted=%v pending=%d response=%+v", isTrusted, pending, resp)
				}
				if _, ok := memberOf(da, hb.ID().String()); !ok {
					t.Fatal("explicit auto-accepted peer must enter receiver members")
				}
				// Once explicitly accepted, ordinary handshakes use existing trust.
				if resp, err = db.fetchInfo(ctx, ha.ID()); err != nil || resp.Rejected != "" {
					t.Fatalf("accepted peer's ordinary handshake: response=%+v error=%v", resp, err)
				}
				mu.Lock()
				auto, pending = autoCalls, pendingCalls
				mu.Unlock()
				if auto != 1 || pending != 0 {
					t.Fatalf("trusted ordinary handshake must not re-run approval: auto=%d pending=%d", auto, pending)
				}
			} else {
				if isTrusted || pending != 1 || gotPeer != hb.ID().String() || gotName != "applicant" || resp.Rejected != rejectionNotApproved {
					t.Fatalf("explicit request must become pending: trusted=%v pending=%d peer=%q name=%q response=%+v", isTrusted, pending, gotPeer, gotName, resp)
				}
				if _, ok := memberOf(da, hb.ID().String()); ok {
					t.Fatal("pending applicant must not enter receiver members")
				}
			}
		})
	}
}
