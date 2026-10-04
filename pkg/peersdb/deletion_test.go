package peersdb

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestPersistentDeletionBlocksPassiveWrites(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const id = "deleted"
	if err := db.UpsertPeer(ctx, Peer{PeerID: id}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPending(ctx, PendingRequest{PeerID: id}); err != nil {
		t.Fatal(err)
	}
	if err := db.RemovePeer(ctx, id); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := db.UpsertPeer(ctx, Peer{PeerID: id}, []string{"/ip4/1.2.3.4/tcp/4001"}); !errors.Is(err, ErrUnfriended) {
				t.Errorf("upsert: %v", err)
			}
			if err := db.SetTrusted(ctx, id, true); !errors.Is(err, ErrUnfriended) {
				t.Errorf("trust: %v", err)
			}
			if err := db.SetName(ctx, id, "stale member"); err != nil {
				t.Error(err)
			}
			if err := db.AddPending(ctx, PendingRequest{PeerID: id}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p, err := db.GetPeer(ctx, id); err != nil || p != nil {
		t.Fatalf("resurrected: %+v %v", p, err)
	}
	if pending, err := db.ListPending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending: %v %v", pending, err)
	}
	// Nearby visibility is only a manual re-add affordance, not authorization.
	if err := db.UpsertNearby(ctx, Nearby{PeerID: id}); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearUnfriended(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTrusted(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if trusted, err := db.IsTrusted(ctx, id); err != nil || !trusted {
		t.Fatalf("manual restoration: %v %v", trusted, err)
	}
}
