package lanet

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

//go:embed console/index.html console/logo.png
var consoleFS embed.FS

// 防火墙类型别名：SDK 用户无需直接 import pkg/firewall。
type (
	// FirewallMode 防火墙模式。
	FirewallMode = firewall.Mode
	// FirewallRule 防火墙放行规则。
	FirewallRule = firewall.Rule
)

const (
	// FirewallModeDenyAll 默认：拒绝一切入向。
	FirewallModeDenyAll = firewall.ModeDenyAll
	// FirewallModeAllowList 按规则列表放行。
	FirewallModeAllowList = firewall.ModeAllowList
	// FirewallModeAllowAll 全开：任意来源、任意协议、任意端口。
	FirewallModeAllowAll = firewall.ModeAllowAll
	// FirewallProtoTCP 传输层 TCP（PortFWD 与 TUN 入向 TCP）。
	FirewallProtoTCP = firewall.ProtoTCP
	// FirewallProtoUDP 传输层 UDP（TUN 入向 UDP）。
	FirewallProtoUDP = firewall.ProtoUDP
	// FirewallProtoAny 全部协议（含 libp2p 应用流）。
	FirewallProtoAny = firewall.ProtoAny
)

// consoleState 控制台可热更状态的持久化结构。
type consoleState struct {
	Mode     firewall.Mode   `json:"mode"`
	Rules    []firewall.Rule `json:"rules"`
	Forwards []LANForward    `json:"forwards"`
}

// startConsole 启动内置 Web 控制台（默认 127.0.0.1:8900，占用时向后尝试）。
// ConsolePassword 非空时启用会话认证：未登录访问一律跳转 /login。
func (c *Client) startConsole() error {
	if c.cfg.ConsoleAddr == "-" {
		return nil
	}
	base := c.cfg.ConsoleAddr
	if base == "" {
		base = "127.0.0.1:8900"
	}
	hostPart, portPart, _ := net.SplitHostPort(base)
	port, _ := strconv.Atoi(portPart)
	var ln net.Listener
	var lastErr error
	for i := 0; i < 11; i++ {
		p := port + i
		ln, lastErr = net.Listen("tcp", net.JoinHostPort(hostPart, strconv.Itoa(p)))
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("lanet: 控制台监听失败（%s 起 11 个端口均被占用）: %w", base, lastErr)
	}

	// 设置了访问密码：生成随机会话令牌（内存保存，重启后需重新登录）。
	if c.cfg.ConsolePassword != "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("lanet: 生成控制台会话令牌失败: %w", err)
		}
		c.sessionToken = hex.EncodeToString(buf)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", c.apiState)
	mux.HandleFunc("PUT /api/firewall", c.apiSetFirewall)
	mux.HandleFunc("PUT /api/forwards", c.apiSetForwards)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if logo, err := consoleFS.ReadFile("console/logo.png"); err == nil {
			_, _ = w.Write(logo)
		}
	})
	if c.sessionToken != "" {
		mux.HandleFunc("GET /login", c.apiLoginPage)
		mux.HandleFunc("POST /login", c.apiLoginSubmit)
		mux.HandleFunc("GET /logout", c.apiLogout)
	}
	for pattern, handler := range c.cfg.ConsoleExtra {
		mux.Handle(pattern, handler)
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		page, _ := consoleFS.ReadFile("console/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})

	var handler http.Handler = mux
	if c.sessionToken != "" {
		handler = c.authMiddleware(mux)
	}
	c.consoleSrv = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := c.consoleSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			c.logf("控制台退出: %v", err)
		}
	}()

	// 记录实际访问地址（端口被占用时 listener 会向后回退）。
	host := hostPart
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c.consoleURL = fmt.Sprintf("http://%s", net.JoinHostPort(host, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)))
	c.logf("Web 控制台已启动：%s（%s）", c.consoleURL,
		map[bool]string{true: "已启用访问密码", false: "无密码"}[c.sessionToken != ""])
	return nil
}

const sessionCookieName = "lanet_console_session"

