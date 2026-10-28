package serverless

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	lproto "github.com/ayflying/pvn/pkg/protocol"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

func publicControlDiscovery(t *testing.T, h host.Host, key string) *Discovery {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &Discovery{
		host: h, groupKey: GroupKey("", key), serviceCtx: ctx, serviceCancel: cancel,
		memberTTL: time.Minute, members: make(map[string]*Member),
		cfg: Config{IsTrusted: func(string) bool { return false }},
	}
	d.protoControl = lproto.GroupProtoID("control", d.groupKey)
	h.SetStreamHandler(d.protoControl, d.handleControl)
	t.Cleanup(cancel)
	return d
}

func publicControlDHTs(t *testing.T, d *Discovery) {
	t.Helper()
	for _, private := range []bool{false, true} {
		prefix := libprotocol.ID("/public-control-test/public")
		if private {
			prefix = PrivateDHTPrefix
		}
		kad, err := kaddht.New(d.host,
			kaddht.ProtocolPrefix(prefix), kaddht.Mode(kaddht.ModeServer), kaddht.DisableAutoRefresh())
		if err != nil {
			t.Fatal(err)
		}
		if private {
			d.dhtPrivate.Store(kad)
		} else {
			d.dhtPublic.Store(kad)
		}
	}
	t.Cleanup(func() {
		// A retired instance has already been closed; only own those still installed.
		if kad := d.dhtPublic.Swap(nil); kad != nil {
			_ = kad.Close()
		}
		if kad := d.dhtPrivate.Swap(nil); kad != nil {
			_ = kad.Close()
		}
	})
}

func publicControlWaitRetired(t *testing.T, d *Discovery, want bool) {
	t.Helper()
	deadline := time.NewTimer(200 * time.Millisecond)
	if want {
		deadline.Stop()
		deadline.Reset(3 * time.Second)
	}
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		state := d.PublicDHTState()
		if state.Retired {
			if !want {
				t.Fatal("unverified, stale, or disconnected peer retired public DHT")
			}
			if state.Active {
				t.Fatal("retired public DHT is still active")
			}
			return
		}
		select {
		case <-deadline.C:
			if want {
				t.Fatal("connected verified peer did not retire public DHT")
			}
			if !state.Active {
				t.Fatal("public DHT disappeared without retirement")
			}
			return
		case <-tick.C:
		}
	}
}

// The default private routing table is shared across groups. Neither RT presence,
// a failed proof, a stale proof, nor a cached but disconnected peer is readiness.
func TestPublicControlRetirementRequiresConnectedFreshEvidence(t *testing.T) {
	for _, name := range []string{"cross-group-rt", "control", "wrong-proof", "expired-control", "disconnected-control", "member", "expired-member", "disconnected-member", "member-and-control-deduplicate"} {
		t.Run(name, func(t *testing.T) {
			a, b := testHost(t, false), testHost(t, false)
			da := publicControlDiscovery(t, a, "public-retire-a")
			keyB := "public-retire-a"
			if name == "cross-group-rt" || name == "wrong-proof" {
				keyB = "public-retire-other-group"
			}
			db := publicControlDiscovery(t, b, keyB)
			da.memberTTL = time.Minute
			publicControlDHTs(t, da)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ai := peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}
			if err := a.Connect(ctx, ai); err != nil {
				t.Fatal(err)
			}
			added, err := da.dhtPrivate.Load().RoutingTable().TryAddPeer(b.ID(), false, false)
			if err != nil || !added {
				t.Fatalf("populate shared private RT: added=%v err=%v", added, err)
			}
			want := false
			switch name {
			case "control", "member-and-control-deduplicate":
				if err := da.exchangeControl(ctx, ai, false); err != nil {
					t.Fatal(err)
				}
				if len(da.ControlPeers()) != 1 || len(da.Peers()) != 0 {
					t.Fatal("control proof must not grant business membership")
				}
				want = true
			case "wrong-proof":
				// Negotiate the same stream ID to exercise proof validation, not just
				// the different-group multistream rejection.
				b.SetStreamHandler(da.protoControl, db.handleControl)
				if err := da.exchangeControl(ctx, ai, false); err == nil {
					t.Fatal("wrong group proof unexpectedly succeeded")
				}
				if len(da.ControlPeers()) != 0 {
					t.Fatal("failed proof entered verified cache")
				}
			case "expired-control":
				da.control.remember(ai, time.Now().Add(-controlCollabTTL-time.Second))
			case "disconnected-control":
				da.control.remember(ai, time.Now())
			case "member":
				want = true
			}
			if name == "member" || name == "expired-member" || name == "disconnected-member" || name == "member-and-control-deduplicate" {
				lastSeen := time.Now()
				if name == "expired-member" {
					lastSeen = lastSeen.Add(-2 * da.memberTTL)
				}
				da.mu.Lock()
				da.members[b.ID().String()] = &Member{PeerID: b.ID().String(), LastSeen: lastSeen}
				da.mu.Unlock()
			}
			if strings.HasPrefix(name, "disconnected-") {
				if err := a.Network().ClosePeer(b.ID()); err != nil {
					t.Fatal(err)
				}
				if len(a.Network().ConnsToPeer(b.ID())) != 0 {
					t.Fatal("test peer is still connected")
				}
			}
			da.maybeRetirePublicDHT()
			publicControlWaitRetired(t, da, want)
			if name == "member-and-control-deduplicate" && !strings.Contains(da.PublicDHTState().Reason, "1 个") {
				t.Fatalf("same peer counted twice: %q", da.PublicDHTState().Reason)
			}
		})
	}
}

func TestPublicControlRetirePreservesNonmemberUnderlayAndBusinessStream(t *testing.T) {
	a, b := testHost(t, false), testHost(t, false)
	d := publicControlDiscovery(t, a, "public-underlay")
	publicControlDHTs(t, d)
	const echo = libprotocol.ID("/public-control-test/business/1")
	b.SetStreamHandler(echo, func(s network.Stream) {
		defer s.Close()
		_, _ = io.Copy(s, s)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatal(err)
	}
	s, err := a.NewStream(ctx, b.ID(), echo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	roundtrip := func(text string) {
		t.Helper()
		if _, err := s.Write([]byte(text)); err != nil {
			t.Fatalf("existing business stream write: %v", err)
		}
		got := make([]byte, len(text))
		if _, err := io.ReadFull(s, got); err != nil || string(got) != text {
			t.Fatalf("existing business stream read: got=%q err=%v", got, err)
		}
	}
	roundtrip("before")
	if _, err := d.dhtPrivate.Load().RoutingTable().TryAddPeer(b.ID(), false, false); err != nil {
		t.Fatal(err)
	}
	if len(d.Peers()) != 0 {
		t.Fatal("test remote must be a nonmember")
	}
	d.retirePublicDHT("test shared underlay")
	if d.dhtPublic.Load() != nil || !d.PublicDHTState().Retired {
		t.Fatal("public DHT did not retire")
	}
	if a.Network().Connectedness(b.ID()) != network.Connected || s.Conn().IsClosed() {
		t.Fatal("retirement closed nonmember shared underlay")
	}
	if d.dhtPrivate.Load() == nil {
		t.Fatal("retirement stopped private DHT")
	}
	roundtrip("after")
	// Repeated calls cannot acquire or close any shared connection either.
	d.retirePublicDHT("again")
	roundtrip("still alive")
}
