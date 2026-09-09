//go:build windows

// .lanet 虚拟域名的系统级 DNS 路由：Windows NRPT（Name Resolution Policy
// Table）。节点以管理员运行（exe 内嵌 requireAdministrator 清单），启动时
// 注册一条 NRPT 规则把 *.lanet 后缀的查询定向到本机内置 DNS 服务
// （127.0.0.1:53），退出时移除。
//
// 为什么用 NRPT 而不是改 /etc/hosts：
//   - 成员虚拟 IP 变化时解析自动跟随（DNS 实时应答），hosts 是死数据；
//   - NRPT 是策略表（HKLM\...\DnsPolicy），不碰用户网络适配器 DNS 配置，
//     断网/换网/VPN 场景不影响系统其余解析行为；
//   - 规则只覆盖 .lanet 后缀，其他域名照常走原 DNS。
//
// 兼容性：NRPT 自 Windows 8/Server 2012 起内置；规则以命名空间
// ".lanet"（含子域）注册。PowerShell cmdlet 失败时降级为直接写注册表。
package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// nrptNamespace 被 NRPT 路由的域名后缀（含全部子域）。
const nrptNamespace = ".lanet"

// nrptRuleRegistryPath NRPT 规则在策略注册表中的位置。
const nrptRuleRegistryPath = `SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`

// ensureNRPTRule 幂等注册 NRPT 规则（存在则跳过）。返回错误仅记日志用。
func ensureNRPTRule(dnsServer string) error {
	if nrptRuleExists() {
		return nil
	}
	// 优先 PowerShell cmdlet（语义明确，处理转义）；失败降级注册表直写。
	ps := fmt.Sprintf(`Add-DnsClientNrptRule -Namespace "%s" -NameServers "%s" -Comment "lanet virtual domain"`, nrptNamespace, dnsServer)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	hideConsoleWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if regErr := nrptRuleViaRegistry(dnsServer); regErr != nil {
			return fmt.Errorf("Add-DnsClientNrptRule 失败 (%v)，注册表直写也失败 (%v): %s",
				err, regErr, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// removeNRPTRule 幂等移除 NRPT 规则（不存在视为成功）。
func removeNRPTRule() error {
	if !nrptRuleExists() {
		return nil
	}
	ps := fmt.Sprintf(`Get-DnsClientNrptRule | Where-Object { $_.Namespace -like "*%s" } | Remove-DnsClientNrptRule -Force`, strings.TrimPrefix(nrptNamespace, "."))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	hideConsoleWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if regErr := nrptRemoveViaRegistry(); regErr != nil {
			return fmt.Errorf("Remove-DnsClientNrptRule 失败 (%v)，注册表直删也失败 (%v): %s",
				err, regErr, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// nrptRuleExists 检查 NRPT 规则是否已注册（按命名空间匹配）。
func nrptRuleExists() bool {
	ps := fmt.Sprintf(`if (Get-DnsClientNrptRule | Where-Object { $_.Namespace -like "*%s" }) { exit 0 } else { exit 1 }`, strings.TrimPrefix(nrptNamespace, "."))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	hideConsoleWindow(cmd)
	return cmd.Run() == nil
}

// nrptRuleViaRegistry 直接写策略注册表创建规则（cmdlet 不可用时的降级路径）。
// 结构参考 NRPT 官方 GPO 布局：{GUID} 子键，Version/DnsServers/Namespace/ConfigFlags。
func nrptRuleViaRegistry(dnsServer string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRuleRegistryPath, registry.CREATE_SUB_KEY|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开策略注册表失败: %w", err)
	}
	defer key.Close()
	sub, _, err := registry.CreateKey(key, "{7BC1A1F2-LANET-DNS-RULE-000000000001}", registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("创建规则键失败: %w", err)
	}
	defer sub.Close()
	_ = sub.SetDWordValue("Version", 2)
	_ = sub.SetStringsValue("DnsServers", []string{dnsServer})
	_ = sub.SetStringsValue("Namespace", []string{nrptNamespace})
	_ = sub.SetDWordValue("ConfigFlags", 0x00000001) // 名字空间启用
	_ = sub.SetStringValue("DisplayName", "lanet virtual domain")
	return nil
}

// nrptRemoveViaRegistry 直接删除规则键。
func nrptRemoveViaRegistry() error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRuleRegistryPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开策略注册表失败: %w", err)
	}
	defer key.Close()
	return registry.DeleteKey(key, "{7BC1A1F2-LANET-DNS-RULE-000000000001}")
}

// hideConsoleWindow 阻止 PowerShell 子进程闪黑框（GUI 程序每 5 秒闪一次
// 是不可接受的，与邻居表写入同款处理）。
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
