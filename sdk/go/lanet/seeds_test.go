package lanet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
	"github.com/ayflying/pvn/pkg/serverless"
)

// newSeedTestClient 造一个只带地址簿的最小 Client。
//
// 种子接线的回调只依赖 c.peers / c.peerID / c.disc，不必起 libp2p host，
// 这样测试既快又不占端口。
func newSeedTestClient(t *testing.T, peerID string) (*Client, *peersdb.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	db, err := peersdb.Open(ctx, filepath.Join(t.TempDir(), "lanet.db"))
	if err != nil {
		t.Fatalf("打开临时地址簿失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := &Client{peerID: peerID, peers: db}
	return c, db
}

// TestSeedSettingsDefaults 默认值：群内开、全域关、上限 1000。
//
// 「全域默认关」是需求硬约束——它决定本机是否在私有 DHT 的全局 rendezvous
// key 上暴露自己，不能被无声改成默认开。
func TestSeedSettingsDefaults(t *testing.T) {
	c, _ := newSeedTestClient(t, "self")
	ctx := context.Background()

	s, err := c.SeedSettings(ctx)
	if err != nil {
		t.Fatalf("读取默认设置失败: %v", err)
	}
	if !s.GroupEnabled {
		t.Error("群内种子共享应默认开启")
	}
	if s.GlobalEnabled {
		t.Error("全域种子必须默认关闭")
	}
	if s.GlobalLimit != peersdb.DefaultGlobalSeedLimit {
		t.Errorf("全域上限默认应为 %d，实际 %d", peersdb.DefaultGlobalSeedLimit, s.GlobalLimit)
	}
}

// TestSeedSettingsRoundTrip 设置落库并能读回；越界值被收敛。
func TestSeedSettingsRoundTrip(t *testing.T) {
	c, _ := newSeedTestClient(t, "self")
	ctx := context.Background()

	if _, err := c.SetSeedSettings(ctx, SeedSettings{GroupEnabled: false, GlobalEnabled: true, GlobalLimit: 5}); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}
	got, err := c.SeedSettings(ctx)
	if err != nil {
		t.Fatalf("读回设置失败: %v", err)
	}
	if got.GroupEnabled || !got.GlobalEnabled || got.GlobalLimit != 5 {
		t.Fatalf("设置往返不一致：%+v", got)
	}

	// limit 非正数回落默认；超大值被天花板收住。
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, peersdb.DefaultGlobalSeedLimit},
		{-3, peersdb.DefaultGlobalSeedLimit},
		{seedLimitCeiling + 1, seedLimitCeiling},
	} {
		if _, err := c.SetSeedSettings(ctx, SeedSettings{GlobalLimit: tc.in}); err != nil {
			t.Fatalf("保存 limit=%d 失败: %v", tc.in, err)
		}
		got, err := c.SeedSettings(ctx)
		if err != nil {
			t.Fatalf("读回 limit=%d 失败: %v", tc.in, err)
		}
		if got.GlobalLimit != tc.want {
			t.Errorf("limit=%d 应收敛到 %d，实际 %d", tc.in, tc.want, got.GlobalLimit)
		}
	}
}

// TestSeedScopeOfMapping 范围字符串必须双向对得上，未知值一律拒绝。
//
// 拒绝而不是回落：回落意味着「拼错的 scope 会静默写进另一张表」，
// 那正好破坏「两张表物理隔离」这条底线。
func TestSeedScopeOfMapping(t *testing.T) {
	for _, ok := range []string{serverless.SeedScopeGroup, serverless.SeedScopeGlobal} {
		sc, valid := seedScopeOf(ok)
		if !valid {
			t.Fatalf("范围 %q 应有效", ok)
		}
		if string(sc) != ok {
			t.Errorf("范围映射不一致：%q → %q", ok, string(sc))
		}
	}
	for _, bad := range []string{"", "bogus", "GROUP", "global_seeds"} {
		if _, valid := seedScopeOf(bad); valid {
			t.Errorf("范围 %q 应被判为无效", bad)
		}
	}
}

