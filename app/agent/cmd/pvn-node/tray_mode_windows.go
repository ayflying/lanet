//go:build windows

package main

// Windows「托盘伴侣」进程。
//
// 为什么必须单独一个进程来画图标：节点以 Windows 服务（LocalSystem）运行时
// 跑在 **Session 0**，而任务栏与 Explorer 在用户会话——这是 Vista 起的
// Session 0 隔离，不是权限问题。服务里调 Shell_NotifyIcon 只会把图标注册到
// 没有任务栏的「服务桌面」上，用户永远看不到（0.5.46 及以前服务模式干脆
// 跳过托盘，日志里那句「不启动托盘和浏览器」就是它）。
//
// 所以托盘交给用户会话里的本进程：装服务时注册一个「登录时触发」的计划任务
// （见 tray_autostart_windows.go），登录后由它拉起 `lanet.exe -tray`。
//
// 本进程**不是节点**：不建 TUN、不开地址簿、不抢控制台端口，也不参与单实例
// 锁判重——只是只读 lanet.lock，问出节点控制台的真实地址（端口回退后配置值
// 是过期的）与版本。职责仅三项：画图标、开控制台、把节点状态显示在悬浮提示
// 与菜单里。节点该不该跑、跑没跑，都由服务决定，托盘不插手。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/systray"
)

const (
	// trayModeArg 托盘伴侣模式参数（计划任务命令行里带）。
	trayModeArg = "-tray"
	// trayLockName 托盘单实例锁文件名（与节点锁分开，两者互不影响）。
	trayLockName = "tray.lock"
	// trayRefreshInterval 节点状态刷新间隔。
	trayRefreshInterval = 15 * time.Second
	// trayHTTPTimeout 单次状态查询超时。
	trayHTTPTimeout = 3 * time.Second
)

// trayLockFile 本进程持有的托盘锁，持有到进程结束（靠内核释放，无需显式解锁）。
var trayLockFile *os.File

// hasTrayArg 命令行是否要求进入托盘伴侣模式。
func hasTrayArg(args []string) bool {
	for _, a := range args {
		if strings.EqualFold(a, trayModeArg) || strings.EqualFold(a, "--tray") {
			return true
		}
	}
	return false
}

// runTrayCompanion 处理 -tray。返回 handled=true 时调用方不再进入节点逻辑。
func runTrayCompanion() (bool, error) {
	if !hasTrayArg(os.Args[1:]) {
		return false, nil
	}
	cfgPath := serviceConfigPath()
	cfgDir := filepath.Dir(cfgPath)
	// 托盘日志与节点日志分开写：两个写入者共享 lanet.log 会让文件里时间戳
	// 非单调、行号错序（排查时 tail 到的不是最新行）。
	setupTrayLog(cfgDir)
	if err := acquireTraySingleton(cfgDir); err != nil {
		log.Printf("[tray] %v，本次启动直接退出", err)
		return true, nil
	}
	log.Printf("[tray] 托盘伴侣启动（config=%s pid=%d）", cfgPath, os.Getpid())
	runTrayUI(cfgPath)
	log.Printf("[tray] 托盘已退出")
	return true, nil
}

// setupTrayLog 托盘日志单独落 lanet-tray.log（含轮转），失败则保持默认 stderr。
func setupTrayLog(dir string) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if lf, err := newRotatingFile(filepath.Join(dir, "lanet-tray.log"), logMaxSize, logMaxBackups); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{lf}})
	}
}

// acquireTraySingleton 保证同一配置目录只有一个托盘进程：计划任务可能被重复
// 触发（用户在任务计划程序里点「运行」），多开会在任务栏堆出多个同款图标。
// 锁文件不可用时降级放行——多一个图标远好过托盘彻底起不来。
func acquireTraySingleton(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, trayLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		log.Printf("[tray] 托盘锁文件不可用（继续启动）: %v", err)
		return nil
	}
	if err := tryLockFile(f); err != nil {
		if isLockBusy(err) {
			_ = f.Close()
			return fmt.Errorf("已有托盘进程在运行")
		}
		log.Printf("[tray] 托盘锁不可用（继续启动）: %v", err)
		_ = f.Close()
		return nil
	}
	trayLockFile = f
	return nil
}

// trayEndpoint 托盘访问节点控制台所需的信息。
type trayEndpoint struct {
	url      string // 形如 http://127.0.0.1:8900
	password string // 控制台访问密码（未设置时为空，即免认证）
}

