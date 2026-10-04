package tundevice

import "fmt"

// Windows 系统防火墙（Windows Defender Firewall）放行脚本生成。
//
// 背景（生产实测 tianzong-pc，2026-10-04）：新建 Wintun 网卡会被 NLA 归入
// 「未识别网络」= 公用配置文件，公用配置默认拦截一切入站 ICMP/TCP——表现为
// 「成员表在线、libp2p 回显探测正常，但所有节点 ping/TCP 它的虚拟 IP 全部
// 超时」。本文件只负责生成脚本（纯函数，跨平台可测），执行在
// osfirewall_windows.go；脚本生成的命令必须幂等（节点每次启动都会执行）。

// OSFirewallRuleName 入站放行规则的固定 Name（幂等键）。规则绑定网卡的
// InterfaceAlias，只放行经虚拟网卡到达的流量，不碰物理网卡策略。
const OSFirewallRuleName = "LanetTunInbound"

// osFirewallNameSafe 校验网卡名可安全嵌入单引号 PowerShell 字面量。
// 网卡名来自用户配置（默认 lanet），含引号/分隔符时宁可跳过也不注入。
func osFirewallNameSafe(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch r {
		case '\'', '"', '`', '$', ';', '\n', '\r':
			return false
		}
	}
	return true
}

// OSFirewallRuleScript 生成「确保入站放行规则存在且绑定该网卡」的脚本。
//   - 规则不存在 → 新建：任意协议（ICMPv4/ICMPv6/TCP/UDP 一并覆盖）、
//     方向入站、动作允许、默认全配置文件（含公用）、绑定 -InterfaceAlias。
//   - 规则已存在 → 仅 Set 更新绑定与启用状态（绝不做删了重建，避免防御窗口）。
//
// 虚拟网自身已有应用层 allow-list（tundevice.Router.SetFirewall）把关谁能
// 建隧道；操作系统层对组内流量保持畅通符合产品语义（组内成员可通过虚拟 IP
// 直接访问本机服务）。
func OSFirewallRuleScript(name string) string {
	return fmt.Sprintf(
		"$r = Get-NetFirewallRule -Name '%s' -ErrorAction SilentlyContinue; "+
			"if ($r) { Set-NetFirewallRule -Name '%s' -Enabled True -InterfaceAlias '%s' } "+
			"else { New-NetFirewallRule -Name '%s' -DisplayName 'Lanet Virtual Network (TUN) Inbound' "+
			"-Description 'Allow inbound from lanet virtual network members (app-level allow-list applies separately)' "+
			"-Direction Inbound -Action Allow -InterfaceAlias '%s' | Out-Null }",
		OSFirewallRuleName, OSFirewallRuleName, name, OSFirewallRuleName, name)
}

// OSFirewallProfileScript 生成「网卡网络类别 → 专用」的脚本。
// 专用（Private）是防御纵深：公用配置文件除入站默认策略外还会拦网络发现等；
// 即便第 1 条规则已放行，类别修正也能让系统对该网卡按局域网语义对待。
func OSFirewallProfileScript(name string) string {
	return fmt.Sprintf("Set-NetConnectionProfile -InterfaceAlias '%s' -NetworkCategory Private", name)
}
