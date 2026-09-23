//go:build windows

package main

// Windows「登录时拉起托盘伴侣」的计划任务注册。
//
// 为什么是计划任务，而不是 HKCU\...\Run 注册表项：
//   - 本 exe 内嵌 requireAdministrator 清单，Run 键拉起时**每次登录都会弹 UAC**，
//     用户点「否」就再也没有图标；
//   - 计划任务可以声明 RunLevel=HighestAvailable，由任务计划程序服务代为提权
//     启动，用户全程无感。
//
// 为什么 LogonType 必须是 InteractiveToken：
//   - 任务的两种运行身份里，只有 InteractiveToken（「只在用户登录时运行」）会把
//     进程放进**用户会话**，托盘图标才画得到任务栏上；另一种（不管用户是否登录
//     都运行）落在 Session 0，等于白干——这正是服务模式看不到图标的原因。
//
// 任务内容 = `lanet.exe -tray -config <节点配置>`，只画图标，不启动节点。

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trayTaskName 计划任务名（任务计划程序里显示的名字，卸载/查询都用它）。
const trayTaskName = "Lanet Tray"

// schtasksPath 取 schtasks.exe 绝对路径（不依赖 PATH；服务/计划任务的 PATH
// 与交互 shell 不同，靠名字调用容易找不到）。
func schtasksPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "schtasks.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "schtasks"
}

// runSchtasks 执行一条 schtasks 命令，返回合并输出（错误信息都在里面）。
func runSchtasks(args ...string) (string, error) {
	cmd := exec.Command(schtasksPath(), args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// currentUserID 当前登录用户的标识（`域\用户名`）。任务的 UserId 用它限定
// 「谁登录时触发」，多用户机器上不会给别人也弹图标。
func currentUserID() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return os.Getenv("USERNAME")
	}
	if strings.Contains(u.Username, `\`) {
		return u.Username
	}
	if domain := os.Getenv("USERDOMAIN"); domain != "" {
		return domain + `\` + u.Username
	}
	return u.Username
}

// trayTaskXML 生成任务定义（默认按当前进程用户）。用 XML 而不是 schtasks 命令行：
// `/TR` 里带引号的 exe 与配置路径在命令行转义下极易被截断，XML 则是结构化的，
// 没有这层风险，还能精确声明 RunLevel / LogonType / 不限执行时长。
func trayTaskXML() (string, error) {
	return trayTaskXMLFor(currentUserID())
}

// trayTaskXMLFor 按指定用户生成任务定义。服务兜底补注册时传入「交互登录用户」，
// 而非进程身份——LocalSystem 下 user.Current() 返回的是 SYSTEM，照抄会让任务
// 声明成「SYSTEM 登录时触发」，永远等不到触发。
func trayTaskXMLFor(userID string) (string, error) {
	exe := selfExe()
	cfg := serviceConfigPath()
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	userID = esc(userID)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Lanet 托盘图标：用户登录时在用户会话中启动（节点本体由 Windows 服务「Lanet」运行，服务在 Session 0 无法显示托盘）</Description>
    <URI>\%s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>-tray -config "%s"</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`, esc(trayTaskName), userID, userID, esc(exe), esc(cfg), esc(filepath.Dir(exe))), nil
}

// writeUTF16LE schtasks /XML 只接受 UTF-16（带 BOM）的 XML 文件，UTF-8 会报
// 「XML 格式不正确」。这里手工编码，不引入额外依赖。
func writeUTF16LE(path, content string) error {
	units := utf16.Encode([]rune(content))
	buf := make([]byte, 0, len(units)*2+2)
	buf = append(buf, 0xFF, 0xFE) // BOM：小端
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return os.WriteFile(path, buf, 0o600)
}