// trayStatus 一次状态查询的结果，用于渲染菜单与悬浮提示。
type trayStatus struct {
	reachable bool
	name      string
	version   string
	virtualIP string
	members   int
	online    int
	pending   int
	errMsg    string
}

// runTrayUI 起托盘图标与菜单，阻塞直到用户选择「退出托盘」。
func runTrayUI(cfgPath string) {
	ep := resolveTrayEndpoint(cfgPath)
	client := newTrayClient()
	var st trayStatus

	systray.Run(func() {
		systray.SetIcon(trayIconICO)
		systray.SetTooltip("Lanet 虚拟局域网节点")

		mTitle := systray.AddMenuItem("Lanet", "节点版本")
		mAddr := systray.AddMenuItem("—", "本机虚拟 IP")
		mMembers := systray.AddMenuItem("状态获取中…", "同群成员")
		mPending := systray.AddMenuItem("", "等待你同意的连接申请")
		mPending.Hide()
		for _, it := range []*systray.MenuItem{mTitle, mAddr, mMembers} {
			it.Disable()
		}
		systray.AddSeparator()
		mOpen := systray.AddMenuItem("打开控制台", "在浏览器中打开 Web 控制台")
		mRefresh := systray.AddMenuItem("刷新状态", "立即重新读取节点状态")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出托盘", "只关闭托盘图标，节点继续运行")

		render := func() {
			renderTrayStatus(&st, mTitle, mAddr, mMembers, mPending)
		}
		refresh := func() {
			st = queryTrayStatus(client, ep)
			render()
		}

		go func() {
			refresh()
			ticker := time.NewTicker(trayRefreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-mOpen.ClickedCh:
					openBrowser(ep.url)
				case <-mPending.ClickedCh:
					openBrowser(ep.url)
				case <-mRefresh.ClickedCh:
					refresh()
				case <-ticker.C:
					refresh()
				case <-mQuit.ClickedCh:
					log.Printf("[tray] 用户选择退出托盘（节点不受影响）")
					systray.Quit()
					return
				}
			}
		}()
	}, func() {})
}

