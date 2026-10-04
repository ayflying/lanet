package peersdb

import (
	"context"
	"testing"
	"time"
)

func TestActivityMigrationDropsAmbiguousLegacyTime(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO peers (peer_id,name,trusted,notes,last_seen) VALUES ('legacy','name',1,'note',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := d.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := d.GetPeer(ctx, "legacy")
	if err != nil || p == nil || !p.LastSeen.IsZero() || p.Name != "name" || p.Notes != "note" || !p.Trusted {
		t.Fatalf("migration: %v %+v", err, p)
	}
	seen := time.Now().Truncate(time.Second)
	if err := d.UpsertPeer(ctx, Peer{PeerID: "legacy", LastSeen: seen}, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, err = d.GetPeer(ctx, "legacy")
	if err != nil || !p.LastSeen.Equal(seen) {
		t.Fatalf("migration repeated: %v %+v", err, p)
	}
}
