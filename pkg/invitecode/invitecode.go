// Package invitecode：连接码（invite code）编解码。
//
// 连接码 = 「节点 ID + 连接种子」合并压缩成的一串短字符串，形如：
//
//	lanet://12D3KooW...@1.2.3.4:4001
//	lanet://12D3KooW...@[2408:824e::1]:4001
//
// 语义与完整 multiaddr（/ip4/1.2.3.4/tcp/4001/p2p/<ID>）完全等价，
// 但更短、可读、可直接发微信/邮箱。用户只需复制粘贴一串即可连接：
// 对端地址直接可拨号，省去「查地址簿 → 私有 DHT 兜底查找」的整条链路。
//
// 连接码可携带**多个地址**（逗号分隔），这也是默认做法：
//
//	lanet://12D3KooW...@192.168.1.9:4001,[2408:824e::577]:4001,10.70.38.92:4001
//
// 原因：一台机器通常有多块网卡（物理网卡 / 公网 IPv6 / VPN / 虚拟网卡），
// 对端与本机到底走哪条路可达，只有逐个尝试才知道。只给一条地址等于把
// 成功率押在那一块网卡上——第一版就踩过这个坑：只带 ZeroTier 地址时，
// 对端在物理局域网内反而连不上。把本机所有网卡都带上，对端逐个尝试即可
// 命中。地址数量上限见 MaxAddrs。
package invitecode

