// 虚拟 IP 冲突裁决：同一网络密钥内虚拟 IPv4 唯一。
//
// 问题：DeriveVirtualIP 把（群密钥, PeerID）哈希压进 10.7.1.1~10.7.254.254
// （约 6.4 万个地址）。群越大，两个成员的派生结果越可能撞成同一个 IP；
// 撞上时双方互相 Dial 会打到对方，控制台/成员表还会同时显示同一个 IP。
// netmapclient 在路由层遇到重复地址直接拒绝（tunnel 不可用），但成员表
// 里仍然是一片「看起来正常」的重复 IP。
//
// 方案（用户已拍板）：不改 DeriveVirtualIP 本身（改基础算法等于全网换网，
// 所有节点的地址一次性全变），只在**冲突发生时**对冲突的那一个成员改用
// 域分离哈希重新派生；同一网络密钥内一个 IP 只属于一个 PeerID，
// 不同网络密钥之间允许重复（本来就是两张互不可见的网）。
//
// 裁决规则：同群内两个（或多个）PeerID 的基础派生结果相同时，PeerID
// 字符串字典序最小者保留原地址，其余按盐值 1..64 依次重新派生，取第一个
// 本群内未被占用的地址。用「ID 字典序」而不是「谁先被发现」来裁决，
// 保证两端各自独立计算、且与成员入表顺序无关 —— 这是本方案能被两端
// 同时接受的前提（PeerID 是 base58，同一群内字符序即稳定序）。
//
// 冲突集合从**完整成员表**导出（基础派生是纯函数），因此裁决是全表范围的：
// 第 3 个成员的到来不会推翻前两个成员之间的既有裁决。
//
// ---- 并发与锁纪律 ----
//
// 占用表 virtualIPTable 的读写全部受 Discovery.mu 保护，不引入第二把锁。
// 锁顺序：virtualIPTable 的所有操作**只允许在已持有 d.mu（读或写）时调用**，
// 且它自己绝不获取任何其他锁（不碰 peerstore、不碰 scheduler、不打日志）。
// 裁决产生的日志由各写入点在释放 d.mu 之后统一输出，避免持锁做 I/O。
//
// ---- 兼容取舍（旧版本节点） ----
//
// 旧版本节点不认识「被改派生的 IP」：它仍然按 DeriveVirtualIP 算自己
// （以及我们）的地址，因此冲突对里的旧版本对端会看到 IP 对不上。
// 取舍如下：
//   - 只影响真正冲突的那一对成员。非冲突成员的基础派生值原样保留，
//     整网绝大多数节点地址一个字节都不变，不存在全网换网问题；
//   - 冲突对在旧版本下本来就是坏掉的：tunnel 层因重复地址直接拒绝该路由，
//     两边谁都不可用。改派生之后，裁决方（保留原地址的一方）在 tunnel
//     层恢复唯一性，另一方的地址变成新地址——最坏情况仍然是「其中一方
//     在旧版本眼里不可用」，即「从完全不可用变为部分可用」，不会更差；
//   - 新版/旧版混跑时一律以「谁在成员表里」为准，不做版本协商：冲突是
//     本机成员表内部的一致性问题，两端各自算各自的成员表即可。
package serverless

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// VirtualIPv4SaltMax 冲突重新派生的盐值上限（1..64）。
//
// 64 次线性探测在 6.4 万空间的占用密度下几乎不可能耗尽（真耗尽说明这个
// 群已经接近地址空间上限）。耗尽时保留基础派生值、把自己标记为
// Unresolved，让上层能判别「这是没消解的冲突」，而不是静默假装没事。
const VirtualIPv4SaltMax = 64

// virtualIPDomainSep 「虚拟 IPv4 重派生」的域分隔前缀。与 DeriveVirtualIP
// 的输入（裸 groupKey||peerID）不同域，避免两条派生路径撞成同一摘要。
const virtualIPDomainSep = "lanet-virtual-ipv4-v2:"

