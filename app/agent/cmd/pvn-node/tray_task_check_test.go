//go:build windows

package main

// 托盘任务「定义过期」判定的回归测试。
//
// 核心用例是 2026-10-26 真机故障的原始任务定义：控制台在**未提升**的上下文里注册
// 托盘任务，Windows 静默丢弃 RunLevel=HighestAvailable，于是这个任务只剩
// InteractiveToken；而 lanet.exe 内嵌 requireAdministrator 清单，登录启动直接
// 失败 0x800702E4，图标一直不出现。若判定逻辑退化回「任务存在就拉起」，本测试会失败。

import (
	"strings"
	"testing"
)

// staleTrayTaskXML 真机抓取的任务定义（缺 <RunLevel>，触发用户 ANYANG-PC\ay）。
const staleTrayTaskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Lanet 托盘图标：用户登录时在用户会话中启动</Description>
    <URI>\Lanet Tray</URI>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-21-2079961313-745938444-792566237-1002</UserId>
      <LogonType>InteractiveToken</LogonType>
    </Principal>
  </Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Triggers>
    <LogonTrigger>
      <UserId>ANYANG-PC\ay</UserId>
    </LogonTrigger>
  </Triggers>
  <Actions Context="Author">
    <Exec>
      <Command>D:\lanet-node\lanet.exe</Command>
      <Arguments>-tray -config "D:\lanet-node\lanet.json"</Arguments>
      <WorkingDirectory>D:\lanet-node</WorkingDirectory>
    </Exec>
  </Actions>
</Task>`

// healthyTrayTaskXML 服务（SYSTEM）按当前定义重建后应有的形态。
const healthyTrayTaskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-21-2079961313-745938444-792566237-1002</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Triggers>
    <LogonTrigger>
      <UserId>ANYANG-PC\ay</UserId>
    </LogonTrigger>
  </Triggers>
  <Actions Context="Author">
    <Exec>
      <Command>D:\lanet-node\lanet.exe</Command>
      <Arguments>-tray -config "D:\lanet-node\lanet.json"</Arguments>
      <WorkingDirectory>D:\lanet-node</WorkingDirectory>
    </Exec>
  </Actions>
</Task>`

// TestTrayTaskStaleDetectsMissingRunLevel 真机过期定义必须被判为过期，且原因点明
// 登录启动会因提升失败（0x800702E4）——这是图标消失的真实成因，不能被静默吞掉。
func TestTrayTaskStaleDetectsMissingRunLevel(t *testing.T) {
	parsed, err := parseTrayInstalledTask(staleTrayTaskXML)
	if err != nil {
		t.Fatalf("解析真机任务定义失败: %v", err)
	}
	if parsed.Principals.Principal.RunLevel != "" {
		t.Fatalf("真机夹具应缺少 RunLevel，实际 %q", parsed.Principals.Principal.RunLevel)
	}
	reason := trayTaskStaleReason(parsed, `D:\lanet-node\lanet.exe`, `D:\lanet-node\lanet.json`, `ANYANG-PC\ay`)
	if reason == "" {
		t.Fatal("缺 RunLevel 的任务应判为过期（否则登录启动永远失败且无人修正）")
	}
	if !strings.Contains(reason, "0x800702E4") {
		t.Errorf("过期原因应点明 0x800702E4，实际 %q", reason)
	}
}

// TestTrayTaskHealthyNotStale 正确的任务定义不应被反复重建（每次服务启动都 /F
// 覆盖会打断登录触发，也可能覆盖用户手工调整过的设置）。
func TestTrayTaskHealthyNotStale(t *testing.T) {
	parsed, err := parseTrayInstalledTask(healthyTrayTaskXML)
	if err != nil {
		t.Fatalf("解析任务定义失败: %v", err)
	}
	if reason := trayTaskStaleReason(parsed, `D:\lanet-node\lanet.exe`, `D:\lanet-node\lanet.json`, `ANYANG-PC\ay`); reason != "" {
		t.Errorf("正确任务被误判为过期: %s", reason)
	}
	// 开机场景服务先于登录启动，此时不知道交互用户；不得因此判为过期。
	if reason := trayTaskStaleReason(parsed, `D:\lanet-node\lanet.exe`, `D:\lanet-node\lanet.json`, ""); reason != "" {
		t.Errorf("用户未知时不应判为过期: %s", reason)
	}
}