// registerTrayTask 把任务定义落盘并注册到任务计划程序（/F 覆盖，幂等）。
func registerTrayTask(doc string) error {
	f, err := os.CreateTemp("", "lanet-tray-*.xml")
	if err != nil {
		return fmt.Errorf("创建任务定义临时文件失败: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_ = f.Close()
	if err := writeUTF16LE(f.Name(), doc); err != nil {
		return fmt.Errorf("写入任务定义失败: %w", err)
	}
	if out, err := runSchtasks("/Create", "/F", "/TN", trayTaskName, "/XML", f.Name()); err != nil {
		return fmt.Errorf("注册托盘任务失败: %v（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

// installTrayAutostart 注册「登录时启动托盘」的任务；已存在则原地更新
// （/F 覆盖），所以重复开启自启是幂等的。
func installTrayAutostart() error {
	doc, err := trayTaskXML()
	if err != nil {
		return err
	}
	return registerTrayTask(doc)
}

// removeTrayAutostart 删除托盘任务。任务不存在视为成功，保证开关幂等。
func removeTrayAutostart() error {
	if !isTrayAutostartInstalled() {
		return nil
	}
	if out, err := runSchtasks("/Delete", "/F", "/TN", trayTaskName); err != nil {
		return fmt.Errorf("删除托盘任务失败: %v（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

// isTrayAutostartInstalled 托盘任务是否已注册。
func isTrayAutostartInstalled() bool {
	_, err := runSchtasks("/Query", "/TN", trayTaskName)
	return err == nil
}

// startTrayTaskNow 立即启动一次托盘任务：用户刚打开「开机自启」开关时不必
// 重新登录就能看到图标。失败只记日志——任务注册本身已经成功，下次登录照样生效。
func startTrayTaskNow() {
	if out, err := runSchtasks("/Run", "/TN", trayTaskName); err != nil {
		log.Printf("[tray] 立即启动托盘任务失败（不影响下次登录自动启动）: %v（%s）",
			err, strings.TrimSpace(out))
		return
	}
	log.Printf("[tray] 已请求任务计划程序立即启动托盘")
}

// consoleSessionUsers 枚举当前已登录交互会话的用户（域\用户名）。
//
// 服务跑在 Session 0，user.Current() 是 SYSTEM，不能用来声明「谁登录时触发」；
// 改从 explorer.exe 的进程令牌取所有者——explorer 由登录用户启动，其 token
// 即交互会话身份，LocalSystem 有权读取（TOKEN_QUERY 只需查询权限）。
// 无人登录（开机后、登录前）返回空；快速用户切换/多用户时返回全部去重结果。
func consoleSessionUsers() []string {
	const processQueryLimitedInformation = 0x1000
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	seen := map[string]bool{}
	var users []string
	for windows.Process32Next(snap, &pe) == nil {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "explorer.exe") {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			continue
		}
		var tok windows.Token
		if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err == nil {
			if u := tokenUserString(tok); u != "" && !seen[u] {
				seen[u] = true
				users = append(users, u)
			}
			tok.Close()
		}
		_ = windows.CloseHandle(h)
	}
	return users
}

// tokenUserString 取令牌对应账户的「域\用户名」；失败返回空串。
func tokenUserString(tok windows.Token) string {
	var needed uint32
	_ = windows.GetTokenInformation(tok, windows.TokenUser, nil, 0, &needed)
	if needed == 0 {
		return ""
	}
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(tok, windows.TokenUser, &buf[0], uint32(len(buf)), &needed); err != nil {
		return ""
	}
	tu := (*windows.Tokenuser)(unsafe.Pointer(&buf[0]))

	var nameBuf, domBuf [256]uint16
	nameLen := uint32(len(nameBuf))
	domLen := uint32(len(domBuf))
	var use uint32
	if err := windows.LookupAccountSid(nil, tu.User.Sid, &nameBuf[0], &nameLen, &domBuf[0], &domLen, &use); err != nil {
		return ""
	}
	name := windows.UTF16ToString(nameBuf[:nameLen])
	dom := windows.UTF16ToString(domBuf[:domLen])
	if name == "" {
		return ""
	}
	if dom == "" {
		return name
	}
	return dom + `\` + name
}

// ensureTrayAutostartFromService 服务（Session 0）启动后的托盘兜底。
//
// 为什么需要它：托盘任务只在控制台开「开机自启」时注册；若用户经 services.msc
// 手动装/启动服务、或任务被清理过，登录后永远不会有图标，且没有任何报错。
// 这里在服务启动后周期探测登录用户（开机场景服务先于用户登录是常态，explorer
// 出现即视为已登录），一旦取到交互用户就补注册任务并立即拉起托盘，最多等待
// 2 小时后放弃；任务已注册则只立即拉起一次（托盘侧有单实例锁，重复拉起安全）。
func ensureTrayAutostartFromService(ctx context.Context) {
	if isTrayAutostartInstalled() {
		startTrayTaskNow()
		return
	}
	// 开机自启的服务普遍先于用户登录：登录后 explorer 出现，下一轮探测即可注册。
	const detectEvery = time.Minute
	deadline := time.After(2 * time.Hour)
	tick := time.NewTicker(detectEvery)
	defer tick.Stop()
	for {
		users := consoleSessionUsers()
		if len(users) > 0 {
			doc, err := trayTaskXMLFor(users[0])
			if err != nil {
				log.Printf("[tray] 服务兜底生成任务定义失败: %v", err)
				return
			}
			if err := registerTrayTask(doc); err != nil {
				// SYSTEM 创建任务失败通常是策略限制；交还用户走控制台开关。
				log.Printf("[tray] 服务兜底补注册托盘任务失败（请从控制台重开一次「开机自启」）: %v", err)
				return
			}
			log.Printf("[tray] 服务兜底：已补注册托盘任务（登录用户 %s）", users[0])
			startTrayTaskNow()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			log.Printf("[tray] 服务兜底：等待登录用户超时，放弃本次补注册")
			return
		case <-tick.C:
		}
	}
}
