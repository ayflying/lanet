package lanet

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func seedTestPeerID(t *testing.T) string {
	t.Helper()
	_, pub, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func TestConnectSeedValidatesBeforeTrusting(t *testing.T) {
	self, target := seedTestPeerID(t), seedTestPeerID(t)
	valid := "/ip4/127.0.0.1/tcp/4001/p2p/" + target
	for _, suffix := range []string{"not-an-address", "/ip4/127.0.0.1/tcp/4001", "/ip4/127.0.0.1/tcp/4001/p2p/" + self} {
		t.Run(suffix, func(t *testing.T) {
			c, db := newSeedTestClient(t, self)
			if err := c.RemovePeer(target); err != nil {
				t.Fatal(err)
			}
			c.disc = &serverless.Discovery{}
			if _, err := c.ConnectSeed(valid + "," + suffix); err == nil {
				t.Fatal("invalid seed accepted")
			}
			if !c.isUnfriendedPeer(target) {
				t.Fatal("invalid input cleared earlier target's tombstone")
			}
			if p, err := db.GetPeer(context.Background(), target); err != nil || p != nil {
				t.Fatalf("invalid input trusted earlier target: %+v %v", p, err)
			}
		})
	}
}

func TestConnectSeedTrustDatabaseError(t *testing.T) {
	c, db := newSeedTestClient(t, seedTestPeerID(t))
	c.disc = &serverless.Discovery{}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConnectSeed("/ip4/127.0.0.1/tcp/4001/p2p/" + seedTestPeerID(t)); err == nil {
		t.Fatal("database error ignored")
	}
}

func TestConnectSeedGrantsLocalTrust(t *testing.T) {
	dir := t.TempDir()
	a := newStandaloneClient(t, "seed-a", "seed-local-trust", filepath.Join(dir, "a.db"))
	b := newStandaloneClient(t, "seed-b", "seed-local-trust", filepath.Join(dir, "b.db"))
	// 手动连接种子不依赖本机 auto_accept；对方预先同意，避免异步审批干扰。
	a.cfg.AutoAccept = false
	if err := b.ApprovePeer(a.peerID); err != nil {
		t.Fatal(err)
	}
	if err := a.RemovePeer(b.peerID); err != nil {
		t.Fatal(err)
	}
	seed := fmt.Sprintf("%s/p2p/%s", b.Host().Addrs()[0], b.peerID)
	id, err := a.ConnectSeed(seed)
	if err != nil || id != b.peerID {
		t.Fatalf("ConnectSeed = %q, %v", id, err)
	}
	if a.isUnfriendedPeer(b.peerID) || !a.isTrustedPeer(b.peerID) {
		t.Fatal("manual seed connection did not restore local trust")
	}
}
