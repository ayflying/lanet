package p2pkit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

type testRelayACL struct{ reserve, connect bool }

type floodTestACL struct{ checks int }

func (a *floodTestACL) AllowReserve(p peer.ID, _ ma.Multiaddr) bool {
	a.checks++
	return p == "legitimate"
}
func (a *floodTestACL) AllowConnect(src peer.ID, _ ma.Multiaddr, _ peer.ID) bool {
	a.checks++
	return src == "legitimate"
}

func TestRelayUnauthenticatedFloodDoesNotSpendAuthorizedBudget(t *testing.T) {
	now := time.Now()
	acl := &floodTestACL{}
	a := newRelayAdmission(acl)
	a.now = func() time.Time { return now }
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1")
	for i := 0; i < 5000; i++ {
		if a.AllowConnect(peer.ID(fmt.Sprint(i)), addr, "victim") || a.AllowReserve(peer.ID(fmt.Sprint(i)), addr) {
			t.Fatal("unauthorized request accepted")
		}
	}
	if acl.checks != 1024 || len(a.requests) != 1024 {
		t.Fatalf("unbounded callback work: checks=%d requests=%d", acl.checks, len(a.requests))
	}
	if len(a.global.connect)+len(a.global.reserve)+len(a.global.leases)+len(a.peers)+len(a.ips) != 0 {
		t.Fatal("unauthorized flood consumed authorized quota/state")
	}
	// A short global gate can saturate under a live flood, but it must recover
	// in one second rather than poison legitimate hour-long traffic quotas.
	now = now.Add(time.Second)
	if !a.AllowConnect("legitimate", addr, "target") || !a.AllowReserve("legitimate", addr) {
		t.Fatal("legitimate peer blocked after unauthenticated gate expired")
	}
	if len(a.global.connect) != 1 || len(a.global.leases) != 1 || len(a.global.reserve) != 1 {
		t.Fatal("authorized accounting incorrect")
	}
}

func (a *testRelayACL) AllowReserve(peer.ID, ma.Multiaddr) bool          { return a.reserve }
func (a *testRelayACL) AllowConnect(peer.ID, ma.Multiaddr, peer.ID) bool { return a.connect }

func TestRelayAdmissionRenewalAndSeparateQuotas(t *testing.T) {
	now := time.Now()
	acl := &testRelayACL{reserve: true, connect: true}
	a := newRelayAdmission(acl)
	a.now = func() time.Time { return now }
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1")
	if !a.AllowReserve("p", addr) {
		t.Fatal("initial reserve")
	}
	acl.reserve = false
	if a.AllowReserve("p", addr) {
		t.Fatal("renewal cached expired authorization")
	}
	if !a.AllowConnect("p", addr, "q") {
		t.Fatal("reserve attempts affected CONNECT quota")
	}
	acl.connect = false
	if a.AllowConnect("p", addr, "q") {
		t.Fatal("CONNECT ignored caller ACL")
	}
}

func TestRelayAdmissionConcurrentPeerAndIPLimits(t *testing.T) {
	a := newRelayAdmission(nil)
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if a.AllowConnect(peer.ID(fmt.Sprint(i)), addr, peer.ID(fmt.Sprint(i+100))) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if allowed != 8 {
		t.Fatalf("same-IP allowed %d, want 8", allowed)
	}
}

func TestRelayAdmissionTrafficQuotaSurvivesReconnect(t *testing.T) {
	now := time.Now()
	a := newRelayAdmission(nil)
	a.now = func() time.Time { return now }
	addr := ma.StringCast("/ip4/127.0.0.1/tcp/1")
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 4; i++ {
			if !a.AllowConnect("p", addr, "q") {
				t.Fatalf("batch %d connect %d refused", batch, i)
			}
		}
		now = now.Add(relayCircuitLease + time.Second)
	}
	if a.AllowConnect("p", addr, "q") {
		t.Fatal("reconnect reset hourly budget")
	}
	now = now.Add(time.Hour)
	if !a.AllowConnect("p", addr, "q") {
		t.Fatal("expired quota not reclaimed")
	}
}