// renderTrayStatus 依据最新状态刷新菜单文本与悬浮提示。
func renderTrayStatus(st *trayStatus, mTitle, mAddr, mMembers, mPending *systray.MenuItem) {
	if !st.reachable {
		systray.SetTooltip("Lanet · 节点未运行")
		mTitle.SetTitle("Lanet · 节点未运行")
		mAddr.SetTitle("点「打开控制台」查看详情")
		mMembers.SetTitle("—")
		mPending.Hide()
		return
	}
	name := st.name
	if name == "" {
		name = "Lanet"
	}
	ver := st.version
	if ver != "" {
		ver = " v" + ver
	}
	systray.SetTooltip(fmt.Sprintf("Lanet%s · %s\n虚拟 IP %s\n成员 %d 人（在线 %d）",
		ver, name, nonEmpty(st.virtualIP, "—"), st.members, st.online))
	mTitle.SetTitle(fmt.Sprintf("Lanet%s · %s", ver, name))
	mAddr.SetTitle("虚拟 IP " + nonEmpty(st.virtualIP, "—"))
	if st.pending > 0 {
		mMembers.SetTitle(fmt.Sprintf("成员 %d 人 · 在线 %d", st.members, st.online))
		mPending.SetTitle(fmt.Sprintf("待审批 %d 项", st.pending))
		mPending.Show()
	} else {
		mMembers.SetTitle(fmt.Sprintf("成员 %d 人 · 在线 %d", st.members, st.online))
		mPending.Hide()
	}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// resolveTrayEndpoint 定位节点控制台地址。
//
// 优先级：lanet.lock 里节点回报的实际地址 > lanet.json 的 console > 默认
// 127.0.0.1:8900。前者最准：控制台端口被占用时节点会向后回退，配置值仍是
// 8900，照着配置连只会连到空气或无关程序上。
func resolveTrayEndpoint(cfgPath string) trayEndpoint {
	ep := trayEndpoint{url: "http://127.0.0.1:8900"}
	if cfg := readTrayConfig(cfgPath); cfg != nil {
		ep.password = cfg.ConsolePassword
		if u := normalizeConsoleURL(cfg.Console); u != "" {
			ep.url = u
		}
	}
	if li := readTrayLockInfo(cfgPath); li != nil && li.ConsoleURL != "" {
		ep.url = li.ConsoleURL
	}
	if !trayConsoleAlive(ep.url) {
		// 兜底探一次 8901：端口回退的另一种可能（8900 被无关程序占用时节点会 +1）。
		if alt := swapPort(ep.url, 8901); alt != "" && trayConsoleAlive(alt) {
			ep.url = alt
		}
	}
	log.Printf("[tray] 节点控制台 = %s", ep.url)
	return ep
}

// trayConfigView 节点配置里托盘关心的两个字段（只读，不引入完整配置结构，
// 避免托盘跟着节点配置字段变动而耦合）。
type trayConfigView struct {
	Console         string `json:"console"`
	ConsolePassword string `json:"console_password"`
}

// readTrayConfig 读节点配置文件里托盘要用到的字段。读不到/解析失败返回 nil。
func readTrayConfig(path string) *trayConfigView {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg trayConfigView
	if err := decodeConfigJSON(data, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// readTrayLockInfo 只读读取节点写的单实例锁内容（pid/name/version/控制台地址）。
// 锁是内核级建议锁，数据区在锁区之外，因此别的进程能读——这正是它存在的意义。
func readTrayLockInfo(cfgPath string) *singletonInfo {
	f, err := os.Open(singletonLockPath(cfgPath))
	if err != nil {
		return nil
	}
	defer f.Close()
	return readSingletonInfo(f)
}

// normalizeConsoleURL 把配置里的监听地址整理成可访问的 http URL：
// 0.0.0.0 / :: / 空主机一律换成 127.0.0.1（托盘与节点同机），缺端口补 8900。
func normalizeConsoleURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "-" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if host == "" || host == "0.0.0.0" || host == "::" || host == "0:0:0:0:0:0:0:0" {
		host = "127.0.0.1"
	}
	port := u.Port()
	if port == "" {
		port = "8900"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// swapPort 替换 URL 的端口，解析失败返回空串。
func swapPort(raw string, port int) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
}

// newTrayClient 带 cookie jar 的 HTTP 客户端：控制台启用访问密码时，登录一次
// 拿到的会话 Cookie 需要跨请求复用。
func newTrayClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: trayHTTPTimeout, Jar: jar}
}

// trayConsoleAlive 探测控制台是否可达（任意 HTTP 响应都算活着：未登录会回
// 401/303，那也说明端口后面确实是 lanet）。
func trayConsoleAlive(base string) bool {
	if base == "" {
		return false
	}
	client := &http.Client{Timeout: trayHTTPTimeout}
	resp, err := client.Get(base + "/api/state")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// queryTrayStatus 拉一次控制台状态。任何失败都返回 reachable=false + 原因，
// 由调用方渲染成「节点未运行」，绝不 panic、不阻塞菜单。
func queryTrayStatus(client *http.Client, ep trayEndpoint) trayStatus {
	st := trayStatus{}
	raw, err := trayGetState(client, ep)
	if err != nil {
		st.errMsg = err.Error()
		return st
	}
	st.reachable = true
	if info, ok := raw["info"].(map[string]any); ok {
		st.name, _ = info["name"].(string)
		st.virtualIP, _ = info["virtual_ip"].(string)
	}
	if members, ok := raw["members"].([]any); ok {
		st.members = len(members)
		for _, m := range members {
			if mv, ok := m.(map[string]any); ok {
				if on, _ := mv["online"].(bool); on {
					st.online++
				}
			}
		}
	}
	if n, ok := raw["pending_count"].(float64); ok {
		st.pending = int(n)
	}
	// 版本号以节点进程自己写的锁文件为准（/api/state 里没有顶层版本字段）。
	if li := readTrayLockInfo(serviceConfigPath()); li != nil {
		st.version = li.Version
	}
	return st
}

// trayGetState 取 /api/state；启用访问密码时用配置里的密码登录一次再重试。
func trayGetState(client *http.Client, ep trayEndpoint) (map[string]any, error) {
	raw, code, err := trayFetchState(client, ep.url)
	if err != nil {
		return nil, err
	}
	if code == http.StatusUnauthorized && ep.password != "" {
		if err := trayLogin(client, ep); err != nil {
			return nil, err
		}
		raw, code, err = trayFetchState(client, ep.url)
		if err != nil {
			return nil, err
		}
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("控制台返回 %d", code)
	}
	if raw == nil {
		return nil, fmt.Errorf("控制台响应为空")
	}
	return raw, nil
}

func trayFetchState(client *http.Client, base string) (map[string]any, int, error) {
	resp, err := client.Get(base + "/api/state")
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// trayLogin 用配置文件里的密码换会话 Cookie。
func trayLogin(client *http.Client, ep trayEndpoint) error {
	form := url.Values{"password": {ep.password}}
	resp, err := client.PostForm(ep.url+"/login", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
