package peersdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "lanet.db"))
	if err != nil {
		t.Fatalf("open peersdb: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestMigrateVersion 迁移应建立全部表并记录到目标版本。
func TestMigrateVersion(t *testing.T) {
	d := openTest(t)
	v, err := d.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v != 3 {
		t.Fatalf("期望迁移到版本 3，实际 %d", v)
	}
}

// TestUpsertAndTrust 审批状态流转：新节点默认不可信，置信任后永久免审。
func TestUpsertAndTrust(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a", Name: "node-a"}, []string{"/ip4/1.1.1.1/tcp/4001"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, _ := d.IsTrusted(ctx, "peer-a"); ok {
		t.Fatal("新节点不应默认为可信")
	}
	if err := d.SetTrusted(ctx, "peer-a", true); err != nil {
		t.Fatalf("set trusted: %v", err)
	}
	if ok, _ := d.IsTrusted(ctx, "peer-a"); !ok {
		t.Fatal("置信任后应为可信")
	}
	// 再次 Upsert 不得清掉信任状态（自动发现很频繁，不能误清审批）。
	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a"}, nil); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if ok, _ := d.IsTrusted(ctx, "peer-a"); !ok {
		t.Fatal("重复上报不得清掉已审批的信任状态")
	}
	// 撤销信任应生效。
	if err := d.SetTrusted(ctx, "peer-a", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, _ := d.IsTrusted(ctx, "peer-a"); ok {
		t.Fatal("撤销后不应为可信")
	}
}

// TestSetTrustedUnknownPeer 对手动添加的可信节点（库中尚无记录）也能直接置信任。
func TestSetTrustedUnknownPeer(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.SetTrusted(ctx, "peer-new", true); err != nil {
		t.Fatalf("set trusted on unknown: %v", err)
	}
	if ok, _ := d.IsTrusted(ctx, "peer-new"); !ok {
		t.Fatal("手动添加的可信节点应生效")
	}
}

// TestKnownAddrsPrefersWorking 地址优先排序：拨通过的成功次数多者优先，
// 这是「命中本地地址簿零查询秒连」的关键——必须优先给出真正可用的地址。
func TestKnownAddrsPrefersWorking(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	pid := "peer-addr"
	_ = d.UpsertPeer(ctx, Peer{PeerID: pid}, []string{
		"/ip4/10.0.0.1/tcp/4001",
		"/ip4/10.0.0.2/tcp/4001",
		"/ip4/10.0.0.3/tcp/4001",
	})
	// 10.0.0.3 成功两次，10.0.0.2 成功一次，10.0.0.1 从未成功。
	if err := d.NoteDialResult(ctx, pid, "/ip4/10.0.0.3/tcp/4001", true); err != nil {
		t.Fatalf("note ok 3: %v", err)
	}
	if err := d.NoteDialResult(ctx, pid, "/ip4/10.0.0.3/tcp/4001", true); err != nil {
		t.Fatalf("note ok 3 again: %v", err)
	}
	if err := d.NoteDialResult(ctx, pid, "/ip4/10.0.0.2/tcp/4001", true); err != nil {
		t.Fatalf("note ok 2: %v", err)
	}

	got, err := d.KnownAddrs(ctx, pid)
	if err != nil {
		t.Fatalf("known addrs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 条地址，实际 %d: %v", len(got), got)
	}
	if got[0] != "/ip4/10.0.0.3/tcp/4001" {
		t.Fatalf("成功次数最多的地址应排最前，实际首位 %q（全部：%v）", got[0], got)
	}
	if got[1] != "/ip4/10.0.0.2/tcp/4001" {
		t.Fatalf("成功一次应排第二，实际 %q（全部：%v）", got[1], got)
	}
	if got[2] != "/ip4/10.0.0.1/tcp/4001" {
		t.Fatalf("从未成功的地址应排最后，实际 %q（全部：%v）", got[2], got)
	}
}

// TestNoteDialFailureKeepsRecord 拨号失败也要留下地址记录（便于排查），
// 但不增加成功计数、不能让失败地址排到成功地址前面。
func TestNoteDialFailureKeepsRecord(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	pid := "peer-fail"
	_ = d.UpsertPeer(ctx, Peer{PeerID: pid}, nil)
	if err := d.NoteDialResult(ctx, pid, "/ip4/10.0.0.9/tcp/4001", true); err != nil {
		t.Fatalf("note ok: %v", err)
	}
	if err := d.NoteDialResult(ctx, pid, "/ip4/10.0.0.8/tcp/4001", false); err != nil {
		t.Fatalf("note fail: %v", err)
	}
	got, err := d.KnownAddrs(ctx, pid)
	if err != nil {
		t.Fatalf("known addrs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("失败地址也应保留记录，实际 %d 条: %v", len(got), got)
	}
	if got[0] != "/ip4/10.0.0.9/tcp/4001" {
		t.Fatalf("成功地址必须排在失败地址之前，实际 %v", got)
	}
}

// TestPendingLifecycle 待审批请求：新增、去重刷新、同意后移除。
func TestPendingLifecycle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.AddPending(ctx, PendingRequest{
		PeerID: "req-1", Name: "newbie", Addrs: []string{"/ip4/2.2.2.2/tcp/4001"},
		RequestedAt: time.Now(), Reason: "私有 DHT 发现",
	}); err != nil {
		t.Fatalf("add pending: %v", err)
	}
	if ok, _ := d.IsPending(ctx, "req-1"); !ok {
		t.Fatal("应处于待审批")
	}
	list, err := d.ListPending(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(list) != 1 || list[0].PeerID != "req-1" || len(list[0].Addrs) != 1 {
		t.Fatalf("待审批列表不符：%+v", list)
	}
	// 重复请求应刷新而非重复插入。
	if err := d.AddPending(ctx, PendingRequest{PeerID: "req-1", Name: "newbie2"}); err != nil {
		t.Fatalf("re-add pending: %v", err)
	}
	list, _ = d.ListPending(ctx)
	if len(list) != 1 {
		t.Fatalf("重复请求不应产生多条，实际 %d", len(list))
	}
	if list[0].Name != "newbie2" {
		t.Fatalf("重复请求应刷新名称，实际 %q", list[0].Name)
	}
	// 同意 → 置信任并清掉待审批。
	if err := d.SetTrusted(ctx, "req-1", true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if ok, _ := d.IsPending(ctx, "req-1"); ok {
		t.Fatal("同意后不应仍在待审批列表")
	}
	if ok, _ := d.IsTrusted(ctx, "req-1"); !ok {
		t.Fatal("同意后应为可信")
	}
}

// TestTrustedIDs 批量取可信集合（运行时热路径用）。
func TestTrustedIDs(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_ = d.SetTrusted(ctx, "t1", true)
	_ = d.SetTrusted(ctx, "t2", true)
	_ = d.UpsertPeer(ctx, Peer{PeerID: "t3"}, nil) // 不可信

	ids, err := d.TrustedIDs(ctx)
	if err != nil {
		t.Fatalf("trusted ids: %v", err)
	}
	if !ids["t1"] || !ids["t2"] || ids["t3"] {
		t.Fatalf("可信集合不符：%v", ids)
	}
}

// TestListPeersTrustedOnly 列表过滤。
func TestListPeersTrustedOnly(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_ = d.UpsertPeer(ctx, Peer{PeerID: "p1", Name: "one"}, []string{"/ip4/1.1.1.1/tcp/1"})
	_ = d.UpsertPeer(ctx, Peer{PeerID: "p2", Name: "two"}, nil)
	_ = d.SetTrusted(ctx, "p1", true)

	all, err := d.ListPeers(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("期望 2 个节点，实际 %d（err=%v）", len(all), err)
	}
	trusted, err := d.ListPeers(ctx, true)
	if err != nil || len(trusted) != 1 || trusted[0].PeerID != "p1" {
		t.Fatalf("期望仅 1 个可信节点 p1，实际 %+v（err=%v）", trusted, err)
	}
	if len(trusted[0].Addrs) != 1 {
		t.Fatalf("可信节点应带回地址，实际 %+v", trusted[0].Addrs)
	}
}

// TestDeletePeerCascades 删除节点应级联清掉其地址与待审批记录。
func TestDeletePeerCascades(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_ = d.UpsertPeer(ctx, Peer{PeerID: "gone"}, []string{"/ip4/3.3.3.3/tcp/1"})
	_ = d.AddPending(ctx, PendingRequest{PeerID: "gone"})
	if err := d.DeletePeer(ctx, "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p, _ := d.GetPeer(ctx, "gone"); p != nil {
		t.Fatal("节点应已删除")
	}
	if addrs, _ := d.KnownAddrs(ctx, "gone"); len(addrs) != 0 {
		t.Fatalf("地址应级联删除，实际 %v", addrs)
	}
}

// TestPersistenceAcrossReopen 重启后数据仍在（这是本需求的核心价值：
// 「不然每次都要申请太麻烦了」）。
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.db")
	ctx := context.Background()

	d1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if err := d1.UpsertPeer(ctx, Peer{PeerID: "keep", Name: "kept"}, []string{"/ip4/9.9.9.9/tcp/4001"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d1.SetTrusted(ctx, "keep", true); err != nil {
		t.Fatalf("trust: %v", err)
	}
	if err := d1.NoteDialResult(ctx, "keep", "/ip4/9.9.9.9/tcp/4001", true); err != nil {
		t.Fatalf("note dial: %v", err)
	}
	_ = d1.Close()

	// 重新打开（模拟进程重启）。
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer d2.Close()
	if ok, _ := d2.IsTrusted(ctx, "keep"); !ok {
		t.Fatal("重启后信任关系必须保留（否则每次都要重新审批）")
	}
	addrs, err := d2.KnownAddrs(ctx, "keep")
	if err != nil || len(addrs) != 1 {
		t.Fatalf("重启后地址簿必须保留，实际 %v（err=%v）", addrs, err)
	}
	p, _ := d2.GetPeer(ctx, "keep")
	if p == nil || p.Name != "kept" {
		t.Fatalf("重启后节点名应保留，实际 %+v", p)
	}
}

// TestNormalizeAddrs 地址清洗：去空、去重、保序。
func TestNormalizeAddrs(t *testing.T) {
	got := NormalizeAddrs([]string{"  /ip4/1.1.1.1/tcp/1 ", "", "/ip4/1.1.1.1/tcp/1", "/ip4/2.2.2.2/tcp/2"})
	if len(got) != 2 || got[0] != "/ip4/1.1.1.1/tcp/1" || got[1] != "/ip4/2.2.2.2/tcp/2" {
		t.Fatalf("地址清洗结果不符：%v", got)
	}
}

// TestNearbyLifecycle 附近节点：发现落库、去重刷新、成为好友后不再进附近。
func TestNearbyLifecycle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.UpsertNearby(ctx, Nearby{PeerID: "n1", Addrs: []string{"/ip4/4.4.4.4/tcp/4001"}, Source: "dht-private"}); err != nil {
		t.Fatalf("upsert nearby: %v", err)
	}
	list, err := d.ListNearby(ctx)
	if err != nil || len(list) != 1 || list[0].PeerID != "n1" {
		t.Fatalf("附近列表应含 n1，实际 %+v（err=%v）", list, err)
	}
	// 重复发现刷新地址/来源，不重复插入。
	if err := d.UpsertNearby(ctx, Nearby{PeerID: "n1", Addrs: []string{"/ip4/5.5.5.5/tcp/4001"}, Source: "mdns"}); err != nil {
		t.Fatalf("re-upsert nearby: %v", err)
	}
	list, _ = d.ListNearby(ctx)
	if len(list) != 1 {
		t.Fatalf("重复发现不应产生多条，实际 %d", len(list))
	}
	if list[0].Source != "mdns" || len(list[0].Addrs) != 1 || list[0].Addrs[0] != "/ip4/5.5.5.5/tcp/4001" {
		t.Fatalf("重复发现应刷新来源与地址，实际 %+v", list[0])
	}
	// 已成为好友（可信）的节点不该进附近。
	_ = d.SetTrusted(ctx, "friend", true)
	if err := d.UpsertNearby(ctx, Nearby{PeerID: "friend"}); err != nil {
		t.Fatalf("upsert trusted nearby: %v", err)
	}
	list, _ = d.ListNearby(ctx)
	for _, n := range list {
		if n.PeerID == "friend" {
			t.Fatal("已信任节点不应出现在附近列表")
		}
	}
	// 移除（成为好友时清理）。
	if err := d.RemoveNearby(ctx, "n1"); err != nil {
		t.Fatalf("remove nearby: %v", err)
	}
	list, _ = d.ListNearby(ctx)
	if len(list) != 0 {
		t.Fatalf("移除后附近应为空，实际 %+v", list)
	}
}

// TestUnfriendedLifecycle 删除好友墓碑：记录、查询、清除。
func TestUnfriendedLifecycle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if ok, _ := d.IsUnfriended(ctx, "x"); ok {
		t.Fatal("初始不应有墓碑")
	}
	if err := d.AddUnfriended(ctx, "x", "老张"); err != nil {
		t.Fatalf("add unfriended: %v", err)
	}
	if ok, _ := d.IsUnfriended(ctx, "x"); !ok {
		t.Fatal("写入后应存在墓碑")
	}
	// 墓碑不屏蔽附近可见性（被删好友应能重新申请连接）。
	if err := d.UpsertNearby(ctx, Nearby{PeerID: "x", Addrs: []string{"/ip4/6.6.6.6/tcp/1"}}); err != nil {
		t.Fatalf("upsert nearby after unfriend: %v", err)
	}
	list, _ := d.ListNearby(ctx)
	found := false
	for _, n := range list {
		if n.PeerID == "x" {
			found = true
		}
	}
	if !found {
		t.Fatal("被删除过的好友应仍出现在附近列表（可重新申请）")
	}
	// 清除墓碑（重新加回 / 告知送达后消费）。
	if err := d.ClearUnfriended(ctx, "x"); err != nil {
		t.Fatalf("clear unfriended: %v", err)
	}
	if ok, _ := d.IsUnfriended(ctx, "x"); ok {
		t.Fatal("清除后不应有墓碑")
	}
}
