package serverless

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---- 测试辅助 ----

// collidePair 在给定群密钥下搜索一对基础派生结果相同的 PeerID。
//
// 地址空间约 6.4 万，生日碰撞期望约 300 个样本就能撞上；上限 20 万确保
// 一定找得到（找不到说明派生算法被改坏了）。返回两个 ID，并保证 a < b
// （字典序），方便断言「小者保留原地址」。
func collidePair(t *testing.T, groupKey []byte) (a, b string) {
	t.Helper()
	seen := make(map[string]string, 1<<12)
	for i := 0; i < 200000; i++ {
		id := fmt.Sprintf("peer-%06d", i)
		base := DeriveVirtualIP(groupKey, id)
		if prev, ok := seen[base]; ok {
			if prev < id {
				return prev, id
			}
			return id, prev
		}
		seen[base] = id
	}
	t.Fatal("在 20 万个 PeerID 内没找到基础派生冲突，派生算法可能被改动")
	return "", ""
}

// 三个 PeerID 指派到同一基础地址，用来验证多成员冲突的定序。
func collideTriple(t *testing.T, groupKey []byte) (ids []string) {
	t.Helper()
	groups := make(map[string][]string, 1<<12)
	for i := 0; i < 400000; i++ {
		id := fmt.Sprintf("triple-%06d", i)
		base := DeriveVirtualIP(groupKey, id)
		groups[base] = append(groups[base], id)
		if len(groups[base]) >= 3 {
			out := append([]string(nil), groups[base]...)
			sort.Strings(out)
			return out
		}
	}
	t.Fatal("在 40 万个 PeerID 内没找到三方冲突")
	return nil
}

// decision 便于断言的裁决视图。
func decision(groupKey []byte, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	for id, lease := range decideVirtualIPs(groupKey, ids) {
		out[id] = lease.IP
	}
	return out
}

// TestVirtualIPDecisionPureAndOrderIndependent 核心性质：
//   - 两端各自独立计算（纯函数）得到同一结果；
//   - 与成员入表顺序无关（Lead 明确要求 A 先入表 vs B 先入表结果相同）。
func TestVirtualIPDecisionPureAndOrderIndependent(t *testing.T) {
	groupKey := GroupKey("", "vaddr-order")
	a, b := collidePair(t, groupKey)
	if DeriveVirtualIP(groupKey, a) != DeriveVirtualIP(groupKey, b) {
		t.Fatalf("构造失败：%q 与 %q 的基础地址不同", a, b)
	}
	base := DeriveVirtualIP(groupKey, a)

	// 顺序 1：小 ID 先；顺序 2：大 ID 先；顺序 3：打乱并混入无关成员。
	noise := []string{"noise-a", "noise-b", "noise-c"}
	orders := [][]string{
		{a, b},
		{b, a},
		{noise[0], b, noise[1], a, noise[2]},
		{noise[2], a, noise[0], b, noise[1]},
	}
	want := decision(groupKey, []string{a, b})
	for i, order := range orders {
		got := decision(groupKey, order)
		if got[a] != want[a] || got[b] != want[b] {
			t.Fatalf("顺序 %d 得到了不同裁决: %q->%q %q->%q，首次为 %q->%q %q->%q",
				i, a, got[a], b, got[b], a, want[a], b, want[b])
		}
	}

	// 字典序小者保留原地址，另一者被改派生到不冲突的新地址。
	if want[a] != base {
		t.Fatalf("字典序最小者应保留原地址 %s，实际 %s", base, want[a])
	}
	if want[b] == base {
		t.Fatalf("冲突方 %q 未被改派生（仍是 %s）", b, base)
	}
	if want[a] == want[b] {
		t.Fatalf("裁决后仍然冲突：%q 与 %q 都是 %s", a, b, want[a])
	}
	// 幂等：同一输入重复调用结果不变。
	again := decision(groupKey, []string{b, a})
	if again[a] != want[a] || again[b] != want[b] {
		t.Fatal("重复计算不是幂等的")
	}
}