// virtualIPLease 一个成员在冲突裁决后的地址归属。
type virtualIPLease struct {
	// BaseIP 基础派生结果 DeriveVirtualIP(groupKey, peerID)，永不改变。
	// 它也是「谁和谁构成冲突」的判据：基础 IP 相同才是一组。
	BaseIP string
	// IP 裁决后实际写进成员表的虚拟 IP。无冲突时等于 BaseIP。
	IP string
	// Salt 重新派生使用的盐值；0 表示保留基础派生值（无冲突或盐耗尽）。
	Salt int
	// Losers 仅出现在**保留原地址**的一方身上：与它撞车、被迫改派生的
	// 其他 PeerID（升序）。控制台据此展示「谁撞了谁」。
	Losers []string
	// Unresolved 盐值 1..VirtualIPv4SaltMax 全部被占用，冲突未消解：
	// IP 仍是 BaseIP，与冲突方重复。可判定状态，供告警/展示。
	Unresolved bool
}

// virtualIPTable 同群虚拟 IPv4 占用表：PeerID ↔ 虚拟 IP 双向映射。
//
// 生命周期与成员表 d.members 完全一致：成员入表时同步、成员离开成员表时
// 释放，任何一处漏掉都会变成「IP 被永久占用、后来者被迫改派」的泄漏。
// 因此所有调用点都必须与成员表写操作处于**同一个 d.mu 临界区**。
type virtualIPTable struct {
	lea        map[string]*virtualIPLease // PeerID -> 归属
	ipOwners   map[string][]string        // IP -> 占用它的 PeerID（升序）
	peerIPs    map[string]string          // 冗余 1:1 快查索引（Peers 热路径）
	conflicts  map[string]bool            // 发生过冲突的基础 IP（仅变化过的）
	unresolved int                        // 当前未消解的成员数
}

func newVirtualIPTable() *virtualIPTable {
	return &virtualIPTable{
		lea:       make(map[string]*virtualIPLease),
		ipOwners:  make(map[string][]string),
		peerIPs:   make(map[string]string),
		conflicts: make(map[string]bool),
	}
}

// ---- 纯函数裁决（不依赖占用表、不依赖入表顺序） ----

// deriveVirtualIPSalted 域分离加盐重新派生虚拟 IPv4：
//   - salt <= 0 与 DeriveVirtualIP 语义一致（返回基础派生值）；
//   - salt > 0 用 "lanet-virtual-ipv4-v2:<salt>:<groupKey>:<peerID>" 摘要。
func deriveVirtualIPSalted(groupKey []byte, peerID string, salt int) string {
	if salt <= 0 {
		return DeriveVirtualIP(groupKey, peerID)
	}
	buf := make([]byte, 0, len(virtualIPDomainSep)+4+len(groupKey)+1+len(peerID))
	buf = append(buf, virtualIPDomainSep...)
	buf = append(buf, fmt.Sprintf("%d:", salt)...)
	buf = append(buf, groupKey...)
	buf = append(buf, ':')
	buf = append(buf, peerID...)
	h := sha256.Sum256(buf)
	return fmt.Sprintf("10.7.%d.%d", int(h[0])%254+1, int(h[1])%254+1)
}

// decideVirtualIPs 对「同群 PeerID 集合」给出确定性裁决：peerID -> 归属。
//
// 纯函数：同一组 PeerID（不管以什么顺序传入、从哪里收集）必然得到同一结果。
// 集合里没有「自己」的概念——本机把自己的 PeerID 一起传进来即可，
// 裁决只依赖 ID 的字典序与哈希，与谁在算、什么时候算无关。
//
// 算法：
//  1. 全部基础派生，按基础 IP 分组；
//  2. 组内按 PeerID 字典序升序：最小者保留原地址，其余逐个加盐重派生；
//  3. 盐值从 1 递增到 VirtualIPv4SaltMax，跳过已被**全表**占用的地址；
//     耗尽则保留原值并标记 Unresolved（可判定，不静默）。
//
// 「盐耗尽时保留原值而不是继续加盐」是刻意的：保证裁决在任意成员集合上
// 都是有限、终止的纯函数，且两端算出的状态一致。
func decideVirtualIPs(groupKey []byte, peerIDs []string) map[string]*virtualIPLease {
	return decideVirtualIPsWithBlocked(groupKey, peerIDs, nil)
}

