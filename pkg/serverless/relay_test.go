package serverless

import (
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
		// 种子层：即使没连上、很久没见过，也应排在所有成员之前
		{id: mustPeerID(t, testPeerA), peerID: testPeerA, lastSeen: time.Time{}, addrs: []ma.Multiaddr{addr}, tier: 0},
		// 与种子同 ID 的重复项：应被去重
		{id: mustPeerID(t, testPeerA), peerID: testPeerA, addrs: []ma.Multiaddr{addr}, tier: 0},
	}

	got := orderRelayCandidates(cands, 3)
	if len(got) != 3 {
		t.Fatalf("应得 3 条（去重后共 3 个不同 ID），实际 %d：%v", len(got), got)
	}
	wantOrder := []string{testPeerA, testPeerB, testPeerC}
	for i, want := range wantOrder {
		if got[i].ID.String() != want {
			t.Errorf("第 %d 位应为 %s，实际 %s（顺序：%v）", i, want, got[i].ID, got)
		}
	}

	// 截断生效。
	if short := orderRelayCandidates(cands, 1); len(short) != 1 || short[0].ID.String() != testPeerA {
		t.Errorf("截断到 1 条应剩种子，实际 %v", short)
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