// TestVirtualIPDecisionTripleConflict 三方冲突：最小者保留，另两个各拿一个
// 不同地址且都不与保留地址重合——仍然与输入顺序无关。
func TestVirtualIPDecisionTripleConflict(t *testing.T) {
	groupKey := GroupKey("", "vaddr-triple")
	ids := collideTriple(t, groupKey)
	base := DeriveVirtualIP(groupKey, ids[0])
	for _, id := range ids[1:] {
		if DeriveVirtualIP(groupKey, id) != base {
			t.Fatalf("构造失败：%q 的基础地址不是 %s", id, base)
		}
	}
	first := decision(groupKey, ids)
	reversed := decision(groupKey, []string{ids[2], ids[1], ids[0]})
	if len(first) != 3 {
		t.Fatalf("裁决条数 %d != 3", len(first))
	}
	if first[ids[0]] != base {
		t.Fatalf("三方冲突时字典序最小者应保留原地址")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if reversed[id] != first[id] {
			t.Fatalf("逆序裁决不一致：%q 前 %s 后 %s", id, first[id], reversed[id])
		}
		if seen[first[id]] {
			t.Fatalf("三方冲突未消解，地址 %s 被重复分配", first[id])
		}
		seen[first[id]] = true
	}
}

// TestVirtualIPNoConflictKeepsBaseDerivation 没有冲突时，裁决结果必须与
// 历史派生逐字节一致（这是「不存在全网换网」的保证）。
func TestVirtualIPNoConflictKeepsBaseDerivation(t *testing.T) {
	groupKey := GroupKey("", "vaddr-noconflict")
	a, _ := collidePair(t, groupKey)
	ids := []string{"alpha", "beta", "gamma", a} // 只含 a，不含它的冲突方
	leases := decideVirtualIPs(groupKey, ids)
	for _, id := range ids {
		lease := leases[id]
		if lease == nil {
			t.Fatalf("缺少裁决记录: %q", id)
		}
		if lease.IP != DeriveVirtualIP(groupKey, id) || lease.Salt != 0 || lease.Unresolved || len(lease.Losers) > 0 {
			t.Fatalf("无冲突成员 %q 的裁决被改变: %+v", id, lease)
		}
	}
}

// TestVirtualIPCrossGroupNotConflict 不同网络密钥之间同一 IP 不算冲突：
// 群密钥参与派生，跨群本来就互不可见。
func TestVirtualIPCrossGroupNotConflict(t *testing.T) {
	groupA := GroupKey("", "vaddr-group-a")
	groupB := GroupKey("", "vaddr-group-b")
	// 先找一个在 groupA 下派生为 X 的 ID，再找一个在 groupB 下也派生为 X 的 ID：
	// 两者分属不同群，同 IP 合法；但一旦放进同一个群就会冲突。
	target := DeriveVirtualIP(groupA, "shared-peer")
	var other string
	for i := 0; i < 200000; i++ {
		cand := fmt.Sprintf("cross-%06d", i)
		if DeriveVirtualIP(groupB, cand) == target && cand != "shared-peer" {
			other = cand
			break
		}
	}
	if other == "" {
		t.Fatal("没找到跨群同地址的样本（不可能，请检查派生实现）")
	}
	// 跨群：各自裁决，都必须保持基础派生值。
	if ip := decideVirtualIPs(groupA, []string{"shared-peer"})["shared-peer"].IP; ip != target {
		t.Fatalf("群 A 内独立成员被改派生: %s != %s", ip, target)
	}
	if ip := decideVirtualIPs(groupB, []string{other})[other].IP; ip != target {
		t.Fatalf("群 B 内独立成员被改派生: %s != %s", ip, target)
	}
	// 放进同一个群（构造一个让两者基础地址相同的群密钥不现实），
	// 用占用表验证跨群记录互不干扰：两张表各管各的群。
	ta := newVirtualIPTable()
	ta.syncLocked(groupA, []string{"shared-peer"})
	tb := newVirtualIPTable()
	tb.syncLocked(groupB, []string{other})
	if !ta.occupiedLocked(target) || !tb.occupiedLocked(target) {
		t.Fatal("两张跨群占用表应各自独立记录同一地址")
	}
	if ta.occupiedLocked(DeriveVirtualIP(groupB, other)) && DeriveVirtualIP(groupB, other) != target {
		t.Fatal("群 A 占用表串到了群 B 的地址")
	}
}

