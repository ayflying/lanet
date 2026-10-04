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
	// 删除不是拉黑：被删节点的重新申请会进入待审批（是否恢复由用户显式决定），
	// 但申请记录本身不赋予信任——peers 行仍不得复活、SetTrusted 仍被挡。
	if pending, err := db.ListPending(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %v", pending, err)
	} else if pending[0].PeerID != id {
		t.Fatalf("pending peer: %s", pending[0].PeerID)
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

func TestUnfriendedNotifiedLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const id = "notified"
	if err := db.RemovePeer(ctx, id); err != nil {
		t.Fatal(err)
	}
	// 全新删除：未告知。
	if notified, err := db.IsUnfriendedNotified(ctx, id); err != nil || notified {
		t.Fatalf("fresh tombstone notified: %v %v", notified, err)
	}
	if err := db.MarkUnfriendedNotified(ctx, id); err != nil {
		t.Fatal(err)
	}
	if notified, err := db.IsUnfriendedNotified(ctx, id); err != nil || !notified {
		t.Fatalf("marked tombstone not notified: %v %v", notified, err)
	}
	// 再次删除：告知状态重置，新一次删除重新走一遍告知。
	if err := db.RemovePeer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if notified, err := db.IsUnfriendedNotified(ctx, id); err != nil || notified {
		t.Fatalf("re-deleted tombstone kept notified: %v %v", notified, err)
	}
	// 未知节点：不是墓碑，查询应报错（与 IsUnfriended 的 no-row 语义区分）。
	if _, err := db.IsUnfriendedNotified(ctx, "nobody"); err == nil {
		t.Fatal("unknown peer must error")
	}
}
