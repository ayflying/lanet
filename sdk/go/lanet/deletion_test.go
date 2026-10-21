package lanet

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ayflying/pvn/pkg/peersdb"
)

func TestDeletedPeerExplicitRequestAutoAccept(t *testing.T) {
	for _, approval := range []bool{true, false} {
		for _, autoAccept := range []bool{true, false} {
			name := map[bool]string{true: "approval", false: "no-approval"}[approval] + "/" +
				map[bool]string{true: "auto-accept", false: "manual-accept"}[autoAccept]
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				dbPath := filepath.Join(t.TempDir(), "peers.db")
				db, err := peersdb.Open(ctx, dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				c := &Client{cfg: Config{AutoAccept: autoAccept, RequireApproval: &approval}, peers: db}
				const id = "never-connected-unnamed"
				if !c.rememberPeer(id, nil, "", true) {
					t.Fatal("initial remember failed")
				}
				if err := c.RemovePeer(id); err != nil {
					t.Fatal(err)
				}
				c.onSeenUntrusted(id, nil, "dht-private")
				c.onPendingRequest(id, nil, "")
				// 被动发现、待审批记录与普通地址簿写入都不能恢复墓碑。
				for _, trusted := range []bool{false, true} {
					if c.rememberPeer(id, nil, "passive", trusted) {
						t.Fatal("passive remember restored deleted peer")
					}
				}
				if c.isTrustedPeer(id) || !c.isUnfriendedPeer(id) {
					t.Fatal("passive callbacks lost deletion")
				}
				if p, err := db.GetPeer(ctx, id); err != nil || p != nil {
					t.Fatalf("deleted row resurrected: %+v %v", p, err)
				}
				if pending := c.PendingList(); len(pending) != 1 || pending[0].PeerID != id {
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
				if !c.isUnfriendedPeer(id) || c.isTrustedPeer(id) {
					t.Fatal("restart lost deletion")
				}
				// 普通入向握手即使通过了调用前的检查，也不得恢复后来写入的墓碑。
				if c.maybeAutoAccept(id, nil, "ordinary") || !c.isUnfriendedPeer(id) {
					t.Fatal("ordinary autoaccept restored deleted peer")
				}
				// 协议层只为真实申请调用：自动同意开启时允许恢复，否则保留墓碑。
				if accepted := c.maybeAutoReaccept(id, nil, "requested"); accepted != autoAccept {
					t.Fatalf("explicit request accepted = %v, want %v", accepted, autoAccept)
				}
				if !autoAccept {
					if !c.isUnfriendedPeer(id) || c.isTrustedPeer(id) {
						t.Fatal("disabled autoaccept lost deletion")
					}
					if p, err := db.GetPeer(ctx, id); err != nil || p != nil {
						t.Fatalf("disabled autoaccept restored row: %+v %v", p, err)
					}
					if err := c.ApprovePeer(id); err != nil {
						t.Fatal(err)
					}
				}
				if c.isUnfriendedPeer(id) || !c.isTrustedPeer(id) {
					t.Fatal("explicit acceptance did not restore peer")
				}
				if autoAccept {
					if p, err := db.GetPeer(ctx, id); err != nil || p == nil || p.Name != "requested" || !p.Trusted {
						t.Fatalf("autoaccepted peer not remembered: %+v %v", p, err)
					}
					if pending := c.PendingList(); len(pending) != 0 {
						t.Fatalf("autoaccepted peer still pending: %v", pending)
					}
				}
			})
		}
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
	if c.maybeAutoAccept("new", nil, "") || c.maybeAutoReaccept("new", nil, "") {
		t.Fatal("autoaccept/reaccept must not report success on database error")
	}
	if err := c.restorePeer(ctx, "new", nil); err == nil {
		t.Fatal("explicit restore must report database error")
	}
}

func TestAutoAcceptClearUnfriendedFailure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "peers.db")
	db, err := peersdb.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := &Client{cfg: Config{AutoAccept: true}, peers: db}
	if err := c.RemovePeer("deleted"); err != nil {
		t.Fatal(err)
	}
	// 模拟墓碑写入失败，查询仍可用，自动同意必须失败且不得写回成员。
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER fail_clear_unfriended BEFORE DELETE ON unfriended
BEGIN SELECT RAISE(FAIL, 'clear unfriended failed'); END`); err != nil {
		t.Fatal(err)
	}
	if c.maybeAutoReaccept("deleted", nil, "") {
		t.Fatal("autoaccept succeeded despite tombstone clear failure")
	}
	if !c.isUnfriendedPeer("deleted") {
		t.Fatal("failed autoaccept lost tombstone")
	}
	if p, err := db.GetPeer(ctx, "deleted"); err != nil || p != nil {
		t.Fatalf("failed autoaccept restored row: %+v %v", p, err)
	}
	if !c.maybeAutoAccept("new", nil, "") || !c.maybeAutoReaccept("new-request", nil, "") {
		t.Fatal("unrelated tombstone failure blocked new peer")
	}
}

func TestRejectedPeerNewRequestAutoAccept(t *testing.T) {
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
	if !c.isUnfriendedPeer("rejected") || c.rememberPeer("rejected", nil, "", true) {
		t.Fatal("passive remember undid explicit rejection")
	}
	if !c.maybeAutoReaccept("rejected", nil, "") || c.isUnfriendedPeer("rejected") || !c.isTrustedPeer("rejected") {
		t.Fatal("new explicit request did not restore rejected peer")
	}
}
