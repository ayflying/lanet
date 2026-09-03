package group

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ayflying/pvn/app/ctl/internal/logic/node"
)

// tempDBPath 返回测试专用的临时 SQLite 文件路径。
func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "groups.db")
}

// 持久化验证：创建群组 + 成员加入 + 地址通告后，
// 用同一个数据库文件重新打开注册表，所有数据必须完整恢复。

func TestPersistentRegistryRestoresAfterReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "groups.db")

	first, err := NewPersistentRegistry(ctx, dbPath)
	if err != nil {
		t.Fatalf("open persistent registry: %v", err)
	}

	grpA, creator, err := first.Create(ctx, CreateInput{
		PeerID: "peer-a", Name: "a", OS: "windows", GroupName: "alpha",
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, _, err = first.Join(ctx, JoinInput{
		InviteCode: grpA.InviteCode, PeerID: "peer-b", Name: "b", OS: "linux",
	}); err != nil {
		t.Fatalf("join alpha: %v", err)
	}
	if _, _, err = first.Create(ctx, CreateInput{
		PeerID: "peer-c", Name: "c", OS: "linux", GroupName: "beta",
	}); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if err = first.Announce(ctx, AnnounceInput{
		PeerID: "peer-b", Addrs: []string{"/ip4/203.0.113.5/udp/4001/quic-v1"},
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatalf("close registry: %v", err)
	}

	// 重新打开同一个数据库，模拟服务重启。
	second, err := NewPersistentRegistry(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen persistent registry: %v", err)
	}
	defer func() { _ = second.Close() }()

	// 1. 群组恢复：数量与子网序号正确，新群组不与旧群组冲突。
	groups := second.ListGroups(ctx)
	if len(groups) != 2 {
		t.Fatalf("restored groups = %d, want 2", len(groups))
	}
	grpNew, _, err := second.Create(ctx, CreateInput{
		PeerID: "peer-new", Name: "n", OS: "linux", GroupName: "gamma",
	})
	if err != nil {
		t.Fatalf("create after restore: %v", err)
	}
	if grpNew.CIDR == grpA.CIDR {
		t.Fatal("restored subnet counter lost; new group reuses old CIDR")
	}

	// 2. 成员与虚拟 IP 恢复：creator 仍是 100.64.0.2。
	netmap, err := second.NetMapFor(ctx, "peer-a")
	if err != nil {
		t.Fatalf("netmap for restored creator: %v", err)
	}
	if netmap.GroupName != "alpha" || netmap.CIDR != grpA.CIDR {
		t.Fatalf("restored netmap = %+v", netmap)
	}
	var creatorIP, memberBIP string
	var memberBAddrs []string
	for _, m := range netmap.Members {
		switch m.PeerID {
		case "peer-a":
			creatorIP = m.VirtualIP
		case "peer-b":
			memberBIP = m.VirtualIP
			memberBAddrs = m.Addrs
		}
	}
	if creatorIP != creator.VirtualIP {
		t.Fatalf("creator virtual IP after restore = %s, want %s", creatorIP, creator.VirtualIP)
	}
	if memberBIP != "100.64.0.3" {
		t.Fatalf("member-b virtual IP after restore = %s, want 100.64.0.3", memberBIP)
	}

	// 3. 邀请码恢复：凭旧邀请码仍能加入。
	if _, _, err = second.Join(ctx, JoinInput{
		InviteCode: grpA.InviteCode, PeerID: "peer-d", Name: "d", OS: "linux",
	}); err != nil {
		t.Fatalf("join with restored invite code: %v", err)
	}

	// 4. 通告地址恢复。
	if len(memberBAddrs) != 1 || memberBAddrs[0] != "/ip4/203.0.113.5/udp/4001/quic-v1" {
		t.Fatalf("restored addrs of peer-b = %v", memberBAddrs)
	}

	// 5. 隔离语义在恢复后依然成立。
	netmapC, err := second.NetMapFor(ctx, "peer-c")
	if err != nil {
		t.Fatalf("netmap for peer-c: %v", err)
	}
	for _, m := range netmapC.Members {
		if m.PeerID == "peer-a" || m.PeerID == "peer-b" {
			t.Fatalf("alpha member leaked into beta netmap after restore: %s", m.PeerID)
		}
	}
}

func TestRestoreNodeRejectsForeignIP(t *testing.T) {
	registry, err := node.NewRegistry("100.64.7.0/24", []string{"token"})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	if err = registry.RestoreNode(node.Node{PeerID: "peer-x", VirtualIP: "10.0.0.99"}); err == nil {
		t.Fatal("expected foreign virtual IP to be rejected")
	}
	if err = registry.RestoreNode(node.Node{PeerID: "peer-x", VirtualIP: "100.64.7.2"}); err != nil {
		t.Fatalf("restore in-subnet node: %v", err)
	}
	// 重复恢复同一 PeerID 应幂等。
	if err = registry.RestoreNode(node.Node{PeerID: "peer-x", VirtualIP: "100.64.7.2"}); err != nil {
		t.Fatalf("duplicate restore should be idempotent: %v", err)
	}
}