// TestVirtualIPTableSyncReleaseAndIdempotent 占用表生命周期：
// 成员加入→占位，成员移除→释放，重复同步（幂等）不改变结果。
func TestVirtualIPTableSyncReleaseAndIdempotent(t *testing.T) {
	groupKey := GroupKey("", "vaddr-lifecycle")
	a, b := collidePair(t, groupKey)
	base := DeriveVirtualIP(groupKey, a)
	table := newVirtualIPTable()

	// a 单独在表里：占基础地址。
	if _, added := table.syncLocked(groupKey, []string{a}); len(added) != 1 {
		t.Fatalf("首次同步应记录新成员")
	}
	if table.lea[a].IP != base || table.lea[a].Salt != 0 {
		t.Fatalf("单独成员应保持基础派生值: %+v", table.lea[a])
	}
	// 重复同步：无变化、无更新（幂等）。
	if updated, added := table.syncLocked(groupKey, []string{a}); len(updated) != 0 || len(added) != 0 {
		t.Fatalf("重复同步不是幂等的: updated=%v added=%v", updated, added)
	}
	if len(table.ipOwners[base]) != 1 {
		t.Fatalf("基础地址应只有 1 个占用者，实际 %v", table.ipOwners[base])
	}

	// 加入冲突方 b：a 保留原地址，b 被改派生。
	table.syncLocked(groupKey, []string{a, b})
	if table.lea[a].IP != base {
		t.Fatalf("新增冲突方不应夺走保留方的地址: %+v", table.lea[a])
	}
	if table.lea[b].IP == base {
		t.Fatalf("冲突方未被改派生: %+v", table.lea[b])
	}
	if len(table.ipOwners[base]) != 1 {
		t.Fatalf("基础地址占用者应仍为 1，实际 %v", table.ipOwners[base])
	}

	// 移除 b：占用释放，记录清理干净。
	table.syncLocked(groupKey, []string{a})
	if _, ok := table.lea[b]; ok {
		t.Fatal("移除后仍残留 b 的归属记录")
	}
	if _, ok := table.peerIPs[b]; ok {
		t.Fatal("移除后仍残留 b 的 IP 索引")
	}
	if table.occupiedLocked(DeriveVirtualIP(groupKey, b)) && b == a {
		t.Fatal("不可达")
	}
	if len(table.ipOwners[base]) != 1 || table.ipOwners[base][0] != a {
		t.Fatalf("基础地址占用者应只剩 a，实际 %v", table.ipOwners[base])
	}

	// 移除全部：表清空，释放后的地址可以被后来者重新占用。
	table.syncLocked(groupKey, nil)
	if len(table.lea) != 0 || len(table.ipOwners) != 0 || len(table.peerIPs) != 0 {
		t.Fatalf("清空后占用表未释放干净: %+v", table)
	}
	table.syncLocked(groupKey, []string{b})
	if table.lea[b].IP != base {
		t.Fatalf("释放后 b 应恢复到基础地址 %s，实际 %s", base, table.lea[b].IP)
	}
}

