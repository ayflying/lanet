package peersdb

import (
	"context"
	"testing"
	"time"
)

func TestLastSeenOnlySuccessfulCommunication(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	id := "never-active"
	if err := d.UpsertPeer(ctx, Peer{PeerID: id}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTrusted(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetName(ctx, id, "display-name"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetNotes(ctx, id, "note"); err != nil {
		t.Fatal(err)
	}
	p, err := d.GetPeer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !p.LastSeen.IsZero() {
		t.Fatalf("non-communication created activity: %v", p.LastSeen)
	}
	active := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := d.UpsertPeer(ctx, Peer{PeerID: id, LastSeen: active}, nil); err != nil {
		t.Fatal(err)
	}
	for _, seen := range []time.Time{{}, active.Add(-time.Hour)} {
		if err := d.UpsertPeer(ctx, Peer{PeerID: id, LastSeen: seen}, nil); err != nil {
			t.Fatal(err)
		}
	}
	p, err = d.GetPeer(ctx, id)
	if err != nil || !p.LastSeen.Equal(active) {
		t.Fatalf("lost real activity: %v %+v", err, p)
	}
}
