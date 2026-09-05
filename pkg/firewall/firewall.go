// Package firewall 提供节点全部入向暴露面的统一防火墙：
//
//   - PortFWD（本机/局域网 TCP 服务转发）—— 传输层 tcp 端口；
//   - TUN 虚拟网卡入向（IP 层：本机/局域网任意 TCP/UDP 端口）—— 传输层 tcp/udp 端口；
//   - OnStream 应用流（libp2p 协议 ID，如 /pvn/tunnel/1.0.0）—— 协议维度。
//
// 三种模式：
//
//   - deny-all（默认）：拒绝一切入向（三类暴露面都拒）；
//   - allow-list：按规则放行（来源虚拟 IP/网段 + 协议 + 端口）；
//   - allow-all：全开。
//
// 判定在「向本机/内网目标发起连接或把包写进协议栈」之前执行；
// 并发安全，支持运行中热更新（Web 控制台）。
package firewall

import (
	"net"
	"strconv"
	"strings"
	"sync"
)

// Mode 防火墙模式。
type Mode string

const (
	// ModeDenyAll 默认：拒绝一切入向。
	ModeDenyAll Mode = "deny-all"
	// ModeAllowList 按规则列表放行。
	ModeAllowList Mode = "allow-list"
	// ModeAllowAll 全开：任意来源、任意协议、任意端口。
	ModeAllowAll Mode = "allow-all"
)

// 传输层协议常量（Rule.Proto 取值；协议 ID 以 "/" 开头，不会与之混淆）。
const (
	ProtoTCP = "tcp"
	ProtoUDP = "udp"
	ProtoAny = "*"
)

// Rule 放行规则。
type Rule struct {
	// Source 来源虚拟 IP（10.7.x.x）、CIDR（10.7.0.0/16）或 "*"（任意成员）。
	Source string `json:"source"`
	// Proto 协议维度：
	//   - "tcp"（默认/留空）：TCP 端口（PortFWD 与 TUN 入向 TCP）；
	//   - "udp"：UDP 端口（TUN 入向 UDP）；
	//   - "/" 开头：libp2p 应用流协议 ID（如 "/pvn/tunnel/1.0.0"，管控 OnStream 入向）；
	//   - "*"：全部协议。
	Proto string `json:"proto,omitempty"`
	// Port 目标端口：单端口 "3389"、范围 "5000-5010" 或 "*"（任意端口）。
	// 协议 ID 规则忽略本字段（等价 "*"）。
	Port string `json:"port"`
}

// Firewall 入向统一防火墙。
type Firewall struct {
	mu    sync.RWMutex
	mode  Mode
	rules []Rule
}

// New 创建防火墙，默认 deny-all。
func New() *Firewall {
	return &Firewall{mode: ModeDenyAll}
}

// Set 热更新模式与规则（allow-list 之外的 mode 忽略 rules）。
func (f *Firewall) Set(mode Mode, rules []Rule) {
	if mode == "" {
		mode = ModeDenyAll
	}
	if rules == nil {
		rules = []Rule{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
	f.rules = rules
}

// Snapshot 返回当前模式与规则（控制台展示用）。
func (f *Firewall) Snapshot() (Mode, []Rule) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Rule, len(f.rules))
	copy(out, f.rules)
	return f.mode, out
}

// Allow 判定来源虚拟 IP 以传输层协议 proto（"tcp"/"udp"）访问端口 port
// 是否放行（PortFWD 与 TUN 入向共用）。sourceIP 为空（来源未知/不在
// 成员表/IP 包源地址非法）时除 allow-all 外一律拒绝。
func (f *Firewall) Allow(sourceIP, proto string, port int) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch f.mode {
	case ModeAllowAll:
		return true
	case ModeAllowList:
		for _, r := range f.rules {
			if sourceMatch(r.Source, sourceIP) && protoMatch(r.Proto, proto) && portMatch(r.Port, port) {
				return true
			}
		}
		return false
	default: // deny-all
		return false
	}
}

// AllowStream 判定来源虚拟 IP 打开 libp2p 应用流（协议 protoID，
// 如 "/pvn/tunnel/1.0.0"）是否放行（OnStream 入向共用）。
// 匹配规则：Proto 恰为 protoID（或 "*"），Port 需为 "*" 或空。
func (f *Firewall) AllowStream(sourceIP, protoID string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch f.mode {
	case ModeAllowAll:
		return true
	case ModeAllowList:
		for _, r := range f.rules {
			if !sourceMatch(r.Source, sourceIP) {
				continue
			}
			if r.Proto == protoID && streamPortOK(r.Port) {
				return true
			}
			if r.Proto == ProtoAny && streamPortOK(r.Port) {
				return true
			}
		}
		return false
	default: // deny-all
		return false
	}
}

// streamPortOK 协议流规则的 Port 字段约束：空或 "*"。
func streamPortOK(p string) bool {
	p = strings.TrimSpace(p)
	return p == "" || p == "*"
}

// sourceMatch 来源匹配："*"、精确 IP 或 CIDR（空规则不匹配任何来源）。
// 未知来源（ip 为空）除 allow-all 模式外一律不匹配。
func sourceMatch(pattern, ip string) bool {
	if ip == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if pattern == ip {
		return true
	}
	if strings.Contains(pattern, "/") && !strings.HasPrefix(pattern, "/") {
		if _, cidr, err := net.ParseCIDR(pattern); err == nil {
			if n := net.ParseIP(ip); n != nil {
				return cidr.Contains(n)
			}
		}
	}
	return false
}

// protoMatch 传输层协议匹配：规则留空视为 tcp；"*" 匹配全部。
func protoMatch(ruleProto, proto string) bool {
	if ruleProto == "" {
		ruleProto = ProtoTCP
	}
	if ruleProto == ProtoAny || ruleProto == proto {
		return true
	}
	return false
}

// portMatch 端口匹配："*"、"3389" 或 "5000-5010"。
func portMatch(pattern string, port int) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" || pattern == "" {
		return true
	}
	if hyphen := strings.IndexByte(pattern, '-'); hyphen > 0 {
		lo, err1 := strconv.Atoi(strings.TrimSpace(pattern[:hyphen]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(pattern[hyphen+1:]))
		return err1 == nil && err2 == nil && port >= lo && port <= hi
	}
	n, err := strconv.Atoi(pattern)
	return err == nil && n == port
}
