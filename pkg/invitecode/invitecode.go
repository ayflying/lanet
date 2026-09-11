// Package invitecode：连接码（invite code）编解码。
//
// 连接码 = 「节点 ID + 连接种子」合并压缩成的一串短字符串，形如：
//
//	lanet://12D3KooW...@1.2.3.4:4001
//	lanet://12D3KooW...@[fe80::1]:4001
//
// 语义与完整 multiaddr（/ip4/1.2.3.4/tcp/4001/p2p/<ID>）完全等价，
// 但更短、可读、可直接发微信/邮箱。用户只需复制粘贴一串即可连接：
// 对端地址直接可拨号，省去「查地址簿 → 私有 DHT 兜底查找」的整条链路。
package invitecode

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Scheme 连接码的 URI scheme。
const Scheme = "lanet"

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

// Decode 解析连接码，返回节点 ID 与 hostPort（可能为空）。
// 返回的 hostPort 已做好拨号侧的 multiaddr 组装准备。
func Decode(code string) (peerID, hostPort string, err error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(strings.ToLower(code), Scheme+"://") {
		return "", "", fmt.Errorf("不是连接码（需以 %s:// 开头）", Scheme)
	}
	u, err := url.Parse(code)
	if err != nil {
		return "", "", fmt.Errorf("连接码格式不正确: %w", err)
	}
	id := u.Opaque
	if id == "" {
		id = strings.Trim(u.Host+u.Path, "/")
	}
	// 去掉 user@host 中的 user 部分
	if i := strings.LastIndex(id, "@"); i >= 0 {
		hostPort = id[i+1:]
		id = id[:i]
	} else if u.User != nil {
		hostPort = u.Host
		id = u.User.Username()
	}
	id = strings.Trim(id, "/")
	if _, derr := peer.Decode(id); derr != nil {
		return "", "", fmt.Errorf("连接码中的节点 ID 无效: %w", derr)
	}
	if hostPort != "" {
		h, _, herr := net.SplitHostPort(hostPort)
		if herr != nil || h == "" {
			return "", "", fmt.Errorf("连接码中的地址段无效（应为 IP:端口）: %q", hostPort)
		}
	}
	return id, hostPort, nil
}

// IsInviteCode 判断字符串是否形如连接码。
func IsInviteCode(s string) bool {
	_, _, err := Decode(s)
	return err == nil
}

// ToMultiaddrs 把连接码还原为可拨号的 multiaddr 列表。
// hostPort 为空时返回空列表（调用方退回按 ID 查找）。
func ToMultiaddrs(code string) (peer.ID, []ma.Multiaddr, error) {
	id, hostPort, err := Decode(code)
	if err != nil {
		return "", nil, err
	}
	pid, derr := peer.Decode(id)
	if derr != nil {
		return "", nil, fmt.Errorf("节点 ID 无效: %w", derr)
	}
	if hostPort == "" {
		return pid, nil, nil
	}
	// 同时给出 TCP 与 UDP(QUIC) 两种传输：拨号侧按可达性自动选择。
	host, port, _ := net.SplitHostPort(hostPort)
	out := make([]ma.Multiaddr, 0, 2)
	ip := net.ParseIP(host)
	if ip == nil {
		return pid, nil, fmt.Errorf("地址段不是合法 IP: %q", host)
	}
	mustAddr := func(s string) ma.Multiaddr {
		m, merr := ma.NewMultiaddr(s)
		if merr != nil {
			panic(merr) // 参数由本函数内部构造，格式恒合法
		}
		return m
	}
	if ip4 := ip.To4(); ip4 != nil {
		out = append(out,
			mustAddr(fmt.Sprintf("/ip4/%s/tcp/%s", host, port)),
			mustAddr(fmt.Sprintf("/ip4/%s/udp/%s/quic-v1", host, port)),
		)
	} else {
		out = append(out,
			mustAddr(fmt.Sprintf("/ip6/%s/tcp/%s", host, port)),
			mustAddr(fmt.Sprintf("/ip6/%s/udp/%s/quic-v1", host, port)),
		)
	}
	return pid, out, nil
}