import (
	"fmt"
	"net"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Scheme 连接码的 URI scheme。
const Scheme = "lanet"

// MaxAddrs 连接码最多携带的直拨地址数。
// 地址越多连接成功率越高，但连接码越长越难复制粘贴，故设上限：
// 4 个地址已足够覆盖「物理局域网 + 公网 IPv6 + VPN + 虚拟网卡」四类网卡。
const MaxAddrs = 4

// Encode 把「节点 ID + 主机地址」编码为连接码。
// hostPort 可为 IPv4（1.2.3.4:4001）或 IPv6（[::1]:4001）形式；
// 空 hostPort 时生成 lanet://<ID> 形式（仅身份，无直拨地址，
// 对端识别后退回按 ID 查找路径）。
func Encode(peerID, hostPort string) string {
	if hostPort == "" {
		return Scheme + "://" + peerID
	}
	return Scheme + "://" + peerID + "@" + hostPort
}

// EncodeList 把「节点 ID + 多个主机地址」编码为连接码：
//
//	lanet://12D3KooW...@192.168.1.9:4001,[2408:824e::577]:4001
//
// 调用方应把本机**所有**可用网卡的地址传进来（推荐顺序见
// p2pkit.SortByReachability：公网 IPv6 / 局域网优先，link-local 靠后）——
// 顺序即对端拨号尝试顺序，最优的路径排在前面能显著缩短建连时间。
// 空元素与重复项会被剔除；超过 MaxAddrs 的地址被截断；
// 无有效地址时退化为仅身份形式 lanet://<ID>。
func EncodeList(peerID string, hostPorts []string) string {
	clean := make([]string, 0, len(hostPorts))
	seen := make(map[string]bool, len(hostPorts))
	for _, hp := range hostPorts {
		hp = strings.TrimSpace(hp)
		if hp == "" || seen[hp] {
			continue
		}
		seen[hp] = true
		clean = append(clean, hp)
		if len(clean) >= MaxAddrs {
			break
		}
	}
	if len(clean) == 0 {
		return Scheme + "://" + peerID
	}
	return Scheme + "://" + peerID + "@" + strings.Join(clean, ",")
}

// splitHostPorts 切分地址段为多个 hostPort。
// 兼容逗号为主、分号与空白为辅的分隔方式：用户手工拼接或从多行文本
// 复制过来时（给连接种子用过的多行格式），同样能解析。
func splitHostPorts(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// EncodeFromMultiaddr 从 multiaddr 字符串提取连接码。
// 仅支持 /ip4|/ip6 + /tcp|/udp + /p2p/<ID> 形式；不满足时返回空串。
func EncodeFromMultiaddr(addr string) string {
	m, err := ma.NewMultiaddr(addr)
	if err != nil {
		return ""
	}
	ai, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil || ai == nil || len(ai.Addrs) == 0 {
		return ""
	}
	hostPort := ""
	for _, a := range ai.Addrs {
		if ip4, err := a.ValueForProtocol(ma.P_IP4); err == nil {
			if tcp, err2 := a.ValueForProtocol(ma.P_TCP); err2 == nil {
				hostPort = net.JoinHostPort(ip4, tcp)
				break
			}
		}
		if ip6, err := a.ValueForProtocol(ma.P_IP6); err == nil {
			if tcp, err2 := a.ValueForProtocol(ma.P_TCP); err2 == nil {
				hostPort = net.JoinHostPort(ip6, tcp)
				break
			}
		}
		// QUIC / WS 等传输在 multiaddr 里是 /udp 或 /tcp + 额外段，
		// 这里取端口通用协议（有 tcp 用 tcp，否则 quic/udp 也能拨）。
		if udp, err := a.ValueForProtocol(ma.P_UDP); err == nil && hostPort == "" {
			if ip4, err2 := a.ValueForProtocol(ma.P_IP4); err2 == nil {
				hostPort = net.JoinHostPort(ip4, udp)
			} else if ip6, err2 := a.ValueForProtocol(ma.P_IP6); err2 == nil {
				hostPort = net.JoinHostPort(ip6, udp)
			}
		}
	}
	return Encode(ai.ID.String(), hostPort)
}

// Decode 解析连接码，返回节点 ID 与首个 hostPort（可能为空）。
// 保留此单地址签名是为了兼容既有调用方；需要拿到全部地址请用 DecodeList。
func Decode(code string) (peerID, hostPort string, err error) {
	id, addrs, derr := DecodeList(code)
	if derr != nil {
		return "", "", derr
	}
	if len(addrs) == 0 {
		return id, "", nil
	}
	return id, addrs[0], nil
}

// DecodeList 解析连接码，返回节点 ID 与**全部** hostPort（可能为空列表）。
// 返回的各 hostPort 已做好拨号侧的 multiaddr 组装准备。
//
// 手写解析而非 net/url：地址段可能含多个「IP:端口」（逗号分隔），
// url.Parse 会把 "1.2.3.4:4001,5.6.7.8:4001" 的端口段判为非法并报错。
func DecodeList(code string) (peerID string, hostPorts []string, err error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(strings.ToLower(code), Scheme+"://") {
		return "", nil, fmt.Errorf("不是连接码（需以 %s:// 开头）", Scheme)
	}
	rest := code[len(Scheme)+3:]
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		peerID, hostPorts = rest[:i], splitHostPorts(rest[i+1:])
	} else {
		peerID = rest
	}
	peerID = strings.Trim(peerID, "/")
	if _, derr := peer.Decode(peerID); derr != nil {
		return "", nil, fmt.Errorf("连接码中的节点 ID 无效: %w", derr)
	}
	for _, hp := range hostPorts {
		h, _, herr := net.SplitHostPort(hp)
		if herr != nil || h == "" {
			return "", nil, fmt.Errorf("连接码中的地址段无效（应为 IP:端口，IPv6 需方括号）: %q", hp)
		}
		if parseHostIP(h) == nil {
			return "", nil, fmt.Errorf("连接码中的地址段不是合法 IP: %q", hp)
		}
	}
	return peerID, hostPorts, nil
}

// parseHostIP 解析主机段为 IP；IPv6 zone（fe80::1%12）会被剥离——
// zone 是本机接口索引，对端无意义且会让 ParseIP 直接失败。
func parseHostIP(host string) net.IP {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// IsInviteCode 判断字符串是否形如连接码。
func IsInviteCode(s string) bool {
	_, _, err := Decode(s)
	return err == nil
}

// ToMultiaddrs 把连接码还原为可拨号的 multiaddr 列表。
// 多个地址会被全部展开（每个地址各给 TCP 与 QUIC 两种传输），
// 拨号侧逐个尝试，任一可达即建连成功——这正是多地址连接码的价值。
// 无地址（仅身份形式）时返回空列表，调用方退回按 ID 查找。
func ToMultiaddrs(code string) (peer.ID, []ma.Multiaddr, error) {
	id, hostPorts, err := DecodeList(code)
	if err != nil {
		return "", nil, err
	}
	pid, derr := peer.Decode(id)
	if derr != nil {
		return "", nil, fmt.Errorf("节点 ID 无效: %w", derr)
	}
	if len(hostPorts) == 0 {
		return pid, nil, nil
	}
	mustAddr := func(s string) ma.Multiaddr {
		m, merr := ma.NewMultiaddr(s)
		if merr != nil {
			panic(merr) // 参数由本函数内部构造，格式恒合法
		}
		return m
	}
	out := make([]ma.Multiaddr, 0, len(hostPorts)*2)
	for _, hostPort := range hostPorts {
		host, port, _ := net.SplitHostPort(hostPort)
		ip := parseHostIP(host)
		if ip == nil {
			return pid, nil, fmt.Errorf("地址段不是合法 IP: %q", host)
		}
		// 规范化为点分/冒号形式：IPv4-mapped IPv6（::ffff:1.2.3.4）
		// 直接用原串会拼出非法的 /ip4/::ffff:1.2.3.4/...。
		if ip4 := ip.To4(); ip4 != nil {
			host = ip4.String()
			out = append(out,
				mustAddr(fmt.Sprintf("/ip4/%s/tcp/%s", host, port)),
				mustAddr(fmt.Sprintf("/ip4/%s/udp/%s/quic-v1", host, port)),
			)
			continue
		}
		host = ip.String()
		out = append(out,
			mustAddr(fmt.Sprintf("/ip6/%s/tcp/%s", host, port)),
			mustAddr(fmt.Sprintf("/ip6/%s/udp/%s/quic-v1", host, port)),
		)
	}
	return pid, out, nil
}
