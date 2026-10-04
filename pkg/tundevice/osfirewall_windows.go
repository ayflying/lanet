//go:build windows

package tundevice

import (
	"log"
	"os/exec"
	"strings"
	"time"
)

// profileMaxAttempts / profileRetryDelay 网卡连接配置文件（NLA）在地址配置后
// 可能稍晚出现，Set-NetConnectionProfile 找不到对象时重试。异步执行，多等
// 一会儿不拖慢入网。
const (
	profileMaxAttempts = 5
	profileRetryDelay  = 3 * time.Second
)

// runPowerShell 执行一段 PowerShell（-NoProfile -NonInteractive），聚合输出。
func runPowerShell(script string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	hideWindow(cmd) // 与 netsh 同理：服务/GUI 进程调起不许闪黑框
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// configureOSFirewall 尽力让 Windows 防火墙放行 TUN 网卡（name=网卡名，如 lanet）：
//  1. 幂等写入绑定该网卡的入站放行规则（先做：全配置文件生效，不依赖第 2 步）；
//  2. 网络类别设为专用（公用配置文件是「节点在线但 ping/TCP 全部不通」的根因）。
//
// 全程尽力而为：非管理员/组策略受限环境逐项失败仅记日志，绝不影响入网。
func configureOSFirewall(name string) {
	if !osFirewallNameSafe(name) {
		log.Printf("[tun] 网卡名 %q 含特殊字符，跳过 Windows 防火墙自动放行（请手动放行入站）", name)
		return
	}
	if out, err := runPowerShell(OSFirewallRuleScript(name)); err != nil {
		log.Printf("[tun] Windows 防火墙入站放行规则写入失败（其它成员 ping/访问本机虚拟 IP 可能被本机拦截；需管理员）: %v: %s",
			err, strings.TrimSpace(out))
	} else {
		log.Printf("[tun] Windows 防火墙已放行虚拟网卡 %s 入站（规则 %s）", name, OSFirewallRuleName)
	}
	var err error
	for i := 0; i < profileMaxAttempts; i++ {
		var out string
		out, err = runPowerShell(OSFirewallProfileScript(name))
		if err == nil {
			log.Printf("[tun] 虚拟网卡 %s 网络类别已设为专用", name)
			return
		}
		if i < profileMaxAttempts-1 {
			time.Sleep(profileRetryDelay)
		}
		_ = out
	}
	log.Printf("[tun] 虚拟网卡 %s 网络类别设为专用失败（公用配置文件可能拦入站，可在「设置-网络」手动改为专用）: %v", name, err)
}
