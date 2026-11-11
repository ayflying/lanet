package serverless

import (
	"github.com/libp2p/go-libp2p/core/peer"
	"testing"
)

func TestReservationCandidatesRotateWithoutStarvation(t *testing.T) {
	peers := make([]peer.AddrInfo, 10)
	for i := range peers {
		peers[i].ID = peer.ID(string(rune('a' + i)))
	}
	cursor := 0
	seen := make(map[peer.ID]bool)
	for n := 0; n < 5; n++ {
		order := rotateReservationCandidates(peers, &cursor)
		// 模拟每轮控制握手容量只能接纳前8，轮转必须覆盖所有候选。
		for _, p := range order[:8] {
			seen[p.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("candidate starvation: %d", len(seen))
	}
	for i, p := range peers {
		if p.ID != peer.ID(string(rune('a'+i))) {
			t.Fatal("rotation mutated caller order")
		}
	}
	if rotateReservationCandidates(nil, &cursor) != nil {
		t.Fatal("empty rotation")
	}
}
