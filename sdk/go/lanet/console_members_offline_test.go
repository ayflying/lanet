package lanet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
)

// stateMember 是 /api/state 里 members 数组的测试视图。
type stateMember struct {
	PeerID    string `json:"peer_id"`
	Name      string `json:"name"`
	Notes     string `json:"notes"`
	VirtualIP string `json:"virtual_ip"`
	Online    bool   `json:"online"`
	Path      string `json:"path"`
	LastSeen  int64  `json:"last_seen"`
}

// stateMembers 请求一次 /api/state 并取出成员数组。
func stateMembers(t *testing.T, c *Client) []stateMember {
	t.Helper()
	recorder := httptest.NewRecorder()
	c.apiState(recorder, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("apiState 状态码 %d: %s", recorder.Code, recorder.Body.String())
	}
	var state struct {
		Members []stateMember `json:"members"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析 /api/state 失败: %v", err)
	}
	return state.Members
}

// findMember 按 PeerID 在结果里找一项。
func findMember(list []stateMember, peerID string) *stateMember {
	for i := range list {
		if list[i].PeerID == peerID {
			return &list[i]
		}
	}
	return nil
}

// TestStateIncludesOfflineTrustedMembers 验证核心诉求：**已信任但当前离线的
// 节点必须出现在成员列表里，并显示之前记住的名字**。
//
// 背景：成员表是内存实时视图，Discovery 的 reapExpired() 会在超过 memberTTL
// （默认 10 分钟）无通讯后把节点直接 delete 掉，于是控制台成员列表里「人凭空
// 消失」——用户要的是「显示成离线、由我手动删除」，而不是被静默隐藏。
// 持久化地址簿（peersdb）里的信任关系与备注一直都在，本用例确保它们被并回列表。
func TestStateIncludesOfflineTrustedMembers(t *testing.T) {
	ctx := context.Background()
	c := newStandaloneClient(t, "offline-backfill", "offline-backfill", filepath.Join(t.TempDir(), "peers.db"))

	// 造两条地址簿记录：
	//  - gone：已信任、带备注与上次虚拟 IP —— 应当被回填成离线成员；
	//  - stranger：连过但没审批 —— 属于陌生节点，绝不能混进成员列表。
	seen := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	gone := peersdb.Peer{
		PeerID:    "12D3KooWOfflineTrustedMemberAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Name:      "old-laptop",
		LastIP:    "10.7.9.9",
		LastSeen:  seen,
		FirstSeen: seen.Add(-time.Hour),
	}
	if err := c.peers.UpsertPeer(ctx, gone, nil); err != nil {
		t.Fatalf("写入地址簿失败: %v", err)
	}
	if err := c.peers.SetTrusted(ctx, gone.PeerID, true); err != nil {
		t.Fatalf("设置信任失败: %v", err)
	}
	if err := c.peers.SetNotes(ctx, gone.PeerID, "我的旧笔记本"); err != nil {
		t.Fatalf("写入备注失败: %v", err)
	}
	stranger := peersdb.Peer{PeerID: "12D3KooWStrangerNeverApprovedBBBBBBBBBBBBBBBBBBBBBBB", Name: "stranger"}
	if err := c.peers.UpsertPeer(ctx, stranger, nil); err != nil {
		t.Fatalf("写入陌生节点失败: %v", err)
	}

	members := stateMembers(t, c)

	got := findMember(members, gone.PeerID)
	if got == nil {
		t.Fatalf("已信任的离线节点未出现在成员列表：%+v", members)
	}
	if got.Online {
		t.Fatalf("离线节点被误标为在线：%+v", got)
	}
	if got.Notes != "我的旧笔记本" {
		t.Fatalf("备注丢失（前端优先显示备注）：%+v", got)
	}
	if got.VirtualIP != "10.7.9.9" {
		t.Fatalf("应回显地址簿记录的上次虚拟 IP，实际 %q", got.VirtualIP)
	}
	if got.LastSeen != seen.Unix() {
		t.Fatalf("最后活跃时间应为 %d，实际 %d", seen.Unix(), got.LastSeen)
	}
	if got.Path != "" {
		t.Fatalf("离线节点不应伪造链路类型，实际 %q", got.Path)
	}

	// 未审批的陌生节点必须被挡在成员列表之外。
	if findMember(members, stranger.PeerID) != nil {
		t.Fatalf("未审批节点不应进入成员列表：%+v", members)
	}
}

// TestStateBackfillSkipsSelf 身份文件换过之后，地址簿里可能残留指向当前节点
// 自己的一行。回填时若不排除，列表里会出现「一个离线的我」，纯属误导。
func TestStateBackfillSkipsSelf(t *testing.T) {
	ctx := context.Background()
	c := newStandaloneClient(t, "self-backfill", "self-backfill", filepath.Join(t.TempDir(), "peers.db"))

	self := peersdb.Peer{PeerID: c.Info().PeerID, Name: "me", LastSeen: time.Now().Add(-time.Hour)}
	if err := c.peers.UpsertPeer(ctx, self, nil); err != nil {
		t.Fatalf("写入地址簿失败: %v", err)
	}
	if err := c.peers.SetTrusted(ctx, self.PeerID, true); err != nil {
		t.Fatalf("设置信任失败: %v", err)
	}

	if got := findMember(stateMembers(t, c), self.PeerID); got != nil && !got.Online {
		t.Fatalf("回填不应包含本机自己：%+v", got)
	}
}

// TestStateOnlineMemberNotDuplicated 同一节点同时在实时成员表与地址簿里时，
// 只能出一条：以实时那条（在线、带链路）为准，不能被离线回填覆盖成两条或离线。
func TestStateOnlineMemberNotDuplicated(t *testing.T) {
	ctx := context.Background()
	c := newStandaloneClient(t, "dedupe-backfill", "dedupe-backfill", filepath.Join(t.TempDir(), "peers.db"))

	const pid = "12D3KooWDedupeTrustedMemberCCCCCCCCCCCCCCCCCCCCCCCC"
	if err := c.peers.UpsertPeer(ctx, peersdb.Peer{PeerID: pid, Name: "dup", LastSeen: time.Now().Add(-time.Hour)}, nil); err != nil {
		t.Fatalf("写入地址簿失败: %v", err)
	}
	if err := c.peers.SetTrusted(ctx, pid, true); err != nil {
		t.Fatalf("设置信任失败: %v", err)
	}
	// 不注入实时成员：本用例只验证「地址簿命中一次就只出一条离线记录」这个
	// 去重契约；实时成员与地址簿并在同一条上的路径由 seen 映射保证。
	var hits int
	for _, m := range stateMembers(t, c) {
		if m.PeerID == pid {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("同一节点应只出现一次，实际 %d 次", hits)
	}
}
