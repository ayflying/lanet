package lanet

import (
	"math/rand"
	"testing"

	"github.com/ayflying/pvn/pkg/netmapclient"
)

// 回归：/api/member-info 曾在循环里反复调用 NetMap()，而 NetMap() 每次都从
// 发现层重建切片（顺序随机），导致「查 A 返回 B」甚至误报 404。修复后查找
// 必须基于调用方传入的同一份快照，任意顺序、任意次数结果都一致。
func TestFindNetMapMemberSameSnapshotAlwaysConsistent(t *testing.T) {
	members := []netmapclient.Member{
		{PeerID: "peer-docker", Name: "docker-server", VirtualIP: "10.7.107.102"},
		{PeerID: "peer-fnos", Name: "fnos", VirtualIP: "10.7.122.184"},
		{PeerID: "peer-share", Name: "sonow-Share", VirtualIP: "10.7.189.26"},
		{PeerID: "peer-yunloli", Name: "yunloli-server", VirtualIP: "10.7.204.166"},
	}
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		shuffled := append([]netmapclient.Member(nil), members...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for _, want := range members {
			got := findNetMapMember(shuffled, want.PeerID)
			if got == nil {
				t.Fatalf("第 %d 轮查 %s 未找到", round, want.PeerID)
			}
			if got.PeerID != want.PeerID || got.Name != want.Name || got.VirtualIP != want.VirtualIP {
				t.Fatalf("第 %d 轮查 %s 却返回 %+v", round, want.PeerID, got)
			}
		}
	}
	if got := findNetMapMember(members, "peer-not-exist"); got != nil {
		t.Fatalf("不存在的成员应返回 nil，实际 %+v", got)
	}
}