// decideVirtualIPsWithBlocked decideVirtualIPs 的可测核心：blocked 是额外
// 视为「已被占用」的地址集合（生产路径传 nil，测试用来模拟盐值耗尽）。
func decideVirtualIPsWithBlocked(groupKey []byte, peerIDs []string, blocked map[string]bool) map[string]*virtualIPLease {
	ids := make([]string, 0, len(peerIDs))
	seen := make(map[string]bool, len(peerIDs))
	for _, id := range peerIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	// 稳定序：PeerID 字典序（Go 字符串比较即字节序）。map 遍历序是随机的，
	// 凡是「谁保留原地址」「谁先挑盐值」的判断都只读这个序。
	sort.Strings(ids)

	byBase := make(map[string][]string, len(ids))
	for _, id := range ids {
		base := DeriveVirtualIP(groupKey, id)
		byBase[base] = append(byBase[base], id)
	}

	out := make(map[string]*virtualIPLease, len(ids))
	for _, id := range ids {
		base := DeriveVirtualIP(groupKey, id)
		out[id] = &virtualIPLease{BaseIP: base, IP: base}
	}

	// 第一遍：每个冲突组内按升序定序，最小者保留原地址，其余待重派生。
	type pending struct{ id, base string }
	var toReassign []pending
	heads := make([]string, 0)
	for base, group := range byBase {
		if len(group) < 2 {
			continue
		}
		// group 内的顺序来自 ids（已排序），因此 group[0] 即字典序最小者。
		out[group[0]].Losers = append([]string(nil), group[1:]...)
		heads = append(heads, base)
		for _, id := range group[1:] {
			toReassign = append(toReassign, pending{id: id, base: base})
		}
	}
	sort.Strings(heads)
	sort.Slice(toReassign, func(i, j int) bool { return toReassign[i].id < toReassign[j].id })

	// 第二遍：其余成员依次加盐重派生，避开全表已占用的地址。
	// taken 初值只含「保留原地址者」；重派生结果按 toReassign 的顺序填入，
	// 而那个顺序完全由 PeerID 字典序决定——所以多成员冲突时谁拿哪个盐值，
	// 在任何一台机器上算都一样。
	taken := make(map[string]bool, len(ids))
	for base := range blocked {
		taken[base] = true
	}
	for _, base := range heads {
		taken[base] = true
	}
	for _, p := range toReassign {
		lease := out[p.id]
		for salt := 1; salt <= VirtualIPv4SaltMax; salt++ {
			cand := deriveVirtualIPSalted(groupKey, p.id, salt)
			if taken[cand] {
				continue
			}
			taken[cand] = true
			lease.IP, lease.Salt, lease.Unresolved = cand, salt, false
			break
		}
		if lease.Salt == 0 {
			// 盐耗尽：保留原值并标记未消解。地址仍然重复，但状态可判定，
			// 上层能把「确实没解决」和「没有冲突」区分开。
			lease.IP, lease.Unresolved = p.base, true
		}
	}
	return out
}

// ---- 占用表操作（全部要求调用方已持有 d.mu 读或写锁） ----

