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
	// 手动连接种子不依赖本机 auto_accept。
	a.cfg.AutoAccept = false
	// 只构造本机持久化删除状态：本测试验证种子恢复本地信任，
	// 不启动 RemovePeer 的异步双向删除通知，否则迟到通知可能关闭
	// 下方 ConnectSeed 刚建立的连接，或撤销 b 的预审批。
	if err := a.peers.RemovePeer(context.Background(), b.peerID); err != nil {
		t.Fatal(err)
	}
	if !a.isUnfriendedPeer(b.peerID) || a.isTrustedPeer(b.peerID) {
		t.Fatal("local deletion fixture did not revoke trust and create tombstone")
	}
	// 最后同步设置对方审批状态。不能调用 ApprovePeer：它会启动后台
	// 重连，在 a 仍持墓碑时握手会触发 unfriended 并反过来撤销 b 的信任。
	if err := b.peers.SetTrusted(context.Background(), a.peerID, true); err != nil {
		t.Fatal(err)
	}
	if !b.isTrustedPeer(a.peerID) {
		t.Fatal("remote approval fixture did not grant trust")
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