// TestTrayTaskStaleOnPathArgsOrUserChange 覆盖换目录、丢参数、换用户三类过期。
func TestTrayTaskStaleOnPathArgsOrUserChange(t *testing.T) {
	base, err := parseTrayInstalledTask(healthyTrayTaskXML)
	if err != nil {
		t.Fatalf("解析任务定义失败: %v", err)
	}
	exe := `D:\lanet-node\lanet.exe`
	cfg := `D:\lanet-node\lanet.json`
	cases := []struct {
		name    string
		mutate  func(*trayInstalledTask)
		wantSub string
	}{
		{"换程序目录", func(t *trayInstalledTask) { t.Actions.Exec.Command = `D:\old\lanet.exe` }, "程序路径"},
		{"丢 -tray 参数", func(t *trayInstalledTask) {
			t.Actions.Exec.Arguments = `-config "D:\lanet-node\lanet.json"`
		}, "启动参数"},
		{"丢配置路径", func(t *trayInstalledTask) { t.Actions.Exec.Arguments = "-tray" }, "启动参数"},
		{"换了登录用户", func(t *trayInstalledTask) { t.Triggers.Logon.UserID = `ANYANG-PC\other` }, "触发用户"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := base
			c.mutate(&mutated)
			reason := trayTaskStaleReason(mutated, exe, cfg, `ANYANG-PC\ay`)
			if !strings.Contains(reason, c.wantSub) {
				t.Errorf("期望原因含 %q，实际 %q", c.wantSub, reason)
			}
		})
	}
}

// TestDecodeTrayTaskXMLBytes 覆盖 schtasks 的三种真实输出形态：UTF-16LE 带 BOM
// （任务定义文件本身）、无 BOM 的 UTF-16LE、以及本地代码页含非法 UTF-8 字节。
func TestDecodeTrayTaskXMLBytes(t *testing.T) {
	const doc = `<Task><Principals><Principal><RunLevel>HighestAvailable</RunLevel></Principal></Principals></Task>`
	utf16le := func(s string, bom bool) []byte {
		var out []byte
		if bom {
			out = append(out, 0xFF, 0xFE)
		}
		for _, r := range s {
			out = append(out, byte(r), byte(r>>8))
		}
		return out
	}
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"UTF-16LE 带 BOM", utf16le(doc, true)},
		{"UTF-16LE 无 BOM", utf16le(doc, false)},
		{"UTF-8 原样", []byte(doc)},
		// GBK 的「托」是 0xCD 0xD0：单字节非法 UTF-8，必须被替换而不是让解析失败。
		{"含本地代码页字节", func() []byte {
			b := []byte(`<Task><RegistrationInfo><Description>`)
			b = append(b, 0xCD, 0xD0)
			return append(b, []byte(`</Description></RegistrationInfo></Task>`)...)
		}()},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := decodeTrayTaskXMLBytes(c.raw)
			if _, err := parseTrayInstalledTask(text); err != nil {
				t.Fatalf("归一化后仍无法解析: %v（文本=%q）", err, text)
			}
		})
	}
	if _, err := parseTrayInstalledTask(""); err == nil {
		t.Error("空定义应返回错误，交给调用方按「无可信定义」处理")
	}
}

// TestTrayTaskXMLRoundTripNotStale 自洽性：按当前版本生成的定义，判定必须为「一致」。
// 这条锁住「重建后的任务不会被自己判成过期」，否则服务每轮启动都会反复 /F 覆盖。
func TestTrayTaskXMLRoundTripNotStale(t *testing.T) {
	const user = `ANYANG-PC\ay`
	doc, err := trayTaskXMLFor(user)
	if err != nil {
		t.Fatalf("生成任务定义失败: %v", err)
	}
	parsed, err := parseTrayInstalledTask(doc)
	if err != nil {
		t.Fatalf("解析自生成定义失败: %v", err)
	}
	if reason := trayTaskStaleReason(parsed, selfExe(), serviceConfigPath(), user); reason != "" {
		t.Errorf("自生成定义被误判为过期: %s", reason)
	}
	// 反过来：把最高权限声明去掉，必须立刻判为过期。
	parsed.Principals.Principal.RunLevel = ""
	if reason := trayTaskStaleReason(parsed, selfExe(), serviceConfigPath(), user); reason == "" {
		t.Error("去掉 RunLevel 后应判为过期")
	}
}
