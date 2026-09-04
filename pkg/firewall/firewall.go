// Package firewall 提供入向 PortFWD（本机/局域网服务暴露）的防火墙规则：
//
//   - deny-all（默认）：拒绝一切入向转发请求；
//   - allow-list：按规则放行（来源虚拟 IP/网段 + 目标端口）；
//   - allow-all：全开（任意成员访问任意端口）。
//
// 规则针对「来源虚拟 IP + 请求端口」二元组判定，在发起对内网目标的
// TCP 连接之前执行；并发安全，支持运行中热更新（Web 控制台）。
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
	// ModeDenyAll 默认：拒绝一切入向转发。
	ModeDenyAll Mode = "deny-all"
	// ModeAllowList 按规则列表放行。
	ModeAllowList Mode = "allow-list"
	// ModeAllowAll 全开：任意来源、任意端口。
	ModeAllowAll Mode = "allow-all"
)

// Rule 放行规则。
type Rule struct {
	// Source 来源虚拟 IP（100.64.x.x）、CIDR（100.64.0.0/16）或 "*"（任意成员）。
	Source string `json:"source"`
	// Port 目标端口：单端口 "3389"、范围 "5000-5010" 或 "*"（任意端口）。
	Port string `json:"port"`
}

// Firewall 入向转发防火墙。
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

// Allow 判定来源虚拟 IP 访问端口 port 是否放行。
// sourceIP 为空（来源未知/不在成员表）时除 allow-all 外一律拒绝。
func (f *Firewall) Allow(sourceIP string, port int) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch f.mode {
	case ModeAllowAll:
		return true
	case ModeAllowList:
		for _, r := range f.rules {
			if sourceMatch(r.Source, sourceIP) && portMatch(r.Port, port) {
				return true
			}
		}
		return false
	default: // deny-all
		return false
	}
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
	if strings.Contains(pattern, "/") {
		if _, cidr, err := net.ParseCIDR(pattern); err == nil {
			if n := net.ParseIP(ip); n != nil {
				return cidr.Contains(n)
			}
		}
	}
	return false
}

// portMatch 端口匹配："*"、"3389" 或 "5000-5010"。
func portMatch(pattern string, port int) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" {
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