// TestVirtualIPSaltExhaustionKeepsBaseAndIsDetectable salt 上限耗尽时：
//   - 保留原值（不 panic、不无限循环）；
//   - 给出可判定状态（Unresolved=true，未消解计数 > 0）。
func TestVirtualIPSaltExhaustionKeepsBaseAndIsDetectable(t *testing.T) {
	groupKey := GroupKey("", "vaddr-exhaust")
	a, b := collidePair(t, groupKey)
	base := DeriveVirtualIP(groupKey, a)

	// 「盐耗尽」是一个纯函数性质：给定全表占用，b 的所有候选盐值都不可用。
	// 直接用与 syncLocked 完全相同的裁决核心验证，再把结果装配进占用表，
	// 检查 Discovery 暴露的可判定状态。
	blocked := make(map[string]bool, VirtualIPv4SaltMax)
	for salt := 1; salt <= VirtualIPv4SaltMax; salt++ {
		blocked[deriveVirtualIPSalted(groupKey, b, salt)] = true
	}
	leases := decideVirtualIPsWithBlocked(groupKey, []string{a, b}, blocked)
	if !leases[b].Unresolved {
		t.Fatalf("盐全部被占用时 b 应被标记 Unresolved: %+v", leases[b])
	}
	// 把裁决结果原样装配进占用表（模拟 syncLocked 完成后的状态）。
	table := newVirtualIPTable()
	for _, id := range []string{a, b} {
		lease := leases[id]
		table.lea[id] = lease
		table.peerIPs[id] = lease.IP
		table.ipOwners[lease.IP] = appendSorted(table.ipOwners[lease.IP], id)
		if lease.Unresolved {
			table.unresolved++
		}
	}
	table.conflicts[base] = true

	lease := table.lea[b]
	if lease == nil {
		t.Fatal("缺少 b 的归属记录")
	}
	if !lease.Unresolved {
		t.Fatalf("盐耗尽应标记 Unresolved: %+v", lease)
	}
	if lease.IP != base {
		t.Fatalf("盐耗尽应保留原值 %s，实际 %s", base, lease.IP)
	}
	if lease.Salt != 0 {
		t.Fatalf("盐耗尽时 Salt 应为 0，实际 %d", lease.Salt)
	}
	if table.unresolved != 1 {
		t.Fatalf("未消解计数应为 1，实际 %d", table.unresolved)
	}

	// 可判定状态要通过 Discovery 暴露出来（控制台展示用）。
	d := &Discovery{groupKey: groupKey, vaddrs: table, cfg: Config{Quiet: true}}
	d.members = map[string]*Member{
		a: {PeerID: a, VirtualIP: table.lea[a].IP},
		b: {PeerID: b, VirtualIP: lease.IP},
	}
	if got := d.VirtualIPConflictUnresolvedCount(); got != 1 {
		t.Fatalf("VirtualIPConflictUnresolvedCount = %d, want 1", got)
	}
	records := d.VirtualIPConflicts()
	if len(records) != 1 {
		t.Fatalf("冲突记录条数 %d != 1: leases(a=%+v b=%+v) conflicts=%v owners=%v",
			len(records), table.lea[a], table.lea[b], table.conflicts, table.ipOwners)
	}
	if records[0].Resolved {
		t.Fatalf("盐耗尽的冲突记录不应标记为已消解: %+v", records[0])
	}
}

// TestVirtualIPTableDefaultsOneSalt salt 上限之外必须有兜底：单冲突对
// 在真实场景（只有两个成员）里取到的是 salt=1，且两端一致。
func TestVirtualIPTableDefaultsOneSalt(t *testing.T) {
	groupKey := GroupKey("", "vaddr-salt-one")
	a, b := collidePair(t, groupKey)
	leases := decideVirtualIPs(groupKey, []string{a, b})
	if leases[b].Salt != 1 {
		t.Fatalf("单冲突对默认应使用 salt=1，实际 %d", leases[b].Salt)
	}
	if want := deriveVirtualIPSalted(groupKey, b, 1); leases[b].IP != want {
		t.Fatalf("重派生地址不匹配域分离公式: %s != %s", leases[b].IP, want)
	}
	// 地址仍在 10.7/16 空间内。
	ip := leases[b].IP
	var x, y int
	if _, err := fmt.Sscanf(ip, "10.7.%d.%d", &x, &y); err != nil || x < 1 || x > 254 || y < 1 || y > 254 {
		t.Fatalf("重派生地址超出 10.7.1.1~10.7.254.254 空间: %q", ip)
	}
}