// TestSeedSourceOnlyVerified 出向只分享已验证的种子（验证门）。
//
// 未验证的条目是「别人转发来、本机从没拨通」的地址，外发等于替谣言背书。
func TestSeedSourceOnlyVerified(t *testing.T) {
	c, db := newSeedTestClient(t, "self-peer")
	ctx := context.Background()

	const (
		verifiedID = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
		rawID      = "12D3KooWQP5ZLg1sjmHJzEHJyZU6zMidVWHqWfApXDMuGdjoBkUB"
		selfID     = "self-peer"
	)
	addrs := []string{"/ip4/9.9.9.9/tcp/4001"}
	for _, id := range []string{verifiedID, rawID, selfID} {
		if _, err := db.UpsertSeed(ctx, peersdb.SeedScopeGroup, peersdb.Seed{
			PeerID: id, Addrs: addrs, Source: "exchange",
		}); err != nil {
			t.Fatalf("写种子 %s 失败: %v", id, err)
		}
	}
	// 只有 verifiedID 被本机拨通过。
	if err := db.NoteSeedDialResult(ctx, peersdb.SeedScopeGroup, verifiedID, true); err != nil {
		t.Fatalf("标记拨通失败: %v", err)
	}

	s := SeedSettings{GroupEnabled: true, GlobalLimit: 100}
	src := c.seedSourceFn(db, s)
	got := src(serverless.SeedScopeGroup, 64)
	if len(got) != 1 || got[0].PeerID != verifiedID {
		t.Fatalf("只应分享 1 条已验证种子（且不含本机自己），实际 %+v", got)
	}

	// 关掉范围 → 一条都不给。
	if off := c.seedSourceFn(db, SeedSettings{}); len(off(serverless.SeedScopeGroup, 64)) != 0 {
		t.Error("范围关闭时不应分享任何种子")
	}
}

// TestSeedMergeStoresUnverifiedAndEvicts 入向合并：一律未验证入库 + 容量闸生效。
func TestSeedMergeStoresUnverifiedAndEvicts(t *testing.T) {
	c, db := newSeedTestClient(t, "self-peer")
	ctx := context.Background()

	settings := SeedSettings{GlobalEnabled: true, GlobalLimit: 3}
	merge := c.seedMergeFn(db, settings)

	ids := []string{
		"12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p",
		"12D3KooWQP5ZLg1sjmHJzEHJyZU6zMidVWHqWfApXDMuGdjoBkUB",
		"12D3KooWGuwB2SpnvuzXBC1HpXrnwNPJC45GSMf2EMzWK9VWow8h",
		"12D3KooWM9sL1nQ4mZvKp3wXc7TbYdRfHgAe2UsJkQnPvBzXy1",
		"12D3KooWL8rTvNbQwEfYdGsHjKmZpAxCv3UeRtYuIoPaSdFgH2",
	}
	records := make([]serverless.SeedRecord, 0, len(ids))
	for _, id := range ids {
		records = append(records, serverless.SeedRecord{PeerID: id, Addrs: []string{"/ip4/9.9.9.9/tcp/4001"}})
	}
	merge(serverless.SeedScopeGlobal, "from-peer", records)

	n, err := db.CountSeeds(ctx, peersdb.SeedScopeGlobal)
	if err != nil {
		t.Fatalf("统计种子失败: %v", err)
	}
	if n != settings.GlobalLimit {
		t.Fatalf("容量闸应把全域表压到 %d 条，实际 %d", settings.GlobalLimit, n)
	}

	// 合并进来的必须是未验证状态（对端自报的 Public 不采信）。
	seeds, err := db.ListSeeds(ctx, peersdb.SeedScopeGlobal, 0)
	if err != nil {
		t.Fatalf("列出种子失败: %v", err)
	}
	for _, sd := range seeds {
		if sd.Verified() {
			t.Errorf("交换来的种子不该直接是已验证状态：%+v", sd)
		}
	}

	// 跨范围注入：在群内通道里声明 global 的记录，连表都不该碰到。
	before, _ := db.CountSeeds(ctx, peersdb.SeedScopeGroup)
	merge(serverless.SeedScopeGroup, "from-peer", []serverless.SeedRecord{
		{PeerID: ids[0], Addrs: []string{"/ip4/9.9.9.9/tcp/4001"}},
	})
	after, _ := db.CountSeeds(ctx, peersdb.SeedScopeGroup)
	if after != before {
		t.Errorf("群内范围关闭时不应写入任何记录：%d → %d", before, after)
	}
}

