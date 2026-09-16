package peersdb

import (
	"context"
	"testing"
	"time"
)

// TestSeedScopeIsolation 两个范围的种子必须物理隔离：写一边，另一边看不到。
func TestSeedScopeIsolation(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	g := Seed{PeerID: "12D3KooGroupSeed", Name: "group-seed", Addrs: []string{"/ip4/1.1.1.1/tcp/4001"}}
	if ok, err := d.UpsertSeed(ctx, SeedScopeGroup, g); err != nil || !ok {
		t.Fatalf("写入群内种子失败：ok=%v err=%v", ok, err)
	}
	gl := Seed{PeerID: "12D3KooGlobalSeed", Name: "global-seed", Addrs: []string{"/ip4/2.2.2.2/tcp/4001"}}
	if ok, err := d.UpsertSeed(ctx, SeedScopeGlobal, gl); err != nil || !ok {
		t.Fatalf("写入全域种子失败：ok=%v err=%v", ok, err)
	}

	gn, _ := d.CountSeeds(ctx, SeedScopeGroup)
	gn2, _ := d.CountSeeds(ctx, SeedScopeGlobal)
	if gn != 1 || gn2 != 1 {
		t.Fatalf("隔离失败：group=%d global=%d", gn, gn2)
	}

	// 跨范围互查必须查不到。
	if s, err := d.GetSeed(ctx, SeedScopeGlobal, g.PeerID); err != nil || s != nil {
		t.Errorf("全域表不该看到群内种子：%+v err=%v", s, err)
	}
	if s, err := d.GetSeed(ctx, SeedScopeGroup, gl.PeerID); err != nil || s != nil {
		t.Errorf("群内表不该看到全域种子：%+v err=%v", s, err)
	}

	// 未登记的范围一律报错，绝不回退到默认表。
	if _, err := d.UpsertSeed(ctx, SeedScope("bogus"), g); err == nil {
		t.Error("未知范围应报错")
	}
	if _, err := d.ListSeeds(ctx, SeedScope(""), 0); err == nil {
		t.Error("空范围应报错")
	}
	if _, err := d.CountSeeds(ctx, SeedScope("global_seeds")); err == nil {
		t.Error("把表名当范围应报错（白名单必须严格）")
	}
}

// TestSeedRejectsFriend 好友永不入种子表：已有信任关系的节点写入被静默跳过，
// 存量的「先当种子、后被审批为好友」的行由 PruneTrustedSeeds 清掉。
func TestSeedRejectsFriend(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const pid = "12D3KooFriendly"

	// 先当种子写进去。
	if ok, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{PeerID: pid, Addrs: []string{"/ip4/1.1.1.1/tcp/4001"}}); err != nil || !ok {
		t.Fatalf("写入种子失败：ok=%v err=%v", ok, err)
	}

	// 变成好友（已信任）。
	if err := d.UpsertPeer(ctx, Peer{PeerID: pid, Name: "friend"}, nil); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	if err := d.SetTrusted(ctx, pid, true); err != nil {
		t.Fatalf("set trusted: %v", err)
	}

	// 再写一次应被拦。
	if ok, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{PeerID: pid, Addrs: []string{"/ip4/9.9.9.9/tcp/4001"}}); err != nil {
		t.Fatalf("好友写入不该报错（应静默跳过）：%v", err)
	} else if ok {
		t.Error("好友不该被写进种子表")
	}

	// 存量行被自愈清理。
	n, err := d.PruneTrustedSeeds(ctx)
	if err != nil {
		t.Fatalf("prune trusted seeds: %v", err)
	}
	if n != 1 {
		t.Errorf("应清理 1 条存量好友种子，实际 %d", n)
	}
	if s, _ := d.GetSeed(ctx, SeedScopeGroup, pid); s != nil {
		t.Error("清理后不该还在种子表里")
	}
}