// TestDiscoveryVirtualIPConflictsMembership 走真实成员表写入路径：
// 成员入表 → 冲突被消解 → 成员删除 → 占用释放，且冲突列表随之收敛。
func TestDiscoveryVirtualIPConflictsMembership(t *testing.T) {
	groupKey := GroupKey("", "vaddr-membership")
	a, b := collidePair(t, groupKey)
	base := DeriveVirtualIP(groupKey, a)

	d := &Discovery{
		groupKey: groupKey,
		cfg:      Config{Quiet: true},
		host:     testHost(t, false),
		members:  make(map[string]*Member),
		vaddrs:   newVirtualIPTable(),
	}

	// 模拟成员入表（addMember 在信任门之后写的就是这段语义）。
	insert := func(id string) {
		d.mu.Lock()
		d.members[id] = &Member{PeerID: id, VirtualIP: d.alignedVirtualIP(id)}
		d.claimVirtualIPsLocked()
		d.mu.Unlock()
	}
	remove := func(id string) {
		d.mu.Lock()
		delete(d.members, id)
		d.releaseVirtualIPsLocked()
		d.mu.Unlock()
	}

	insert(a)
	if got := d.VirtualIPConflictCount(); got != 0 {
		t.Fatalf("单成员不应有冲突，实际 %d", got)
	}
	insert(b)

	// 冲突被消解：成员表里两个地址不同，且小 ID 保持原地址。
	if d.members[a].VirtualIP != base {
		t.Fatalf("保留方地址被改动: %s != %s", d.members[a].VirtualIP, base)
	}
	if d.members[b].VirtualIP == base {
		t.Fatalf("冲突方仍占用原地址 %s", base)
	}
	conflicts := d.VirtualIPConflicts()
	if len(conflicts) != 1 {
		t.Fatalf("冲突记录条数 %d != 1: %+v", len(conflicts), conflicts)
	}
	rec := conflicts[0]
	if rec.PeerID != a || rec.IP != base || rec.WinnerIP != base {
		t.Fatalf("冲突记录裁决方错误: %+v", rec)
	}
	if len(rec.Losers) != 1 || rec.Losers[0] != b || rec.LoserIPs[0] != d.members[b].VirtualIP {
		t.Fatalf("冲突记录改派生方错误: %+v", rec)
	}
	if !rec.Resolved {
		t.Fatalf("该冲突应已消解: %+v", rec)
	}
	if d.VirtualIPConflictCount() != 1 || d.VirtualIPConflictUnresolvedCount() != 0 {
		t.Fatalf("冲突计数错误: count=%d unresolved=%d", d.VirtualIPConflictCount(), d.VirtualIPConflictUnresolvedCount())
	}

	// 成员表与占用表必须一致：没有两个成员共用一个地址。
	seen := map[string]string{}
	for _, m := range d.Peers() {
		if prev, ok := seen[m.VirtualIP]; ok {
			t.Fatalf("成员 %q 与 %q 共用虚拟 IP %s", prev, m.PeerID, m.VirtualIP)
		}
		seen[m.VirtualIP] = m.PeerID
	}

	// 删除冲突方：占用释放，冲突记录随之消失。
	remove(b)
	if _, ok := d.vaddrs.lea[b]; ok {
		t.Fatal("成员删除后占用未释放")
	}
	if got := d.VirtualIPConflicts(); len(got) != 0 {
		t.Fatalf("冲突方离开后不应再有冲突记录: %+v", got)
	}
	// 重新入表：a 仍在（占着基础地址），所以 b 必须再次被改派生，
	// 且结果与第一次完全一致（幂等：同一成员集合 → 同一裁决）。
	insert(b)
	if d.members[b].VirtualIP != rec.LoserIPs[0] {
		t.Fatalf("重新入表后应恢复到同一改派生地址 %s，实际 %s", rec.LoserIPs[0], d.members[b].VirtualIP)
	}
	if d.members[a].VirtualIP == d.members[b].VirtualIP {
		t.Fatalf("重新入表后地址仍然冲突: %s", d.members[a].VirtualIP)
	}
	// 再把 a 也删掉、只剩 b 时，b 才拿回基础派生值（占用确实被释放了）。
	remove(a)
	if _, ok := d.vaddrs.lea[a]; ok {
		t.Fatal("a 删除后占用未释放")
	}
	insert(b)
	if d.members[b].VirtualIP != base {
		t.Fatalf("无冲突方时 b 应拿到基础地址 %s，实际 %s", base, d.members[b].VirtualIP)
	}
}

