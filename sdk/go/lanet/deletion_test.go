package lanet

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ayflying/pvn/pkg/peersdb"
)

func TestDeletedPeerOverridesAutoAccept(t *testing.T) {
	for _, approval := range []bool{true, false} {
		t.Run(map[bool]string{true: "approval", false: "no-approval"}[approval], func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "peers.db")
			db, err := peersdb.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			c := &Client{cfg: Config{AutoAccept: true, RequireApproval: &approval}, peers: db}
			const id = "never-connected-unnamed"
			if !c.maybeAutoAccept(id, nil, "") {
				t.Fatal("initial autoaccept failed")
			}
			if err := c.RemovePeer(id); err != nil {
				t.Fatal(err)
			}
			c.onSeenUntrusted(id, nil, "dht-private")
			c.onPendingRequest(id, nil, "")
			if c.maybeAutoAccept(id, nil, "") || c.isTrustedPeer(id) {
				t.Fatal("deleted peer accepted")
			}
			if p, err := db.GetPeer(ctx, id); err != nil || p != nil {
				t.Fatalf("deleted row resurrected: %+v %v", p, err)
			}
			if pending := c.PendingList(); len(pending) != 0 {
				t.Fatalf("deleted peer pending: %v", pending)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = peersdb.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			c.peers = db
			if c.maybeAutoAccept(id, nil, "") || !c.isUnfriendedPeer(id) {
				t.Fatal("restart lost deletion")
			}
			if err := c.ApprovePeer(id); err != nil {
				t.Fatal(err)
			}
			if c.isUnfriendedPeer(id) || !c.isTrustedPeer(id) {
				t.Fatal("explicit local approval did not restore peer")
			}
		})
	}
}

func TestExplicitRestoreDeletedPeer(t *testing.T) {
	ctx := context.Background()
	db, err := peersdb.Open(ctx, filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := &Client{cfg: Config{AutoAccept: true}, peers: db}
	if err := c.RemovePeer("restore"); err != nil {
		t.Fatal(err)
	}
	if err := c.restorePeer(ctx, "restore", nil); err != nil {
		t.Fatal(err)
	}
	if c.isUnfriendedPeer("restore") || !c.isKnownMember("restore") {
		t.Fatal("explicit re-add failed")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if c.maybeAutoAccept("new", nil, "") {
		t.Fatal("autoaccept must not report success on database error")
	}
	if err := c.restorePeer(ctx, "new", nil); err == nil {
		t.Fatal("explicit restore must report database error")
	}
}

func TestRejectedPeerOverridesAutoAccept(t *testing.T) {
	ctx := context.Background()
	db, err := peersdb.Open(ctx, filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := &Client{cfg: Config{AutoAccept: true}, peers: db}
	c.onPendingRequest("rejected", nil, "")
	if err := c.RejectPeer("rejected"); err != nil {
		t.Fatal(err)
	}
	if c.maybeAutoAccept("rejected", nil, "") {
		t.Fatal("automatic acceptance undid explicit rejection")
	}
}