func TestRelayAdmissionGlobalRotationBudget(t *testing.T) {
	now := time.Now()
	a := newRelayAdmission(nil)
	a.now = func() time.Time { return now }
	for i := 0; i < 128; i++ {
		addr := ma.StringCast(fmt.Sprintf("/ip4/192.0.2.%d/tcp/1", i+1))
		if !a.AllowConnect(peer.ID(fmt.Sprint(i)), addr, peer.ID(fmt.Sprint(i+1000))) {
			t.Fatalf("connect %d", i)
		}
		if i%32 == 31 {
			now = now.Add(relayCircuitLease + time.Second)
		}
	}
	// Keep all admissions within an hour (4 * 13m), while leases age out.
	if a.AllowConnect("new-src", ma.StringCast("/ip4/198.51.100.1/tcp/1"), "new-dst") {
		t.Fatal("ID rotation bypassed global hourly quota")
	}
}

func TestRelayAdmissionStateBoundedAndIPv6Rotation(t *testing.T) {
	now := time.Now()
	a := newRelayAdmission(nil)
	a.now = func() time.Time { return now }
	for i := 0; i < relayAdmissionMaxPeers; i++ {
		a.peers[peer.ID(fmt.Sprint(i))] = &relayAdmissionState{connect: []time.Time{now}}
	}
	if a.AllowReserve("extra", ma.StringCast("/ip4/127.0.0.1/tcp/1")) {
		t.Fatal("peer state table overflow")
	}
	if len(a.peers) != relayAdmissionMaxPeers {
		t.Fatal("live quota state evicted")
	}
	a = newRelayAdmission(nil)
	for i := 0; i < relayAdmissionMaxIPs; i++ {
		ip := relayIPKey(ma.StringCast(fmt.Sprintf("/ip4/192.0.%d.%d/tcp/1", i/256, i%256)))
		a.ips[ip] = &relayAdmissionState{connect: []time.Time{now}}
	}
	if a.AllowReserve("extra", ma.StringCast("/ip4/198.51.100.1/tcp/1")) {
		t.Fatal("IP state table overflow")
	}
	if relayIPKey(ma.StringCast("/ip6/2001:db8::1/tcp/1")) != relayIPKey(ma.StringCast("/ip6/2001:db8::ffff/tcp/1")) {
		t.Fatal("IPv6 interface rotation bypass")
	}
}

func TestRelayACLAllHostEntryPoints(t *testing.T) {
	for _, mode := range []string{"normal", "always", "dedicated", "combined"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			acl := &testRelayACL{reserve: true}
			spec := HostSpec{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, RelayACL: acl}
			switch mode {
			case "normal":
				spec.RelayService = true
			case "always":
				spec.RelayServiceAlways = true
			case "dedicated":
				spec.RelayServiceDedicated = true
			case "combined":
				spec.RelayService = true
				spec.RelayServiceAlways = true
			}
			h, err := NewHost(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			if mode == "normal" {
				emitter, err := h.EventBus().Emitter(new(event.EvtLocalReachabilityChanged))
				if err != nil {
					t.Fatal(err)
				}
				defer emitter.Close()
				if err := emitter.Emit(event.EvtLocalReachabilityChanged{Reachability: network.ReachabilityPublic}); err != nil {
					t.Fatal(err)
				}
			}
			client, err := NewHost(ctx, HostSpec{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			// First succeed to prove that refusal below is the ACL, not an
			// asynchronously absent hop handler or unrelated resource failure.
			ready := time.NewTicker(10 * time.Millisecond)
			defer ready.Stop()
			for {
				found := false
				for _, p := range h.Mux().Protocols() {
					if p == "/libp2p/circuit/relay/0.2.0/hop" {
						found = true
					}
				}
				if found {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("hop service not started")
				case <-ready.C:
				}
			}
			if _, err = relayclient.Reserve(ctx, client, AddrInfo(h)); err != nil {
				t.Fatal(err)
			}
			acl.reserve = false
			if _, err = relayclient.Reserve(ctx, client, AddrInfo(h)); err == nil {
				t.Fatal("denied ACL accepted reservation")
			}
		})
	}
}