// TestParseBoolSetting 设置值解析要宽松但不能把不认识的值当假。
func TestParseBoolSetting(t *testing.T) {
	for _, v := range []string{"1", "true", "True", "on", "yes"} {
		if !parseBoolSetting(v, false) {
			t.Errorf("%q 应解析为真", v)
		}
	}
	for _, v := range []string{"0", "false", "off", "no"} {
		if parseBoolSetting(v, true) {
			t.Errorf("%q 应解析为假", v)
		}
	}
	// 空值与无法识别的值统统回落传入的默认值。
	if !parseBoolSetting("", true) || parseBoolSetting("", false) {
		t.Error("空值应回落默认值")
	}
	if !parseBoolSetting("随便写的", true) || parseBoolSetting("随便写的", false) {
		t.Error("无法识别的值应回落默认值")
	}
}

// TestSeedSourceDisabledByGlobalSwitch 全域关着时全域的回调拿不到数据。
//
// 这条是「全域开关」的行为闸：默认关闭状态下，既不该分享全域种子，
// 也不该从 DHT 路由表拉交换对象。
func TestSeedSourceDisabledByGlobalSwitch(t *testing.T) {
	c, db := newSeedTestClient(t, "self-peer")
	ctx := context.Background()

	const id = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
	if _, err := db.UpsertSeed(ctx, peersdb.SeedScopeGlobal, peersdb.Seed{
		PeerID: id, Addrs: []string{"/ip4/9.9.9.9/tcp/4001"}, Source: "exchange",
	}); err != nil {
		t.Fatalf("写全域种子失败: %v", err)
	}
	if err := db.NoteSeedDialResult(ctx, peersdb.SeedScopeGlobal, id, true); err != nil {
		t.Fatalf("标记拨通失败: %v", err)
	}

	off := SeedSettings{GlobalEnabled: false, GroupEnabled: true}
	if got := c.seedSourceFn(db, off)(serverless.SeedScopeGlobal, 64); len(got) != 0 {
		t.Errorf("全域关闭时不应分享全域种子，实际 %+v", got)
	}
	if got := c.seedCandidateFn(db, off)(ctx, 8); len(got) != 0 {
		t.Errorf("全域关闭时不应把全域种子当中继候选，实际 %+v", got)
	}
	if got := c.seedPeersFn(off)(serverless.SeedScopeGlobal, 3); len(got) != 0 {
		t.Errorf("全域关闭时不应取交换对象，实际 %+v", got)
	}

	on := SeedSettings{GlobalEnabled: true}
	if got := c.seedSourceFn(db, on)(serverless.SeedScopeGlobal, 64); len(got) != 1 {
		t.Errorf("全域开启后应能分享 1 条，实际 %+v", got)
	}
}