// TestSeedVerificationGate 验证门：新种子未验证；拨通一次才通过；失败不加计数。
func TestSeedVerificationGate(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const pid = "12D3KooVerifyMe"

	if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{PeerID: pid, Addrs: []string{"/ip4/1.1.1.1/tcp/4001"}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s, err := d.GetSeed(ctx, SeedScopeGroup, pid)
	if err != nil || s == nil {
		t.Fatalf("get: %+v err=%v", s, err)
	}
	if s.Verified() {
		t.Error("刚写入的种子不该已通过验证门")
	}

	// 失败一次：计数不变。
	if err := d.NoteSeedDialResult(ctx, SeedScopeGroup, pid, false); err != nil {
		t.Fatalf("note fail: %v", err)
	}
	if s, _ = d.GetSeed(ctx, SeedScopeGroup, pid); s.OKCount != 0 || s.Verified() {
		t.Errorf("失败不该改变验证状态：ok_count=%d", s.OKCount)
	}

	// 成功一次：通过验证门。
	if err := d.NoteSeedDialResult(ctx, SeedScopeGroup, pid, true); err != nil {
		t.Fatalf("note ok: %v", err)
	}
	s, _ = d.GetSeed(ctx, SeedScopeGroup, pid)
	if s.OKCount != 1 || !s.Verified() || s.LastOK.IsZero() {
		t.Errorf("成功一次应通过验证门：ok_count=%d verified=%v last_ok=%v", s.OKCount, s.Verified(), s.LastOK)
	}
}

// TestSeedStaleWriteIgnored 乱序到达的旧数据不得覆盖更新的记录。
func TestSeedStaleWriteIgnored(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const pid = "12D3KooStale"
	now := time.Now()

	if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
		PeerID: pid, Addrs: []string{"/ip4/8.8.8.8/tcp/4001"}, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("新数据写入失败：%v", err)
	}
	// 旧数据：地址不同、时间早一小时。
	if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
		PeerID: pid, Addrs: []string{"/ip4/7.7.7.7/tcp/4001"}, UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("旧数据写入不该报错：%v", err)
	}
	s, _ := d.GetSeed(ctx, SeedScopeGroup, pid)
	if len(s.Addrs) != 1 || s.Addrs[0] != "/ip4/8.8.8.8/tcp/4001" {
		t.Errorf("旧数据覆盖了新数据：%v", s.Addrs)
	}
}

// TestSeedPublicFlagOnlyRises 可达标记只升不降，避免探测抖动来回翻。
func TestSeedPublicFlagOnlyRises(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const pid = "12D3KooPublic"

	if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{PeerID: pid, PublicReachable: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkSeedPublic(ctx, SeedScopeGroup, pid, false); err != nil {
		t.Fatalf("mark false: %v", err)
	}
	if s, _ := d.GetSeed(ctx, SeedScopeGroup, pid); !s.PublicReachable {
		t.Error("可达标记不该被降下去")
	}
	// 再 upsert 一个 PublicReachable=false 的新数据，也不该把标记清掉（OR 语义）。
	if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
		PeerID: pid, PublicReachable: false, UpdatedAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	if s, _ := d.GetSeed(ctx, SeedScopeGroup, pid); !s.PublicReachable {
		t.Error("upsert 不该把可达标记清掉（应为 OR 语义）")
	}
}

// TestEvictSeeds 淘汰顺序：未验证 → last_ok 最旧 → last_seen 最旧。
func TestEvictSeeds(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	now := time.Now()

	// A 最好（成功多、最近拨通）；B 次之；C 从未拨通（虽最近见到）；
	// D 验证过但 last_ok 很旧。
	type in struct {
		id       string
		okCount  int
		lastOK   time.Time
		lastSeen time.Time
	}
	items := []in{
		{"A", 5, now.Add(-1 * time.Hour), now.Add(-1 * time.Hour)},
		{"C", 0, time.Time{}, now.Add(-1 * time.Minute)},
		{"B", 2, now.Add(-2 * time.Hour), now.Add(-2 * time.Hour)},
		{"D", 1, now.Add(-72 * time.Hour), now.Add(-72 * time.Hour)},
	}
	for _, it := range items {
		if ok, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
			PeerID: it.id, Addrs: []string{"/ip4/1.1.1.1/tcp/4001"}, LastSeen: it.lastSeen,
		}); err != nil || !ok {
			t.Fatalf("写入 %s 失败：ok=%v err=%v", it.id, ok, err)
		}
		for i := 0; i < it.okCount; i++ {
			if err := d.NoteSeedDialResult(ctx, SeedScopeGroup, it.id, true); err != nil {
				t.Fatalf("note %s: %v", it.id, err)
			}
		}
		// NoteSeedDialResult 会把 last_seen 刷成 now，这里按用例语义回写。
		if _, err := d.db.ExecContext(ctx,
			`UPDATE group_seeds SET last_ok = ?, last_seen = ? WHERE peer_id = ?`,
			nullableTime(it.lastOK), it.lastSeen, it.id); err != nil {
			t.Fatalf("回写 %s 时间: %v", it.id, err)
		}
	}

	// 压到 2 条：应删掉 C（未验证）与 D（last_ok 最旧）。
	n, err := d.EvictSeeds(ctx, SeedScopeGroup, 2)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n != 2 {
		t.Errorf("应淘汰 2 条，实际 %d", n)
	}
	left, _ := d.ListSeeds(ctx, SeedScopeGroup, 0)
	if len(left) != 2 {
		t.Fatalf("剩余应 2 条，实际 %d：%v", len(left), left)
	}
	got := map[string]bool{}
	for _, s := range left {
		got[s.PeerID] = true
	}
	if !got["A"] || !got["B"] {
		t.Errorf("应保留 A 与 B，实际剩下 %v", got)
	}

	// limit <= 0 不淘汰。
	if n, err := d.EvictSeeds(ctx, SeedScopeGroup, 0); err != nil || n != 0 {
		t.Errorf("limit=0 不该淘汰：n=%d err=%v", n, err)
	}
}