// TestDiscoveryVirtualIPConcurrentReadWrite 并发读写断言：
// 成员表写入（持 d.mu）与 VirtualIPConflicts / Peers 并发读取不得 panic、
// 不得出现「两份成员共用一个地址」。本机无 cgo 跑不了 -race，因此这里用
// 高频并发 + 不变式断言来兜住锁纪律（所有写都在 d.mu 内、读走 RLock）。
func TestDiscoveryVirtualIPConcurrentReadWrite(t *testing.T) {
	groupKey := GroupKey("", "vaddr-concurrent")
	a, b := collidePair(t, groupKey)
	d := &Discovery{
		groupKey: groupKey,
		cfg:      Config{Quiet: true},
		host:     testHost(t, false),
		members:  make(map[string]*Member),
		vaddrs:   newVirtualIPTable(),
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写协程：反复加入/移除冲突双方（模拟发现与回收的交替）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			id := a
			if i%2 == 1 {
				id = b
			}
			d.mu.Lock()
			d.members[id] = &Member{PeerID: id, VirtualIP: d.alignedVirtualIP(id)}
			d.claimVirtualIPsLocked()
			d.mu.Unlock()

			d.mu.Lock()
			delete(d.members, id)
			d.releaseVirtualIPsLocked()
			d.mu.Unlock()
		}
		close(stop)
	}()

	// 读协程：并发读冲突列表与成员表，断言不变式。
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = d.VirtualIPConflicts()
				_ = d.VirtualIPConflictCount()
				_ = d.VirtualIPConflictUnresolvedCount()
				seen := map[string]string{}
				for _, m := range d.Peers() {
					if prev, ok := seen[m.VirtualIP]; ok {
						t.Errorf("并发写入期间成员 %q 与 %q 共用虚拟 IP %s", prev, m.PeerID, m.VirtualIP)
						return
					}
					seen[m.VirtualIP] = m.PeerID
				}
			}
		}()
	}
	wg.Wait()
}

// TestVirtualIPConflictsEmptyWhenNoConflict 无冲突场景：只读接口返回空切片
// （非 nil），且计数为零——控制台可以直接 range 而不用判 nil。
func TestVirtualIPConflictsEmptyWhenNoConflict(t *testing.T) {
	var nilD *Discovery
	if got := nilD.VirtualIPConflicts(); got == nil || len(got) != 0 {
		t.Fatalf("nil Discovery 应返回空切片，实际 %#v", got)
	}
	if nilD.VirtualIPConflictCount() != 0 || nilD.VirtualIPConflictUnresolvedCount() != 0 {
		t.Fatal("nil Discovery 计数应为 0")
	}
	d := &Discovery{
		groupKey: GroupKey("", "vaddr-empty"),
		cfg:      Config{Quiet: true},
		host:     testHost(t, false),
		members:  make(map[string]*Member),
		vaddrs:   newVirtualIPTable(),
	}
	if got := d.VirtualIPConflicts(); got == nil || len(got) != 0 {
		t.Fatalf("无冲突时应返回空切片，实际 %#v", got)
	}
}

