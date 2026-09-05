// 虚拟地址：成员的「域名式」稳定连接地址。
//
// 形如 <节点名>.lanet（如 yunloli.lanet），按成员表实时解析为虚拟 IP：
// 成员重启、重新入网导致虚拟 IP 变化时，名字解析自动跟随，无需改配置。
// 规则：
//   - 节点名规范化为 DNS 标签（a-z0-9-，小写，1~63 字符）；
//     无法规范化的成员回退 node-<虚拟IP 尾两段>；
//   - 组内规范化后重名时，按 PeerID 排序后到者追加 -<PeerID 前 4 位>，
//     所有节点从同一成员表独立计算，结果一致（无需持久化/协商）；
//   - 解析目标支持：虚拟 IP、<label>.lanet 完整虚拟地址、短名 label、
//     原始成员名（兼容旧行为）。
package serverless

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// VirtualDomain 虚拟地址的固定后缀。
const VirtualDomain = "lanet"

var dnsLabelStrip = regexp.MustCompile(`[^a-z0-9-]+`)
var dnsLabelCollapse = regexp.MustCompile(`-{2,}`)

// MemberRef 成员解析所需的最小视图（serverless.Member / netmapclient.Member 均可转换）。
type MemberRef struct {
	PeerID    string
	Name      string
	VirtualIP string
}

// DNSLabel 把节点名规范化为 DNS 标签（不含后缀）。无法规范化返回 ""。
// 规则：转小写 → 空格/下划线转连字符 → 剔除非法字符 → 收敛连续连字符
// → 去首尾连字符 → 截断 63 字符。
func DNSLabel(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	s = dnsLabelStrip.ReplaceAllString(s, "")
	s = dnsLabelCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// Hostnames 为成员表计算确定性虚拟主机名（不含 .lanet 后缀）。
// 返回 peerID → label。规则见包注释：按 PeerID 排序先到先得，
// 重名后到者追加 -<PeerID 尾 4 位>；名字不可规范化时回退 node-<IP 尾两段>。
func Hostnames(members []MemberRef) map[string]string {
	sorted := append([]MemberRef(nil), members...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PeerID < sorted[j].PeerID })

	out := make(map[string]string, len(sorted))
	taken := make(map[string]bool, len(sorted))
	for _, m := range sorted {
		label := DNSLabel(m.Name)
		if label == "" {
			// 回退名取虚拟 IP 尾两段，如 10.7.92.241 → node-92-241。
			if ip := net.ParseIP(m.VirtualIP); ip != nil {
				o := ip.To4()
				if o != nil {
					label = fmt.Sprintf("node-%d-%d", o[2], o[3])
				}
			}
			if label == "" {
				label = "node-" + m.PeerID[:4]
			}
		}
		if taken[label] {
			suffix := m.PeerID
			if len(suffix) > 4 {
				// libp2p PeerID 有公共前缀（如 12D3KooW…），后缀取尾部才具区分度。
				suffix = suffix[len(suffix)-4:]
			}
			cand := label + "-" + strings.ToLower(suffix)
			for taken[cand] { // 尾 4 位撞车（极罕见）继续加后缀
				cand = cand + "-x"
			}
			label = cand
		}
		taken[label] = true
		out[m.PeerID] = label
	}
	return out
}

// ResolveTarget 解析连接目标为成员：
//   - 虚拟 IP（如 10.7.92.241）→ 按虚拟 IP 匹配；
//   - 完整虚拟地址（如 yunloli.lanet）/ 短名（yunloli）→ 按规范化名匹配；
//   - 原始成员名 → 兼容旧行为的精确匹配兜底。
//
// 未找到时返回带成员表摘要的错误，便于排障。
func ResolveTarget(members []MemberRef, target string) (MemberRef, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return MemberRef{}, fmt.Errorf("lanet: 连接目标为空")
	}

	// 1. 虚拟 IP 直配。
	if net.ParseIP(target) != nil {
		for _, m := range members {
			if m.VirtualIP == target {
				return m, nil
			}
		}
		return MemberRef{}, fmt.Errorf("lanet: 虚拟 IP %s 不在成员表中（成员可能尚未被发现或已下线）", target)
	}

	// 2. 虚拟地址 / 短名：去掉 .lanet 后缀后按规范化名匹配。
	label := strings.ToLower(target)
	label = strings.TrimSuffix(label, ".")
	label = strings.TrimSuffix(label, "."+VirtualDomain)
	if idx := strings.LastIndex(label, "."+VirtualDomain); idx >= 0 {
		label = label[:idx]
	}
	if label != "" {
		hosts := Hostnames(members)
		var hits []MemberRef
		for _, m := range members {
			if hosts[m.PeerID] == label {
				hits = append(hits, m)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			// 继续走原始名兜底（成员名可能含中文等无法规范化的字符，
			// 但用户直接复制了控制台显示的原始名）。
		default:
			return MemberRef{}, fmt.Errorf("lanet: 目标 %q 命中 %d 个成员，请使用完整虚拟地址（如 %s.%s）",
				target, len(hits), hits[0].Name, VirtualDomain)
		}
	}

	// 3. 原始成员名精确匹配（兼容旧行为）。
	var hit []MemberRef
	for _, m := range members {
		if m.Name == target {
			hit = append(hit, m)
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return MemberRef{}, fmt.Errorf("lanet: 未找到节点 %q（虚拟地址支持 成员名.lanet / 成员名 / 虚拟 IP；当前成员表 %d 人，该成员可能尚未被发现）",
			target, len(members))
	default:
		full := make([]string, 0, len(hit))
		for _, m := range hit {
			hosts := Hostnames(members)
			full = append(full, hosts[m.PeerID]+"."+VirtualDomain)
		}
		return MemberRef{}, fmt.Errorf("lanet: 存在 %d 个名为 %q 的节点，请使用各自的完整虚拟地址区分：%s",
			len(hit), target, strings.Join(full, "、"))
	}
}