// TestEvictSeedsGlobalScope 全域表同样能淘汰，且不影响群内表。
func TestEvictSeedsGlobalScope(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"g1", "g2", "g3"} {
		if _, err := d.UpsertSeed(ctx, SeedScopeGlobal, Seed{PeerID: id}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{PeerID: id}); err != nil {
			t.Fatalf("upsert group %s: %v", id, err)
		}
	}
	n, err := d.EvictSeeds(ctx, SeedScopeGlobal, 1)
	if err != nil {
		t.Fatalf("evict global: %v", err)
	}
	if n != 2 {
		t.Errorf("全域应淘汰 2 条，实际 %d", n)
	}
	gn, _ := d.CountSeeds(ctx, SeedScopeGroup)
	if gn != 3 {
		t.Errorf("淘汰全域不该影响群内表：group=%d", gn)
	}
}

// TestSettings 设置存储的读写删。
func TestSettings(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if v, ok, err := d.GetSetting(ctx, SettingGlobalSeedsEnabled); err != nil || ok || v != "" {
		t.Errorf("未设置应返回 (\"\", false, nil)：v=%q ok=%v err=%v", v, ok, err)
	}
	if err := d.SetSetting(ctx, SettingGlobalSeedsEnabled, "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := d.GetSetting(ctx, SettingGlobalSeedsEnabled)
	if err != nil || !ok || v != "1" {
		t.Errorf("读取失败：v=%q ok=%v err=%v", v, ok, err)
	}
	// 覆盖写。
	if err := d.SetSetting(ctx, SettingGlobalSeedsEnabled, "0"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if v, _, _ := d.GetSetting(ctx, SettingGlobalSeedsEnabled); v != "0" {
		t.Errorf("覆盖写失败：%q", v)
	}
	if err := d.SetSetting(ctx, SettingGlobalSeedsLimit, "1000"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	all, err := d.ListSettings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 || all[SettingGlobalSeedsLimit] != "1000" {
		t.Errorf("list 结果不对：%v", all)
	}
	if err := d.DeleteSetting(ctx, SettingGlobalSeedsEnabled); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := d.GetSetting(ctx, SettingGlobalSeedsEnabled); ok {
		t.Error("删除后不该还在")
	}
	if err := d.SetSetting(ctx, "", "x"); err == nil {
		t.Error("空 key 应报错")
	}
}

// TestPruneSelfCoversSeeds 身份漂移残留：种子表里指向自己的行也要被清掉。
func TestPruneSelfCoversSeeds(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	const self = "12D3KooSelf"

	for _, scope := range []SeedScope{SeedScopeGroup, SeedScopeGlobal} {
		if _, err := d.UpsertSeed(ctx, scope, Seed{PeerID: self}); err != nil {
			t.Fatalf("upsert self: %v", err)
		}
	}
	if n, err := d.PruneSelf(ctx, self); err != nil || n < 2 {
		t.Errorf("应至少清理 2 条（两张种子表各 1）：n=%d err=%v", n, err)
	}
	for _, scope := range []SeedScope{SeedScopeGroup, SeedScopeGlobal} {
		if s, _ := d.GetSeed(ctx, scope, self); s != nil {
			t.Errorf("%s 表里仍有本机自己", scope)
		}
	}
}

// nullableTime 把零值时间转成 NULL（SQLite 里 NULL 表示「从未」）。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// TestPruneStaleSeeds 陈旧清理：未验证 7 天 / 已验证 30 天，两档各自生效。
//
// 「自动剔除长期不上线」不能只靠容量溢出：种子表没满时脏记录会一直躺着，
// 下次要当种子分发出去仍然会被验证门挡住，但名额已被长期占用。
func TestPruneStaleSeeds(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	now := time.Now()

	fresh := "12D3KooWFreshUnverified"
	withinGrace := "12D3KooWGraceUnverified"
	stale := "12D3KooWStaleUnverified"
	for _, tc := range []struct {
		id   string
		seen time.Time
	}{
		{fresh, now},
		{withinGrace, now.Add(-6 * 24 * time.Hour)},
		{stale, now.Add(-10 * 24 * time.Hour)},
	} {
		if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
			PeerID: tc.id, Addrs: []string{"/ip4/1.1.1.1/tcp/4001"},
			FirstSeen: tc.seen, LastSeen: tc.seen, UpdatedAt: tc.seen,
		}); err != nil {
			t.Fatalf("写入种子 %s 失败：%v", tc.id, err)
		}
	}

	liveVerified := "12D3KooWLiveVerified"
	oldVerified := "12D3KooWOldVerified"
	for _, id := range []string{liveVerified, oldVerified} {
		if _, err := d.UpsertSeed(ctx, SeedScopeGroup, Seed{
			PeerID: id, Addrs: []string{"/ip4/1.1.1.2/tcp/4001"},
		}); err != nil {
			t.Fatalf("写入种子 %s 失败：%v", id, err)
		}
		if err := d.NoteSeedDialResult(ctx, SeedScopeGroup, id, true); err != nil {
			t.Fatalf("标记 %s 拨通失败：%v", id, err)
		}
	}
	// NoteSeedDialResult 只会写「现在」，把其中一条的 last_ok 挪到 40 天前。
	if _, err := d.db.ExecContext(ctx,
		`UPDATE group_seeds SET last_ok = ? WHERE peer_id = ?`,
		now.Add(-40*24*time.Hour), oldVerified); err != nil {
		t.Fatalf("调整 last_ok 失败：%v", err)
	}

	n, err := d.PruneStaleSeeds(ctx, SeedScopeGroup, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 2 {
		t.Errorf("应清理 2 条（1 条陈旧未验证 + 1 条长期未上线的已验证），实际 %d", n)
	}
	for _, id := range []string{fresh, withinGrace, liveVerified} {
		got, err := d.GetSeed(ctx, SeedScopeGroup, id)
		if err != nil {
			t.Fatalf("查询种子 %s 失败：%v", id, err)
		}
		if got == nil {
			t.Errorf("种子 %s 不该被清理", id)
		}
	}
	for _, id := range []string{stale, oldVerified} {
		got, err := d.GetSeed(ctx, SeedScopeGroup, id)
		if err != nil {
			t.Fatalf("查询种子 %s 失败：%v", id, err)
		}
		if got != nil {
			t.Errorf("种子 %s 应已被清理", id)
		}
	}

	// 该档传 0 表示不清理。
	before, err := d.CountSeeds(ctx, SeedScopeGroup)
	if err != nil {
		t.Fatalf("统计失败：%v", err)
	}
	if n2, err := d.PruneStaleSeeds(ctx, SeedScopeGroup, 0, 0); err != nil || n2 != 0 {
		t.Errorf("传 0 应不清理任何记录：n=%d err=%v", n2, err)
	}
	after, err := d.CountSeeds(ctx, SeedScopeGroup)
	if err != nil {
		t.Fatalf("统计失败：%v", err)
	}
	if before != after {
		t.Errorf("传 0 时条数不该变化：%d → %d", before, after)
	}
}