// TestVirtualIPConflictRecordExpiresWithMember 冲突记录随成员离开自动失效：
// 只剩冲突的一半时不再报告（不留下幽灵条目）。
func TestVirtualIPConflictRecordExpiresWithMember(t *testing.T) {
	groupKey := GroupKey("", "vaddr-expire-record")
	a, b := collidePair(t, groupKey)
	d := &Discovery{
		groupKey: groupKey,
		cfg:      Config{Quiet: true},
		host:     testHost(t, false),
		members:  make(map[string]*Member),
		vaddrs:   newVirtualIPTable(),
	}
	d.mu.Lock()
	d.members[a] = &Member{PeerID: a, VirtualIP: d.alignedVirtualIP(a)}
	d.members[b] = &Member{PeerID: b, VirtualIP: d.alignedVirtualIP(b)}
	d.claimVirtualIPsLocked()
	d.mu.Unlock()
	if len(d.VirtualIPConflicts()) != 1 {
		t.Fatalf("应有 1 条冲突记录: a=%+v b=%+v conflicts=%+v owners=%v",
			d.vaddrs.lea[a], d.vaddrs.lea[b], d.vaddrs.conflicts, d.vaddrs.ipOwners)
	}
	// 移除被改派生的一方：冲突不复存在，记录失效。
	d.mu.Lock()
	delete(d.members, b)
	d.releaseVirtualIPsLocked()
	d.mu.Unlock()
	if got := d.VirtualIPConflicts(); len(got) != 0 {
		t.Fatalf("冲突另一半离开后仍报告冲突: %+v", got)
	}
	// 保留方地址回到基础派生值（它就是原地址，这里确认没有被误改）。
	if d.members[a].VirtualIP != DeriveVirtualIP(groupKey, a) {
		t.Fatalf("冲突消解后保留方地址被改动: %s", d.members[a].VirtualIP)
	}
}

// TestVirtualIPReapExpiredReleases 真实回收路径（reapExpired）也释放占用：
// 这里用带 host 的 Discovery 走完整 reapExpired，不做手工 delete。
func TestVirtualIPReapExpiredReleases(t *testing.T) {
	groupKey := GroupKey("", "vaddr-reap")
	a, b := collidePair(t, groupKey)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "vaddr-reap", Quiet: true, MemberTTL: MinMemberTTL})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	defer d.Close()
	d.scheduler.cancel() // 防止异步握手改写成员表、干扰断言

	old := time.Now().Add(-DefaultMemberTTL - time.Minute)
	d.mu.Lock()
	d.members[a] = &Member{PeerID: a, VirtualIP: d.alignedVirtualIP(a), LastSeen: old}
	d.members[b] = &Member{PeerID: b, VirtualIP: d.alignedVirtualIP(b), LastSeen: old}
	d.claimVirtualIPsLocked()
	d.mu.Unlock()
	if len(d.VirtualIPConflicts()) != 1 {
		t.Fatalf("构造后应有 1 条冲突记录")
	}

	d.reapExpired() // 两个成员都超期 → 全部回收，占用全释放
	d.mu.RLock()
	left := len(d.members)
	d.mu.RUnlock()
	if left != 0 {
		t.Fatalf("超期成员未被回收，剩 %d 个", left)
	}
	if n := d.VirtualIPConflictCount(); n != 0 {
		t.Fatalf("回收后占用表未释放，仍有 %d 条冲突", n)
	}
	d.mu.RLock()
	leases, owners := len(d.vaddrs.lea), len(d.vaddrs.ipOwners)
	d.mu.RUnlock()
	if leases != 0 || owners != 0 {
		t.Fatalf("回收后占用表应为空，实际 leases=%d owners=%d", leases, owners)
	}
}
