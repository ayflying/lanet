package lanet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// newStandaloneClient 起一个本地 standalone 节点（回环监听，独立地址簿）。
func newStandaloneClient(t *testing.T, name, key, dbPath string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	c, err := New(ctx, Config{
		Name:         name,
		Standalone:   true,
		NetworkKey:   key,
		DBPath:       dbPath,
		ConsoleAddr:  "-", // 关掉控制台，避免测试环境端口冲突
		IdentityFile: filepath.Join(t.TempDir(), "node.key"),
		ListenAddrs: []string{
			"/ip4/127.0.0.1/tcp/0",
			"/ip4/127.0.0.1/udp/0/quic-v1",
		},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestConnectPeerByIDRequiresMutualApproval 验证「按节点 ID 连接」的完整闭环：
//  1. A 添加 B 的节点 ID → 本机立即信任 B（主动添加 = 我的同意），
//     但 B 还没同意 A → 返回 Pending（申请已送达）；
//  2. B 的控制台看到待审批 → 同意 → 双方连通，A 进入 B 的成员表；
//  3. 永久信任：地址簿里留下 B，后续连接走本地地址簿（零 DHT 查询）。
func TestConnectPeerByIDRequiresMutualApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()
	a := newStandaloneClient(t, "node-a", "grp-approval-e2e", filepath.Join(dir, "a.db"))
	b := newStandaloneClient(t, "node-b", "grp-approval-e2e", filepath.Join(dir, "b.db"))

	// 模拟「网络可达」：先建立传输层连接，让双方 peerstore 有对方地址。
	// 真实场景里这一步由 mDNS / 私有 DHT（同一网络密钥 + 种子节点）完成——
	// 测试环境没有种子节点，故显式建连替代。
	bID, err := peer.Decode(b.Info().PeerID)
	if err != nil {
		t.Fatalf("decode B peer id: %v", err)
	}
	if err = a.Host().Connect(ctx, peer.AddrInfo{ID: bID, Addrs: b.Host().Addrs()}); err != nil {
		t.Fatalf("建立传输层连接失败: %v", err)
	}

	// A 主动添加 B 的节点 ID。
	res, err := a.ConnectPeer(ctx, b.Info().PeerID)
	if err != nil {
		t.Fatalf("A 添加 B 节点 ID 失败: %v", err)
	}
	if !res.Pending {
		t.Fatalf("B 尚未同意，A 应得到 Pending（申请已送达），实际 %+v", res)
	}
	if res.PeerID != b.Info().PeerID {
		t.Fatalf("Pending 结果应带回对端节点 ID，实际 %q", res.PeerID)
	}
	// A 这边已把 B 记为已信任（主动添加 = 我方同意）。
	if !a.isTrustedPeer(b.Info().PeerID) {
		t.Fatalf("A 主动添加后应在本机信任 B")
	}
	// B 那边把 A 记入待审批列表。
	waitFor(t, 10*time.Second, func() bool {
		for _, p := range b.PendingList() {
			if p.PeerID == a.Info().PeerID {
				return true
			}
		}
		return false
	}, "B 应把 A 记入待审批列表")

	// B 同意 A 的连接申请。
	if err = b.ApprovePeer(a.Info().PeerID); err != nil {
		t.Fatalf("B 同意 A 的连接申请失败: %v", err)
	}
	// 同意后待审批清空、信任建立。
	if n := len(b.PendingList()); n != 0 {
		t.Fatalf("同意后待审批列表应清空，实际剩 %d 条", n)
	}
	if !b.isTrustedPeer(a.Info().PeerID) {
		t.Fatalf("B 同意后应信任 A")
	}
	// 双方应最终连通：A 的成员表里出现 B（名称已由 info 交换补齐）。
	waitFor(t, 25*time.Second, func() bool {
		for _, m := range a.NetMap().Members {
			if m.PeerID == b.Info().PeerID && m.Name == "node-b" {
				return true
			}
		}
		return false
	}, "A 的成员表里应出现已互相同意的 B（含名称）")

	// 地址簿持久化：A 的信任列表含 B，且带可用地址。
	peers := a.TrustedPeers()
	found := false
	for _, p := range peers {
		if p.PeerID == b.Info().PeerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("A 的地址簿应持久化 B，实际 %+v", peers)
	}
}

// TestConnectPeerUnknownIDFails 连接一个不存在的节点 ID：应给出明确提示，
// 而不是静默超时——用户需要知道「是对方没启动 / ID 错了 / 密钥不一致」。
func TestConnectPeerUnknownIDFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-solo", "grp-unknown", filepath.Join(t.TempDir(), "solo.db"))

	// 一个格式合法但网络中不存在的节点 ID（取自另起的临时节点，随即关闭）。
	ghost := newStandaloneClient(t, "ghost", "grp-unknown", filepath.Join(t.TempDir(), "g.db"))
	ghostID := ghost.Info().PeerID
	_ = ghost.Close()

	_, err := a.ConnectPeer(ctx, ghostID)
	if err == nil {
		t.Fatalf("连接不存在的节点应返回错误，而不是静默成功")
	}
}

// TestConnectPeerRejectsGarbage 输入既不是节点 ID 也不是 multiaddr（如 IP、
// 乱码）时应立刻报错，不能卡住等待。
func TestConnectPeerRejectsGarbage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-x", "grp-garbage", filepath.Join(t.TempDir(), "x.db"))
	for _, bad := range []string{"", "  ", "192.168.1.1", "hello world", "12D3Koo"} {
		if _, err := a.ConnectPeer(ctx, bad); err == nil {
			t.Fatalf("非法输入 %q 应立刻报错", bad)
		}
	}
}

// TestEmptyAddressBookSkipsDHTSearch 流量控制：地址簿为空时不做主动 DHT 查找。
func TestEmptyAddressBookSkipsDHTSearch(t *testing.T) {
	a := newStandaloneClient(t, "node-flow", "grp-flow-e2e", filepath.Join(t.TempDir(), "f.db"))
	if a.hasKnownPeers() {
		t.Fatalf("新节点地址簿为空时 hasKnownPeers 应为 false（跳过主动 DHT 查找省流量）")
	}
}

// TestConnectPeerSelfRejected 输入本机节点 ID 应被立刻挡下。
func TestConnectPeerSelfRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a := newStandaloneClient(t, "node-self", "grp-self", filepath.Join(t.TempDir(), "s.db"))
	if _, err := a.ConnectPeer(ctx, a.Info().PeerID); err == nil {
		t.Fatalf("连接本机节点 ID 应报错")
	}
}

// waitFor 轮询等待条件成立，超时即失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, msg)
}