// authMiddleware 会话认证：除登录页/图标外全部要求有效会话 Cookie。
func (c *Client) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/logout", "/favicon.ico":
			next.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(sessionCookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(c.sessionToken)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录或会话已过期"})
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiLoginPage 登录页（移动端可用的简洁表单）。
func (c *Client) apiLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginPageHTML))
}

// apiLoginSubmit 校验密码并签发会话 Cookie。
func (c *Client) apiLoginSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	pw := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(pw), []byte(c.cfg.ConsolePassword)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Replace(loginPageHTML, `class="msg"`, `class="msg" style="color:#d5484a">密码错误`, 1)))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: c.sessionToken, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// apiLogout 清除会话。
func (c *Client) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

const loginPageHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lanet 控制台登录</title>
<style>
 body { font:14px/1.6 system-ui,"Segoe UI","Microsoft YaHei",sans-serif; background:#f5f6f8;
        display:flex; align-items:center; justify-content:center; min-height:100vh; margin:0; }
 .box { background:#fff; border:1px solid #e2e4e9; border-radius:10px; padding:28px; width:min(340px,90vw); }
 h1 { font-size:17px; margin:0 0 4px; } .sub { color:#6b7280; font-size:12px; margin-bottom:16px; }
 input { width:100%; box-sizing:border-box; padding:9px 10px; border:1px solid #e2e4e9;
         border-radius:6px; font-size:14px; }
 button { width:100%; margin-top:12px; padding:9px; border:0; border-radius:6px;
          background:#2f6fed; color:#fff; font-size:14px; cursor:pointer; }
 .msg { font-size:12px; min-height:16px; margin-top:10px; color:#6b7280; }
</style></head><body><div class="box">
<h1>Lanet 节点控制台</h1>
<div class="sub">本控制台已启用访问密码，请登录</div>
<form method="POST" action="/login">
  <input type="password" name="password" placeholder="访问密码" autofocus autocomplete="current-password">
  <button type="submit">登录</button>
</form>
<div class="msg"></div>
</div></body></html>`

// ConsoleURL 内置 Web 控制台的实际访问地址（含端口回退后的真实端口）；
// 控制台关闭（ConsoleAddr="-"）时返回空串。
func (c *Client) ConsoleURL() string { return c.consoleURL }

// apiState 全量状态：节点信息 + 成员表 + 防火墙 + 转发映射。
func (c *Client) apiState(w http.ResponseWriter, r *http.Request) {
	mode, rules := c.fw.Snapshot()
	c.fwMu.RLock()
	forwards := append([]LANForward(nil), c.forwards...)
	c.fwMu.RUnlock()
	type memberView struct {
		PeerID    string `json:"peer_id"`
		Name      string `json:"name"`
		VirtualIP string `json:"virtual_ip"`
		Hostname  string `json:"hostname"` // 虚拟地址（如 yunloli.lanet），可能为空
		Online    bool   `json:"online"`
		Path      string `json:"path"`
		FirstSeen int64  `json:"first_seen,omitempty"` // Unix 秒，0 = 未知（仅排序用，页面不展示）
		LastSeen  int64  `json:"last_seen"`
		Version   string `json:"version,omitempty"`  // 程序版本号（info 协议交换；旧节点为空）
		Platform  string `json:"platform,omitempty"` // 运行平台（如 windows/amd64）
	}
	members := []memberView{}
	for _, m := range c.NetMap().Members {
		online := false
		if pid, err := peer.Decode(m.PeerID); err == nil {
			online = c.node.Network().Connectedness(pid) == network.Connected
		}
		mv := memberView{
			PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP,
			Hostname: m.Hostname,
			Online:   online,
			Path:     c.LastPathUsed(m.PeerID),
			Version:  m.Version, Platform: m.Platform,
		}
		if !m.FirstSeen.IsZero() {
			mv.FirstSeen = m.FirstSeen.Unix()
		}
		if !m.LastSeen.IsZero() {
			mv.LastSeen = m.LastSeen.Unix()
		}
		members = append(members, mv)
	}
	// 固定排序：按发现时间倒序（最后发现的成员固定在最上面），与在线状态、
	// 活跃时间无关——列表顺序在成员增减之外永不跳动。
	// FirstSeen 为零（控制面 NetMap 无此概念）时按虚拟 IP 兜底，保证稳定。
	sort.Slice(members, func(i, j int) bool {
		if members[i].FirstSeen != members[j].FirstSeen && members[i].FirstSeen != 0 && members[j].FirstSeen != 0 {
			return members[i].FirstSeen > members[j].FirstSeen
		}
		return members[i].VirtualIP < members[j].VirtualIP
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"info":     c.Info(),
		"members":  members,
		"mode":     mode,
		"rules":    rules,
		"forwards": forwards,
	})
}

// apiSetFirewall 热更新防火墙（模式 + 规则）。
func (c *Client) apiSetFirewall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode  firewall.Mode   `json:"mode"`
		Rules []firewall.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	c.fw.Set(req.Mode, req.Rules)
	c.saveState()
	mode, rules := c.fw.Snapshot()
	c.logf("防火墙已更新：mode=%s rules=%d", mode, len(rules))
	writeJSON(w, http.StatusOK, map[string]any{"mode": mode, "rules": rules})
}

// apiSetForwards 热更新局域网转发映射表。
func (c *Client) apiSetForwards(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Forwards []LANForward `json:"forwards"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	for _, f := range req.Forwards {
		if f.Listen <= 0 || f.Listen > 65535 || f.Target == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("映射非法: listen=%d target=%q", f.Listen, f.Target)})
			return
		}
	}
	c.fwMu.Lock()
	c.forwards = req.Forwards
	c.fwMu.Unlock()
	c.saveState()
	c.logf("转发映射已更新：%d 条", len(req.Forwards))
	writeJSON(w, http.StatusOK, map[string]any{"forwards": req.Forwards})
}

