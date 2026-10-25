package serverless

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	ma "github.com/multiformats/go-multiaddr"
)

// 已知中继能力优先，握手时间变化不应扰动同级节点顺序。
func TestRelayCapabilityAndStableOrder(t *testing.T) {
	addr := ma.StringCast("/ip4/192.0.2.1/tcp/4001")
	a, b := mustPeerID(t, testPeerA), mustPeerID(t, testPeerB)
	cands := []relayCandidate{{id: a, peerID: testPeerA, addrs: []ma.Multiaddr{addr}, connected: true, tier: 1}, {id: b, peerID: testPeerB, addrs: []ma.Multiaddr{addr}, hop: true, tier: 0}}
	if got := orderRelayCandidates(cands, 2); got[0].ID != b {
		t.Fatal("已知HOP能力未优先")
	}
	cands[0].hop = true
	cands[1].connected = true
	cands[1].tier = 1
	first := orderRelayCandidates(cands, 2)
	cands[0].lastSeen = time.Now().Add(time.Hour)
	cands[1].lastSeen = time.Now().Add(-time.Hour)
	second := orderRelayCandidates(cands, 2)
	if first[0].ID != second[0].ID {
		t.Fatal("握手时间引起同级候选抖动")
	}
}

func TestReapedMemberStalePEXDoesNotPublish(t *testing.T) {
	h := testHost(t, false)
	id := mustPeerID(t, testPeerA)
	d := &Discovery{host: h, memberTTL: time.Minute, members: map[string]*Member{testPeerA: {PeerID: testPeerA, LastSeen: time.Now().Add(-time.Hour)}}}
	var emits atomic.Int32
	d.OnDiscovered(func(Member) { emits.Add(1) })
	d.reapExpired()
	hint := memberHint{PeerID: testPeerA, Addrs: []string{"/ip4/192.0.2.1/tcp/4001"}}
	for i := 0; i < 3; i++ {
		d.applyMemberHints([]memberHint{hint, hint})
		if d.addMember(id, nil, "dht") {
			t.Fatal("隔离成员被当作新入表")
		}
		d.reapExpired()
	}
	if len(d.Peers()) != 0 || emits.Load() != 0 {
		t.Fatalf("陈旧发现制造假上线: %v, emits=%d", d.Peers(), emits.Load())
	}
	if len(h.Peerstore().Addrs(id)) == 0 {
		t.Fatal("隔离不应丢弃恢复地址")
	}
	fresh := mustPeerID(t, testPeerB)
	if !d.addMember(fresh, nil, "mdns") || len(d.Peers()) != 1 {
		t.Fatal("首次发现兼容性被破坏")
	}
}

func TestQuarantineHandshakeRecovery(t *testing.T) {
	for _, inbound := range []bool{false, true} {
		t.Run(fmt.Sprintf("inbound=%t", inbound), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h, r := testHost(t, false), testHost(t, false)
			d, err := New(ctx, h, Config{NetworkKey: "quarantine-recovery", Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			other, err := New(ctx, r, Config{NetworkKey: "quarantine-recovery", Name: "returned", Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if err = d.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if err = other.Start(ctx); err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			d.members[r.ID().String()] = &Member{PeerID: r.ID().String(), LastSeen: time.Now().Add(-time.Hour)}
			d.mu.Unlock()
			d.reapExpired()
			var falseEvents atomic.Int32
			d.OnDiscovered(func(m Member) {
				if m.LastSeen.IsZero() {
					falseEvents.Add(1)
				}
			})
			h.Peerstore().AddAddrs(r.ID(), r.Addrs(), peerstore.TempAddrTTL)
			if inbound {
				if err = r.Connect(ctx, peer.AddrInfo{ID: h.ID(), Addrs: h.Addrs()}); err != nil {
					t.Fatal(err)
				}
				if _, err = other.fetchInfo(ctx, h.ID()); err != nil {
					t.Fatal(err)
				}
			} else if err = d.connectAndIdentifyContext(ctx, r.ID()); err != nil {
				t.Fatal(err)
			}
			ms := d.Peers()
			if len(ms) != 1 || ms[0].LastSeen.IsZero() || ms[0].Name != "returned" || falseEvents.Load() != 0 {
				t.Fatalf("真实握手未恢复或发布占位: %+v", ms)
			}
			d.mu.RLock()
			blocked := d.expiredMembers.requiresHandshake(r.ID().String())
			d.mu.RUnlock()
			if blocked {
				t.Fatal("成功握手未解除隔离")
			}
		})
	}
}

func TestMemberQuarantineBoundedFailClosed(t *testing.T) {
	var q memberQuarantine
	for i := 0; i < expiredMemberLimit+10; i++ {
		q.remember(fmt.Sprint(i))
	}
	if len(q.ids) != expiredMemberLimit || !q.requiresHandshake("unknown") {
		t.Fatal("隔离容量无界或溢出放行")
	}
	q.verified("0")
	if !q.requiresHandshake("0") {
		t.Fatal("饱和后不能因清除某条记录而遗忘溢出成员")
	}
}

// 隔离解除不能绕过审批或删除好友保护。
func TestQuarantineDoesNotBypassApproval(t *testing.T) {
	d := &Discovery{members: make(map[string]*Member), groupKey: make([]byte, 32)}
	d.expiredMembers.remember(testPeerA)
	d.cfg.IsTrusted = func(string) bool { return false }
	if d.verifiedMemberLocked(testPeerA, "revalidated") != nil {
		t.Fatal("未审批成员恢复")
	}
	d.cfg.IsTrusted = func(string) bool { return true }
	d.cfg.IsUnfriended = func(string) bool { return true }
	if d.verifiedMemberLocked(testPeerA, "revalidated") != nil {
		t.Fatal("删除好友墓碑被绕过")
	}
	d.cfg.IsUnfriended = func(string) bool { return false }
	if d.verifiedMemberLocked(testPeerA, "revalidated") == nil {
		t.Fatal("显式审批后无法恢复")
	}
}

func TestRelayCandidatesExcludeFailedPlaceholder(t *testing.T) {
	h := testHost(t, false)
	id := mustPeerID(t, testPeerA)
	h.Peerstore().AddAddr(id, ma.StringCast("/ip4/192.168.50.20/tcp/4001"), peerstore.TempAddrTTL)
	d := &Discovery{host: h, memberTTL: time.Hour, members: map[string]*Member{testPeerA: {PeerID: testPeerA, FirstSeen: time.Now(), LastDiscovered: time.Now()}}}
	got, err := d.Candidates(context.Background(), 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("从未握手的失败占位进入中继候选: %v %v", got, err)
	}
	if hints := d.pexSnapshot(); len(hints) != 0 {
		t.Fatal("失败占位不应经PEX扩散")
	}
}
