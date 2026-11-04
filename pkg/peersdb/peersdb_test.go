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
	if v != 6 {
		t.Fatalf("期望迁移到版本 6，实际 %d", v)
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

// TestSetTrustedClearsPending 审批通过必须顺手清掉待审批记录；撤销信任
// （删除好友）后，对方的重新申请仍可进入待审批（删除不是拉黑）。
//
// 历史缺陷（0.5.39 时代）出在「附近」观察表上：SetTrusted 只清了
// pending_requests、没清 nearby，已加好友的节点会长期滞留在附近列表。
// 「附近」功能删除后，这张表只由打开库时的幂等清理负责，本条用例只守
// 审批语义本身。
func TestSetTrustedClearsPending(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.AddPending(ctx, PendingRequest{PeerID: "p1", Reason: "DHT 发现"}); err != nil {
		t.Fatalf("add pending: %v", err)
	}
	if list, _ := d.ListPending(ctx); len(list) != 1 {
		t.Fatalf("待审批应含 p1，实际 %+v", list)
	}
	if err := d.SetTrusted(ctx, "p1", true); err != nil {
		t.Fatalf("set trusted: %v", err)
	}
	if list, _ := d.ListPending(ctx); len(list) != 0 {
		t.Fatalf("审批通过后待审批应被清空，实际 %+v", list)
	}

	// 撤销信任（删除好友）不阻塞后续重新申请：删除不是拉黑。
	if err := d.SetTrusted(ctx, "p1", false); err != nil {
		t.Fatalf("untrust p1: %v", err)
	}
	if err := d.AddPending(ctx, PendingRequest{PeerID: "p1", Reason: "重新申请"}); err != nil {
		t.Fatalf("删除后重新申请应可进入待审批: %v", err)
	}
	if list, _ := d.ListPending(ctx); len(list) != 1 || list[0].PeerID != "p1" {
		t.Fatalf("重新申请应进入待审批，实际 %+v", list)
	}
}

// TestPruneSelf 地址簿里指向「本机自己」的残留行必须被清理。
//
// 场景来源：身份文件路径曾解析到进程 CWD（Windows 服务模式下 SCM 把工作目录
// 设为 System32，节点在那里建了一个全新身份），PeerID 与虚拟 IP 双双漂移；
// 漂移前后两个身份共用同一个 lanet.db，于是 pending_requests / unfriended /
// peers 里留下了「现在等于自己」的行。控制台据此把它当陌生节点展示，
// 用户点「申请连接」又被自我保护拦下，报一句看不懂的「这就是本机节点 ID」。
func TestPruneSelf(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const self = "12D3KooWSELFTEST00000000000000000000000000000"
	const other = "12D3KooWOTHERTEST0000000000000000000000000"

	// 造脏数据：自己与正常节点各一条，覆盖待审批 / 墓碑 / 地址簿。
	if err := d.AddPending(ctx, PendingRequest{PeerID: self, Name: "漂移前的本机身份", Reason: "DHT 发现"}); err != nil {
		t.Fatalf("add pending self: %v", err)
	}
	if err := d.AddUnfriended(ctx, self, ""); err != nil {
		t.Fatalf("add unfriended self: %v", err)
	}
	if err := d.AddPending(ctx, PendingRequest{PeerID: other}); err != nil {
		t.Fatalf("add pending other: %v", err)
	}
	// peers / peer_addrs 直接写库，构造「自己仍留在地址簿里」的现场。
	if _, err := d.db.ExecContext(ctx, `
INSERT INTO peers (peer_id, trusted, approved, first_seen, last_seen) VALUES (?, 1, 1, ?, ?)`,
		self, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed self peer: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO peer_addrs (peer_id, addr) VALUES (?, ?)`,
		self, "/ip4/2.2.2.2/tcp/4001"); err != nil {
		t.Fatalf("seed self peer_addr: %v", err)
	}

	n, err := d.PruneSelf(ctx, self)
	if err != nil {
		t.Fatalf("prune self: %v", err)
	}
	if n != 3 {
		t.Fatalf("应清掉 pending_requests/unfriended/peers 各一行，实际 %d", n)
	}

	// 自己：三张表全无痕迹。
	if list, _ := d.ListPending(ctx); len(list) != 1 || list[0].PeerID != other {
		t.Fatalf("待审批应只剩 other，实际 %+v", list)
	}
	if ok, _ := d.IsUnfriended(ctx, self); ok {
		t.Fatal("指向自己的墓碑应被清除（否则本机会拒绝自己的握手）")
	}
	if p, _ := d.GetPeer(ctx, self); p != nil {
		t.Fatalf("指向自己的地址簿条目应被删除，实际 %+v", p)
	}
	if addrs, _ := d.Addrs(ctx, self); len(addrs) != 0 {
		t.Fatalf("peer_addrs 应随 peers 级联删除，实际 %+v", addrs)
	}
	// 正常节点不受影响。
	if list, _ := d.ListPending(ctx); len(list) != 1 {
		t.Fatalf("正常节点的待审批不应被误删，实际 %+v", list)
	}

	// 空自身 ID 必须是 no-op——否则一次误传空串会清空整个地址簿。
	if n, err := d.PruneSelf(ctx, ""); err != nil || n != 0 {
		t.Fatalf("空自身 ID 应为 no-op，实际 n=%d err=%v", n, err)
	}
	if list, _ := d.ListPending(ctx); len(list) != 1 {
		t.Fatalf("no-op 调用不应删任何行，实际 %+v", list)
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
	// 墓碑不阻塞重新申请：对方的再次申请可以进入待审批（删除不是拉黑）。
	if err := d.AddPending(ctx, PendingRequest{PeerID: "x", Reason: "重新申请"}); err != nil {
		t.Fatalf("墓碑状态下待审批写入不应失败: %v", err)
	}
	// 清除墓碑（重新加回 / 告知送达后消费）。
	if err := d.ClearUnfriended(ctx, "x"); err != nil {
		t.Fatalf("clear unfriended: %v", err)
	}
	if ok, _ := d.IsUnfriended(ctx, "x"); ok {
		t.Fatal("清除后不应有墓碑")
	}
}