// syncLocked 让占用表与传入的成员集合**完全一致**：重算纯函数裁决、
// 发放/更新归属、释放已离开成员表的 PeerID。
//
// 返回值 updated 表示「裁决后的 IP 与占用表旧值不同」的 PeerID（调用方据此
// 回写成员表并记日志）；added 表示本表首次见到的 PeerID。
//
// 为什么是「全量同步」而不是逐条 claim/release：裁决本身依赖完整冲突组，
// 逐条累加要额外维护每个冲突组的动态状态，且一旦某处漏调 release 就永久
// 跑偏；全量重建每轮都从成员表出发，天然自愈。单次成本 O(n log n)、n 为
// 成员数，在成员表临界区里可接受（成员表本身也是 O(n) 的快照操作）。
func (t *virtualIPTable) syncLocked(groupKey []byte, ids []string) (updated, added []string) {
	if t == nil {
		return nil, nil
	}
	desired := decideVirtualIPs(groupKey, ids)

	// 1. 释放已离开成员表的 PeerID（reapExpired / Forget / 握手失败的成员
	//    会在下一次同步时自然消失，无需各删除点单独记账）。
	for pid := range t.lea {
		if _, ok := desired[pid]; !ok {
			t.dropLocked(pid)
		}
	}
	// 2. 清理已经没有人占用的冲突记录：冲突双方都走了，这条记录必须消失，
	//    否则 VirtualIPConflictCount 会随历史冲突数量无界增长（泄漏）。
	for ip := range t.conflicts {
		if len(t.ipOwners[ip]) == 0 {
			delete(t.conflicts, ip)
		}
	}
	// 2. 发放/更新。
	for _, id := range ids {
		if id == "" {
			continue
		}
		next := desired[id]
		if next == nil {
			continue
		}
		prev := t.lea[id]
		if prev == nil {
			added = append(added, id)
		}
		if prev != nil && prev.IP == next.IP && prev.BaseIP == next.BaseIP &&
			prev.Salt == next.Salt && prev.Unresolved == next.Unresolved &&
			equalStrings(prev.Losers, next.Losers) {
			continue // 无变化
		}
		if prev != nil {
			t.dropLocked(id) // IP 可能变了：先摘旧索引，再加新索引
		}
		t.lea[id] = next
		t.peerIPs[id] = next.IP
		t.ipOwners[next.IP] = appendSorted(t.ipOwners[next.IP], id)
		if len(next.Losers) > 0 || next.Salt > 0 {
			t.conflicts[next.BaseIP] = true
		}
		if next.Unresolved {
			t.unresolved++
		}
		if prev == nil || prev.IP != next.IP {
			updated = append(updated, id)
		}
	}
	return updated, added
}

// claimVirtualIPsLocked 让占用表与「成员表当前状态」对齐，并把裁决结果回写
// 成员表的 VirtualIP 字段。这是所有成员表写入点唯一需要调用的入口（在成员
// 已写入 d.members 之后、同一个 d.mu 临界区内）。
//
// 成员表是唯一事实来源，占用表只是它的索引：两者在这里一次写完，外部永远
// 看不到「占用表已改、成员表未改」的中间态。
func (d *Discovery) claimVirtualIPsLocked() []string {
	if d == nil {
		return nil
	}
	if d.vaddrs == nil {
		d.vaddrs = newVirtualIPTable()
	}
	ids := make([]string, 0, len(d.members))
	for id := range d.members {
		ids = append(ids, id)
	}
	updated, _ := d.vaddrs.syncLocked(d.groupKey, ids)
	for _, id := range updated {
		m, lease := d.members[id], d.vaddrs.lea[id]
		if m == nil || lease == nil {
			continue
		}
		m.VirtualIP = lease.IP
	}
	return updated
}

// releaseVirtualIPsLocked 成员删除后释放其地址占用。
//
// 语义上等价于 claimVirtualIPsLocked（全量重建），保留独立名字是为了让
// 每个删除点自解释：「这里释放了地址」。调用方须已持 d.mu 写锁，
// 且已先从 d.members 里删掉成员。
func (d *Discovery) releaseVirtualIPsLocked() {
	if d == nil {
		return
	}
	d.claimVirtualIPsLocked()
}

// virtualIPOfPeerLocked 读该 PeerID 当前裁决后的虚拟 IP（占用表无记录时
// 回退纯函数，保证未走同步的测试构造也能拿到合理初值）。
func (d *Discovery) virtualIPOfPeerLocked(peerID string) string {
	if d.vaddrs != nil {
		if ip := d.vaddrs.peerIPs[peerID]; ip != "" {
			return ip
		}
	}
	return DeriveVirtualIP(d.groupKey, peerID)
}

// alignedVirtualIP 成员入表时构造 VirtualIP 字段用：任何一个写入点都不会
// 把「未裁决的重复地址」写进成员表。
//
// 无冲突时返回基础派生值（与历史行为逐字节一致）；有冲突时返回已裁决值。
// 注意它只是**初值**：真正的所有权在 claimVirtualIPsLocked 的全表同步里，
// 同一临界区的后续同步可能再调整一次（例如新成员恰好抢在某成员之前入表）。
// 调用方必须在持有 d.mu 时调用。
func (d *Discovery) alignedVirtualIP(peerID string) string {
	if d == nil {
		return ""
	}
	return d.virtualIPOfPeerLocked(peerID)
}

