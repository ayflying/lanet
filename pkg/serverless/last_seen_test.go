package serverless

import (
	"context"
	"testing"
	"time"
)

func TestPlaceholderNeverSeenAndFiniteDiscoveryGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, remote := testHost(t, false), testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "placeholder-last-seen", Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.scheduler.cancel() // No successful communication can race the placeholder assertions.
	d.addMember(remote.ID(), remote.Addrs(), "dht")
	m := d.members[remote.ID().String()]
	if m == nil || !m.LastSeen.IsZero() || m.LastDiscovered.IsZero() {
		t.Fatalf("discovery must create never-seen placeholder with TTL grace: %+v", m)
	}
	grace := m.LastDiscovered
	d.reapExpired()
	if len(d.members) != 1 {
		t.Fatal("fresh placeholder immediately reaped")
	}
	d.addMember(remote.ID(), remote.Addrs(), "pex")
	if !m.LastSeen.IsZero() || !m.LastDiscovered.Equal(grace) {
		t.Fatal("repeated discovery falsely renewed activity or grace")
	}
	m.LastDiscovered = time.Now().Add(-d.memberTTL - time.Second)
	d.reapExpired()
	if len(d.members) != 0 {
		t.Fatal("never-seen placeholder did not expire")
	}
}

func TestMemberFreshnessPrefersVerifiedActivity(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	m := Member{LastSeen: old, LastDiscovered: recent, FirstSeen: recent}
	if !m.freshness().Equal(old) {
		t.Fatal("discovery must not renew verified member TTL")
	}
	m.LastSeen = time.Time{}
	if !m.freshness().Equal(recent) {
		t.Fatal("placeholder lost discovery grace")
	}
	m.LastDiscovered = time.Time{}
	if !m.freshness().Equal(recent) {
		t.Fatal("legacy placeholder lost FirstSeen grace")
	}
}
