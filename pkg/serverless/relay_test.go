package serverless

import (
	"context"
	"encoding/json"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// 真实可解析的 base58 节点 ID（占位串会被 peer.Decode 拒绝）。
const (
	testPeerA = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
	testPeerB = "12D3KooWQP5ZLg1sjmHJzEHJyZU6zMidVWHqWfApXDMuGdjoBkUB"
	testPeerC = "12D3KooWGuwB2SpnvuzXBC1HpXrnwNPJC45GSMf2EMzWK9VWow8h"
)

func mustPeerID(t *testing.T, s string) peer.ID {
	t.Helper()
	id, err := peer.Decode(s)
	if err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return id
}

// TestRelayUsableAddrs 中继候选地址筛选：三种必然失败的地址必须被剔掉。
func TestRelayUsableAddrs(t *testing.T) {
	in := []ma.Multiaddr{
		ma.StringCast("/ip4/169.254.10.20/tcp/4001"),                             // link-local：对别的主机不可达
		ma.StringCast("/ip6/fe80::1/tcp/4001"),                                   // IPv6 link-local
		ma.StringCast("/ip4/127.0.0.1/tcp/4001"),                                 // 回环
		ma.StringCast("/ip4/0.0.0.0/tcp/4001"),                                   // 未指定
		ma.StringCast("/ip4/10.7.207.102/tcp/4001"),                              // lanet overlay：成环
		ma.StringCast("/ip4/1.2.3.4/tcp/4001/p2p/" + testPeerA + "/p2p-circuit"), // 经中继预约中继：成环
		ma.StringCast("/ip6/2408:824e::99/tcp/4001"),                             // 公网 IPv6：保留且应排最前
		ma.StringCast("/ip4/192.168.50.170/tcp/4001"),                            // 私网：保留
	}
	got := relayUsableAddrs(in)
	want := []string{
		"/ip6/2408:824e::99/tcp/4001",
		"/ip4/192.168.50.170/tcp/4001",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d 条 %v，got %d 条 %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("第 %d 条：want %s，got %s", i, want[i], got[i].String())
		}
	}
}

// TestOrderRelayCandidates 候选排序、去重与截断。
func TestOrderRelayCandidates(t *testing.T) {
	now := time.Now()
	addr := ma.StringCast("/ip4/1.2.3.4/tcp/4001")

	cands := []relayCandidate{
		// 成员层：没连上、很久没见过
		{id: mustPeerID(t, testPeerC), peerID: testPeerC, lastSeen: now.Add(-9 * time.Minute), addrs: []ma.Multiaddr{addr}, tier: 1},
		// 成员层：已连上（应排在未连上的成员之前）
		{id: mustPeerID(t, testPeerB), peerID: testPeerB, lastSeen: now.Add(-9 * time.Minute), addrs: []ma.Multiaddr{addr}, connected: true, tier: 1},
		// 未连接种子必须排在已连接群成员之后
		{id: mustPeerID(t, testPeerA), peerID: testPeerA, lastSeen: time.Time{}, addrs: []ma.Multiaddr{addr}, tier: 0},
		// 与种子同 ID 的重复项：应被去重
		{id: mustPeerID(t, testPeerA), peerID: testPeerA, addrs: []ma.Multiaddr{addr}, tier: 0},
	}

	got := orderRelayCandidates(cands, 3)
	if len(got) != 3 {
		t.Fatalf("应得 3 条（去重后共 3 个不同 ID），实际 %d：%v", len(got), got)
	}
	wantOrder := []string{testPeerB, testPeerA, testPeerC}
	for i, want := range wantOrder {
		if got[i].ID.String() != want {
			t.Errorf("第 %d 位应为 %s，实际 %s（顺序：%v）", i, want, got[i].ID, got)
		}
	}

	// 截断生效。
	if short := orderRelayCandidates(cands, 1); len(short) != 1 || short[0].ID.String() != testPeerB {
		t.Errorf("截断到 1 条应剩已连接成员，实际 %v", short)
	}
	// 空输入不 panic。
	if out := orderRelayCandidates(nil, 3); len(out) != 0 {
		t.Errorf("空输入应得空输出，实际 %v", out)
	}
	// 同一输入两次调用顺序必须一致（autorelay 会反复取候选，顺序抖动会导致反复重试）。
	first := orderRelayCandidates([]relayCandidate{cands[2], cands[0], cands[1]}, 3)
	second := orderRelayCandidates([]relayCandidate{cands[1], cands[2], cands[0]}, 3)
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("顺序不稳定：%v vs %v", first, second)
		}
	}
}

// Reserving the first successful hop must not stop before later hops: the
// remote peer may only be able to reach the latter.
func TestRelayReservationContinuesAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := testHost(t, false)
	first, second := testHost(t, true), testHost(t, true)
	candidates := []peer.AddrInfo{{ID: first.ID(), Addrs: first.Addrs()}, {ID: second.ID(), Addrs: second.Addrs()}}
	if err := reserveRelayCandidates(ctx, target, candidates); err != nil {
		t.Fatal(err)
	}
	target.SetStreamHandler("/lanet/reservation-test/1", func(st network.Stream) { defer st.Close(); _, _ = io.Copy(st, st) })
	for _, hop := range candidates {
		caller := testHost(t, false)
		// No direct target address is installed in caller's peerstore.
		addr := hop.Addrs[0].Encapsulate(ma.StringCast("/p2p/" + hop.ID.String() + "/p2p-circuit/p2p/" + target.ID().String()))
		if err := caller.Connect(network.WithAllowLimitedConn(ctx, "test"), peer.AddrInfo{ID: target.ID(), Addrs: []ma.Multiaddr{addr}}); err != nil {
			t.Fatalf("reservation missing on %s: %v", hop.ID, err)
		}
		st, err := caller.NewStream(network.WithAllowLimitedConn(ctx, "test"), target.ID(), "/lanet/reservation-test/1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Conn().RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT); err != nil {
			t.Fatal("not real circuit")
		}
		_ = st.SetDeadline(time.Now().Add(time.Second))
		_, err = st.Write([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(st, buf); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		const infoProto = "/lanet/relay-info-test/1"
		target.SetStreamHandler(infoProto, func(s network.Stream) {
			defer s.Close()
			var req infoPayload
			if json.NewDecoder(s).Decode(&req) == nil {
				_ = json.NewEncoder(s).Encode(infoPayload{Name: "relay-target"})
			}
		})
		d := &Discovery{host: caller, protoInfo: infoProto, groupKey: make([]byte, 32)}
		info, err := d.fetchInfo(ctx, target.ID())
		if err != nil || info.Name != "relay-target" {
			t.Fatalf("member info over limited relay: %v %+v", err, info)
		}
	}
}

func TestRelayCandidatesExcludeRevokedTrust(t *testing.T) {
	h := testHost(t, false)
	id := mustPeerID(t, testPeerA)
	h.Peerstore().AddAddr(id, ma.StringCast("/ip4/192.168.50.20/tcp/4001"), peerstore.TempAddrTTL)
	d := &Discovery{host: h, memberTTL: time.Hour, members: map[string]*Member{testPeerA: {PeerID: testPeerA, LastSeen: time.Now()}}}
	d.cfg.IsTrusted = func(string) bool { return false }
	got, err := d.Candidates(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("revoked member returned: %v", got)
	}
	d.cfg.IsTrusted = func(string) bool { return true }
	got, err = d.Candidates(context.Background(), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("trusted member lost: %v %v", got, err)
	}
}