// dropLocked 摘掉一个 PeerID 的归属（释放地址）。
func (t *virtualIPTable) dropLocked(peerID string) {
	lease, ok := t.lea[peerID]
	if !ok {
		return
	}
	delete(t.lea, peerID)
	delete(t.peerIPs, peerID)
	if owners := t.ipOwners[lease.IP]; len(owners) > 0 {
		kept := owners[:0]
		for _, o := range owners {
			if o != peerID {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(t.ipOwners, lease.IP)
		} else {
			t.ipOwners[lease.IP] = kept
		}
	}
	if lease.Unresolved && t.unresolved > 0 {
		t.unresolved--
	}
}

// occupiedLocked 该地址是否已被本群某个 PeerID 占用。
func (t *virtualIPTable) occupiedLocked(ip string) bool {
	return t != nil && len(t.ipOwners[ip]) > 0
}

// ---- 只读状态（供控制台展示） ----

// VirtualIPConflict 一条虚拟 IP 冲突裁决记录。
type VirtualIPConflict struct {
	// IP 冲突地址：冲突双方的基础派生值（也是保留方实际使用的地址）。
	// 盐耗尽未消解时，双方实际都仍在使用这个地址。
	IP string `json:"virtual_ip"`
	// PeerID 冲突双方中**保留原地址**的一方（同群内字典序最小者）。
	PeerID string `json:"peer_id"`
	// WinnerIP 保留方裁决后的地址，恒等于 IP；显式给出便于前端直读。
	WinnerIP string `json:"winner_ip"`
	// Losers 与保留方撞车、被改派生的 PeerID（升序）。
	Losers []string `json:"losers"`
	// LoserIPs 与 Losers 一一对应的裁决后地址（盐耗尽未消解时等于 IP）。
	// 旧版本节点会看到「Losers 自己算出来的原地址」，与本字段不一致，
	// 属预期内的兼容取舍（见文件头）。
	LoserIPs []string `json:"loser_ips"`
	// Resolved 是否已消解（false = 盐耗尽，地址仍然重复）。
	Resolved bool `json:"resolved"`
}

// VirtualIPConflicts 返回本机成员表内的虚拟 IP 冲突裁决列表（只读快照）。
// 无冲突时返回空切片（非 nil）。按冲突地址排序，内容稳定、可直接断言。
func (d *Discovery) VirtualIPConflicts() []VirtualIPConflict {
	if d == nil {
		return []VirtualIPConflict{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.virtualIPConflictsLocked()
}

// virtualIPConflictsLocked 冲突列表本体。须在持有 d.mu 时调用。
//
// 一条记录 = 一个发生过冲突的基础 IP。占位表里保留「谁保留了原地址」
// （Losers 非空的一方）与「被改派生的成员」两部分，两者必须都还在成员表
// 里记录才有效——冲突的另一半离开后记录自动失效，不会留下幽灵条目。
func (d *Discovery) virtualIPConflictsLocked() []VirtualIPConflict {
	out := make([]VirtualIPConflict, 0)
	if d.vaddrs == nil {
		return out
	}
	ips := make([]string, 0, len(d.vaddrs.conflicts))
	for ip := range d.vaddrs.conflicts {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	for _, ip := range ips {
		// 保留方自己也占用 base 地址，且它的 Losers 非空——不能把它当成
		// 「被改派生的一方」。判定标准是：Losers 非空者持有裁决信息，
		// 其余占用该地址、且从他人 Losers 里被点名者才是改派生方。
		//
		// 盐耗尽时改派生方也退回 base 地址，因此 ipOwners 里会出现「两个
		// 都 BaseIP==ip」的情况：能通过下方一致性校验的只有真正持有
		// Losers 的那个，用它来定位记录。
		var head *virtualIPLease
		winner := ""
		for _, o := range d.vaddrs.ipOwners[ip] {
			lease := d.vaddrs.lea[o]
			if lease == nil || lease.BaseIP != ip || len(lease.Losers) == 0 {
				continue
			}
			head = lease
			winner = o
			break
		}
		if head == nil {
			continue
		}
		rec := VirtualIPConflict{
			IP:       ip,
			PeerID:   winner,
			WinnerIP: ip,
			Losers:   append([]string(nil), head.Losers...),
			Resolved: true,
		}
		for _, loser := range head.Losers {
			lease := d.vaddrs.lea[loser]
			if lease == nil {
				continue
			}
			rec.LoserIPs = append(rec.LoserIPs, lease.IP)
			if lease.Unresolved {
				rec.Resolved = false
			}
		}
		out = append(out, rec)
	}
	return out
}

// VirtualIPConflictCount 冲突条数（已消解 + 未消解，按冲突地址计）。
func (d *Discovery) VirtualIPConflictCount() int {
	if d == nil {
		return 0
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.vaddrs == nil {
		return 0
	}
	return len(d.vaddrs.conflicts)
}

// VirtualIPConflictUnresolvedCount 盐耗尽、尚未消解的成员数。
// 正常网络里恒为 0；非 0 说明该群的 10.7/16 空间在据此视角下已近耗尽。
func (d *Discovery) VirtualIPConflictUnresolvedCount() int {
	if d == nil {
		return 0
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.vaddrs == nil {
		return 0
	}
	return d.vaddrs.unresolved
}

// ---- 冲突日志（必须在 d.mu 之外调用，避免持锁做 I/O） ----

// logVirtualIPConflicts 对本次变更涉及的 PeerID 打中文冲突日志，说明
// 「谁和谁撞了、裁决后谁的 IP 变成了什么」。短串用 shortID 截断，
// 避免日志被完整 base58 ID 撑爆。
func (d *Discovery) logVirtualIPConflicts(updated []string) {
	if d == nil || d.cfg.Quiet || len(updated) == 0 {
		return
	}
	d.mu.RLock()
	type record struct {
		ip       string
		winner   string
		losers   []string
		loserIPs []string
		resolved bool
	}
	recs := make([]record, 0, 2)
	for _, c := range d.virtualIPConflictsLocked() {
		if !containsAny(updated, c.PeerID, c.Losers) {
			continue
		}
		recs = append(recs, record{ip: c.IP, winner: c.PeerID, losers: c.Losers, loserIPs: c.LoserIPs, resolved: c.Resolved})
	}
	d.mu.RUnlock()
	for _, r := range recs {
		if !r.resolved {
			d.logf("虚拟 IP 冲突未消解：%s 与 %s 都派生出 %s，盐值 1~%d 全部被占用，暂保留原地址（请检查该群规模是否接近地址空间上限）",
				shortID(r.winner), joinShort(r.losers), r.ip, VirtualIPv4SaltMax)
			continue
		}
		parts := make([]string, 0, len(r.losers))
		for i, loser := range r.losers {
			to := r.ip
			if i < len(r.loserIPs) {
				to = r.loserIPs[i]
			}
			parts = append(parts, fmt.Sprintf("%s 改为 %s", shortID(loser), to))
		}
		d.logf("虚拟 IP 冲突：%s 与 %s 都派生出 %s，按节点 ID 升序裁决 %s 保留原地址，%s（只影响本群冲突双方，其他成员地址不变）",
			shortID(r.winner), joinShort(r.losers), r.ip, shortID(r.winner), strings.Join(parts, "，"))
	}
}

// ---- 小工具 ----

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendSorted 插入并保持升序（调用方保证不重复）。
func appendSorted(list []string, id string) []string {
	i := sort.SearchStrings(list, id)
	if i < len(list) && list[i] == id {
		return list
	}
	list = append(list, "")
	copy(list[i+1:], list[i:])
	list[i] = id
	return list
}

func containsAny(haystack []string, first string, rest []string) bool {
	for _, h := range haystack {
		if h == first {
			return true
		}
		for _, r := range rest {
			if h == r {
				return true
			}
		}
	}
	return false
}

func joinShort(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, shortID(id))
	}
	return strings.Join(out, "、")
}
