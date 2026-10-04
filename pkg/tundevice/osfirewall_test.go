package tundevice

import (
	"strings"
	"testing"
)

// TestOSFirewallRuleScriptIdempotent 放行规则脚本必须幂等：存在即 Set 更新，
// 不存在才 New——节点每次启动都会执行，绝不能删了重建制造防御窗口。
func TestOSFirewallRuleScriptIdempotent(t *testing.T) {
	script := OSFirewallRuleScript("lanet")
	for _, want := range []string{
		"Get-NetFirewallRule -Name 'LanetTunInbound'",
		"Set-NetFirewallRule -Name 'LanetTunInbound' -Enabled True -InterfaceAlias 'lanet'",
		"New-NetFirewallRule -Name 'LanetTunInbound'",
		"-Direction Inbound -Action Allow",
		"-InterfaceAlias 'lanet'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺少 %q：\n%s", want, script)
		}
	}
	if strings.Contains(script, "Remove-NetFirewallRule") {
		t.Fatalf("脚本不得删除重建规则（防御窗口）：\n%s", script)
	}
	// New 只能出现在 else 分支里（规则已存在时不得重复创建）。
	if idx := strings.Index(script, "else"); idx < 0 || strings.Index(script, "New-NetFirewallRule") < idx {
		t.Fatalf("New-NetFirewallRule 必须位于 else 分支（幂等）：\n%s", script)
	}
}

// TestOSFirewallProfileScript 类别脚本：绑定网卡 + 设为专用。
func TestOSFirewallProfileScript(t *testing.T) {
	script := OSFirewallProfileScript("lanet")
	if !strings.Contains(script, "Set-NetConnectionProfile -InterfaceAlias 'lanet' -NetworkCategory Private") {
		t.Fatalf("类别脚本不符合预期：%s", script)
	}
}

// TestOSFirewallNameSafe 网卡名校验：默认名放行，注入字符一律拒绝。
func TestOSFirewallNameSafe(t *testing.T) {
	if !osFirewallNameSafe("lanet") {
		t.Fatal("默认网卡名 lanet 应当放行")
	}
	if osFirewallNameSafe("") {
		t.Fatal("空名应拒绝")
	}
	for _, bad := range []string{"la'net", `la"net`, "la`net", "la$net", "la;net", "la\nnet", "la\rnet"} {
		if osFirewallNameSafe(bad) {
			t.Fatalf("含特殊字符的网卡名 %q 应拒绝", bad)
		}
	}
}