// TestSeedWiringEndToEnd 真机路径：New() 后种子交换已装配、接口可读。
//
// 这条覆盖「配置初值 → New → 运行期字段 → 控制台接口」整链，防止接线被
// 后续改动悄悄摘掉（编译通过但功能没了是这类改动的典型失败模式）。
func TestSeedWiringEndToEnd(t *testing.T) {
	c := newStandaloneClient(t, "seed-wire", "grp-seed-wire", filepath.Join(t.TempDir(), "s.db"))
	if c.peers == nil {
		t.Fatal("Standalone 模式应打开地址簿")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := c.SeedSettings(ctx)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if !s.GroupEnabled || s.GlobalEnabled {
		t.Fatalf("默认设置应为「群内开、全域关」，实际 %+v", s)
	}
	if _, err := c.SetSeedSettings(ctx, SeedSettings{GlobalEnabled: true, GlobalLimit: 12}); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}
	got, err := c.SeedSettings(ctx)
	if err != nil {
		t.Fatalf("读回设置失败: %v", err)
	}
	if !got.GlobalEnabled || got.GlobalLimit != 12 {
		t.Fatalf("设置未生效：%+v", got)
	}
}

// TestHasPublicAddr 公网地址判定：只认真正的全局单播地址。
//
// 这条判据决定「谁能进种子表」，宽松一点就会把 NAT 后的私网地址收进来，
// 占满名额却谁也拨不通。
func TestHasPublicAddr(t *testing.T) {
	yes := []string{
		"/ip4/9.9.9.9/tcp/4001",
		"/ip4/43.136.124.167/tcp/4001",
		"/ip6/2408:824e:1592:7d80::2b1/tcp/4001",
	}
	for _, a := range yes {
		if !hasPublicAddr([]string{a}) {
			t.Errorf("%s 应判为含公网地址", a)
		}
	}
	// 列表里掺着私网地址也算（Addrs 是 []string，逐条独立判定）。
	if !hasPublicAddr([]string{"/ip4/192.168.50.176/tcp/4001", "/ip4/9.9.9.9/tcp/4001"}) {
		t.Error("列表含公网地址时应判为 true")
	}
	no := []string{
		"",
		"不是地址",
		"/ip4/127.0.0.1/tcp/4001",
		"/ip6/::1/tcp/4001",
		"/ip4/169.254.1.2/tcp/4001",
		"/ip4/0.0.0.0/tcp/4001",
		"/ip4/192.168.64.2/tcp/4001", // docker 内网
		"/ip4/10.7.204.166/tcp/4001", // 虚拟网
		"/ip4/172.17.0.2/tcp/4001",   // 容器网桥
		"/ip6/fd00::1/tcp/4001",      // ULA
		"/dns4/example.com/tcp/4001", // 非 ip4/ip6 形态一律不算
	}
	for _, a := range no {
		if hasPublicAddr([]string{a}) {
			t.Errorf("%s 不该判为公网地址", a)
		}
	}
}

// TestHarvestSeedCandidatesGuards 自举采集的边界：范围关闭 / 无对端时不写任何东西。
//
// 空库 + 未连任何公网对端的情形下必须安静返回 0（不能 panic、不能凭空造记录）。
func TestHarvestSeedCandidatesGuards(t *testing.T) {
	c := newStandaloneClient(t, "harvest", "grp-harvest", filepath.Join(t.TempDir(), "h.db"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 范围关闭 → 0，且库里仍然为空。
	n := c.harvestSeedCandidates(ctx, c.peers, SeedSettings{}, peersdb.SeedScopeGroup)
	if n != 0 {
		t.Errorf("范围关闭时应收 0 条，实际 %d", n)
	}
	// 范围开启但没有「已连上且带公网地址」的对端 → 仍然是 0。
	n = c.harvestSeedCandidates(ctx, c.peers, SeedSettings{GroupEnabled: true}, peersdb.SeedScopeGroup)
	if n != 0 {
		t.Errorf("没有合格对端时应收 0 条，实际 %d", n)
	}
	total, err := c.peers.CountSeeds(ctx, peersdb.SeedScopeGroup)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if total != 0 {
		t.Errorf("不该凭空写入记录，实际 %d 条", total)
	}
}