// loadState 从 StateFile 恢复防火墙与转发映射。
func (c *Client) loadState() {
	if c.statePath == "" {
		return
	}
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		return // 文件不存在 = 首次启动，用 Config 初始值
	}
	var st consoleState
	if err = json.Unmarshal(data, &st); err != nil {
		c.logf("控制台状态文件解析失败（忽略）: %v", err)
		return
	}
	c.fw.Set(st.Mode, st.Rules)
	c.forwards = st.Forwards
	c.logf("已从 %s 恢复控制台状态（防火墙 %s，映射 %d 条）", filepath.Base(c.statePath), st.Mode, len(st.Forwards))
}

// saveState 持久化当前状态（临时文件 + 原子替换）。
func (c *Client) saveState() {
	if c.statePath == "" {
		return
	}
	mode, rules := c.fw.Snapshot()
	c.fwMu.RLock()
	st := consoleState{Mode: mode, Rules: rules, Forwards: append([]LANForward(nil), c.forwards...)}
	c.fwMu.RUnlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := c.statePath + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		c.logf("控制台状态保存失败: %v", err)
		return
	}
	_ = os.Rename(tmp, c.statePath)
}

// writeJSON 统一 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Firewall 防火墙快照（编程接口，与控制台等价）。
func (c *Client) Firewall() (firewall.Mode, []firewall.Rule) { return c.fw.Snapshot() }

// SetFirewall 热更新防火墙（编程接口）。
func (c *Client) SetFirewall(mode firewall.Mode, rules []firewall.Rule) {
	c.fw.Set(mode, rules)
	c.saveState()
}

// SetLANForwards 热更新局域网转发映射表（编程接口）。
func (c *Client) SetLANForwards(fs []LANForward) {
	c.fwMu.Lock()
	c.forwards = append([]LANForward(nil), fs...)
	c.fwMu.Unlock()
	c.saveState()
}

// LANForwards 当前转发映射表。
func (c *Client) LANForwards() []LANForward {
	c.fwMu.RLock()
	defer c.fwMu.RUnlock()
	return append([]LANForward(nil), c.forwards...)
}
