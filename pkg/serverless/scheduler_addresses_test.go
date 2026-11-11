package serverless

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

func TestDiscoveryNewAddressWakeIsBounded(t *testing.T) {
	now := time.Now()
	s := newDiscoveryScheduler(context.Background(), time.Minute, func(context.Context, peer.ID) error { return errors.New("offline") })
	defer s.close()
	s.now = func() time.Time { return now }
	a := ma.StringCast("/ip4/192.168.1.20/tcp/4001")
	b := ma.StringCast("/ip4/192.168.1.21/tcp/4001")
	s.noteAddresses("p", []ma.Multiaddr{a})
	s.retry["p"] = func() discoveryRetry { r := s.retry["p"]; r.next = now.Add(5 * time.Minute); r.failures = 5; return r }()
	s.noteAddresses("p", []ma.Multiaddr{b})
	if s.retry["p"].generation != 0 {
		t.Fatal("address churn bypassed initial rate limit")
	}
	now = now.Add(discoveryAddressWakeInterval)
	s.noteAddresses("p", []ma.Multiaddr{a, b})
	r := s.retry["p"]
	if r.generation != 1 || r.next.After(now) || r.failures != 5 {
		t.Fatalf("new address did not get bounded wake: %+v", r)
	}
	s.noteAddresses("p", []ma.Multiaddr{b, a})
	s.noteAddresses("p", []ma.Multiaddr{b})
	if s.retry["p"].generation != 1 {
		t.Fatal("reorder/churn repeatedly bypassed budget")
	}
}

func TestDiscoveryOldAddressResultCannotOverwriteWake(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	now := time.Now()
	s := newDiscoveryScheduler(context.Background(), time.Minute, func(context.Context, peer.ID) error { close(entered); <-release; return errors.New("old offline") })
	defer s.close()
	s.now = func() time.Time { return now }
	a := ma.StringCast("/ip4/192.168.1.20/tcp/4001")
	b := ma.StringCast("/ip4/192.168.1.21/tcp/4001")
	s.noteAddresses("p", []ma.Multiaddr{a})
	c, err := s.submit("p", false)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	<-entered
	now = now.Add(discoveryAddressWakeInterval)
	s.noteAddresses("p", []ma.Multiaddr{b})
	close(release)
	<-c.done
	s.mu.Lock()
	r := s.retry["p"]
	s.mu.Unlock()
	if r.generation != 1 || r.next.After(now) || r.failures != 0 {
		t.Fatalf("old result overwrote new generation: %+v", r)
	}
}
