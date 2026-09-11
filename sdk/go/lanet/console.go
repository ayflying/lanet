package lanet

import (
	"context"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/pkg/netmapclient"
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
	mux.HandleFunc("GET /api/local-info", c.apiLocalInfo)
	mux.HandleFunc("GET /api/member-info", c.apiMemberInfo)
	mux.HandleFunc("PUT /api/firewall", c.apiSetFirewall)
	mux.HandleFunc("PUT /api/forwards", c.apiSetForwards)
	mux.HandleFunc("POST /api/public-dht", c.apiSetPublicDHT)
	mux.HandleFunc("POST /api/connect-seed", c.apiConnectSeed)
	// 统一连接入口（节点 ID 或完整地址，自动识别）+ 待审批 / 地址簿管理。
	mux.HandleFunc("POST /api/connect-peer", c.apiConnectPeer)
	mux.HandleFunc("GET /api/pending", c.apiPending)
	mux.HandleFunc("POST /api/pending", c.apiResolvePending)
	mux.HandleFunc("GET /api/peers", c.apiPeers)
	mux.HandleFunc("POST /api/peers", c.apiRemovePeer)
	mux.HandleFunc("GET /api/nearby", c.apiNearby)
	mux.HandleFunc("POST /api/nearby", c.apiReconnectPeer)
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
		// 页面随二进制内置，每次升级内容都会变：禁止缓存，避免升级后浏览器
		// 仍用旧页面导致「新功能看不到 / 字段对不上」的假故障。
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
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
	// 空切片必须序列化为 [] 而不是 null：前端读 xxx.length 不做判空，
	// 一旦返回 null 整个 loadState 中断，后续渲染（连接卡片等）全部失效。
	if forwards == nil {
		forwards = []LANForward{}
	}
	if rules == nil {
		rules = []FirewallRule{}
	}
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
	// 公共 DHT 临时引导状态 + 连接种子（Standalone 专用；常规模式为零值）。
	pubState, hasPub := c.PublicDHTStatus()
	// 待审批连接申请 + 地址簿规模（连接审批启用时有意义）。
	pending := []map[string]any{}
	for _, p := range c.PendingList() {
		item := map[string]any{"peer_id": p.PeerID, "name": p.Name}
		if !p.RequestedAt.IsZero() {
			item["created_at"] = p.RequestedAt.Unix()
		}
		pending = append(pending, item)
	}
	// 附近节点：同网络可发现但未成为好友的节点（含被删除过的好友），
	// 控制台「附近」卡片展示 + 申请连接入口。
	nearby := []map[string]any{}
	for _, n := range c.NearbyList() {
		item := map[string]any{"peer_id": n.PeerID, "name": n.Name, "source": n.Source}
		if !n.LastSeen.IsZero() {
			item["last_seen"] = n.LastSeen.Unix()
		}
		nearby = append(nearby, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"info":                  c.Info(),
		"members":               members,
		"mode":                  mode,
		"rules":                 rules,
		"forwards":              forwards,
		"public_dht":            pubState,
		"has_public_dht_config": hasPub,
		"seed_addrs":            c.SeedAddrs(),
		"pending":               pending,
		"pending_count":         len(pending),
		"nearby":                nearby,
		"trusted_count":         len(c.TrustedPeers()),
		"auto_accept":           c.cfg.AutoAccept,
		"require_approval":      c.requireApproval(),
		"db_path":               c.PeersDBPath(),
	})
}

// apiConnectPeer 统一连接入口：按节点 ID 或完整 multiaddr 连接同网络节点。
// 请求体 {"address": "12D3Koo…" | "/ip4/…/p2p/12D3Koo…"}。
// 若对方尚未审批本机，返回 pending=true（已提交申请，等待对方同意）。
func (c *Client) apiConnectPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := c.ConnectPeer(ctx, req.Address)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// apiPending 待审批连接申请列表。
func (c *Client) apiPending(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, p := range c.PendingList() {
		item := map[string]any{"peer_id": p.PeerID, "name": p.Name, "addrs": p.Addrs}
		if !p.RequestedAt.IsZero() {
			item["created_at"] = p.RequestedAt.Unix()
		}
		list = append(list, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending":       list,
		"auto_accept":   c.cfg.AutoAccept,
		"db_path":       c.PeersDBPath(),
		"trusted_count": len(c.TrustedPeers()),
	})
}

// apiResolvePending 同意或拒绝某个节点的连接申请。
// 请求体 {"peer_id": "12D3Koo…", "approve": true}。
func (c *Client) apiResolvePending(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID  string `json:"peer_id"`
		Approve *bool  `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PeerID == "" || req.Approve == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体需包含 peer_id 与 approve"})
		return
	}
	var err error
	if *req.Approve {
		err = c.ApprovePeer(req.PeerID)
	} else {
		err = c.RejectPeer(req.PeerID)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approved": *req.Approve, "peer_id": req.PeerID})
}

// apiPeers 地址簿（已信任节点）列表。
func (c *Client) apiPeers(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, p := range c.TrustedPeers() {
		item := map[string]any{"peer_id": p.PeerID, "name": p.Name}
		if !p.LastSeen.IsZero() {
			item["last_seen"] = p.LastSeen.Unix()
		}
		if p.LastIP != "" {
			item["last_ip"] = p.LastIP
		}
		list = append(list, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": list, "db_path": c.PeersDBPath()})
}

// apiRemovePeer 从地址簿移除节点（撤销信任）。
// 请求体 {"peer_id": "12D3Koo…"}。
func (c *Client) apiRemovePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PeerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体需包含 peer_id"})
		return
	}
	if err := c.RemovePeer(req.PeerID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer_id": req.PeerID})
}

// apiNearby 附近节点列表：同网络密钥内可发现但尚未成为好友的节点
// （含被删除过的好友）。前端据此展示「申请连接」按钮。
func (c *Client) apiNearby(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, n := range c.NearbyList() {
		item := map[string]any{"peer_id": n.PeerID, "name": n.Name, "addrs": n.Addrs, "source": n.Source}
		if !n.LastSeen.IsZero() {
			item["last_seen"] = n.LastSeen.Unix()
		}
		list = append(list, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nearby": list})
}

// apiReconnectPeer 向附近节点重新申请连接（好友被删除后的恢复入口）。
// 请求体 {"peer_id": "12D3Koo…"}。结果语义同 /api/connect-peer：
// connected / pending（等待对方同意）/ error。
func (c *Client) apiReconnectPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PeerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体需包含 peer_id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := c.ReconnectPeer(ctx, req.PeerID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// apiConnectSeed 立即按连接种子直连一个同群节点（运行时可调，无需重启）。
// 请求体 {"seed": "<multiaddr>"}；成功返回 peer_id 与对应虚拟 IP。
func (c *Client) apiConnectSeed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seed string `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
		return
	}
	peerID, err := c.ConnectSeed(req.Seed)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 回查成员的虚拟 IP，便于前端直接展示「已连接到 xxx」。
	vip := ""
	for _, m := range c.NetMap().Members {
		if m.PeerID == peerID {
			vip = m.VirtualIP
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer_id": peerID, "virtual_ip": vip})
}

// apiSetPublicDHT 运行时开关公共 DHT 临时引导（控制台开关，立即生效）。
// 不改动配置文件：下次启动是否自动开启由节点配置的 enable_public_dht 决定。
func (c *Client) apiSetPublicDHT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enable *bool `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enable == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体需包含 enable 布尔值"})
		return
	}
	if err := c.SetPublicDHT(*req.Enable); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	st, _ := c.PublicDHTStatus()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "public_dht": st})
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
	// 用节点根 context（不是请求作用域 ctx）：监听 goroutine 要与节点同生命周期。
	c.syncListenForwards(c.rootCtx)
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
	if c.forwards == nil {
		c.forwards = []LANForward{}
	}
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

// localIPView 本机网卡 IP 信息（详情弹框用）。
type localIPView struct {
	Iface string `json:"iface"`         // 网卡名
	IP    string `json:"ip"`            // IP 地址（含掩码位数）
	Type  string `json:"type"`          // v4 / v6
	MAC   string `json:"mac,omitempty"` // 物理网卡附带的 MAC
}

// apiLocalInfo 本机基础信息：主机名、系统/平台、版本、虚拟身份、全部网卡 IP。
// 用于在控制台「详情」弹框里识别当前设备是哪台机器。
func (c *Client) apiLocalInfo(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()

	// 枚举所有网卡的活动 IP（排除回环与链路本地地址，详见 collectLocalIPs）。
	ips := []localIPView{}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagLoopback != 0 {
				continue // 回环网卡整块跳过
			}
			addrs, err := ifc.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip4 := ipn.IP.To4()
				if ip4 == nil {
					if ipn.IP.IsLinkLocalUnicast() {
						continue
					}
				} else if ip4.IsLinkLocalUnicast() {
					continue
				}
				mask, _ := ipn.Mask.Size()
				ips = append(ips, localIPView{
					Iface: ifc.Name,
					IP:    fmt.Sprintf("%s/%d", ipn.IP, mask),
					Type:  map[bool]string{true: "v4", false: "v6"}[ip4 != nil],
					MAC:   ifc.HardwareAddr.String(),
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hostname":     hostname,
		"os":           c.cfg.OS,
		"platform":     c.platform(),
		"go_version":   runtime.Version(),
		"version":      c.cfg.Version,
		"node_name":    c.cfg.Name,
		"virtual_ip":   c.myIP,
		"virtual_host": c.selfHostname(),
		"peer_id":      c.peerID,
		"ips":          ips,
	})
}

// apiMemberInfo 指定成员的设备信息：主机名、系统/平台、版本、虚拟身份、
// 本机网卡 IP（经 info 协议从对端交换所得）。0.5.15 起；旧版本对端
// 未上报这些字段时返回空值，前端回退显示已知信息。
func (c *Client) apiMemberInfo(w http.ResponseWriter, r *http.Request) {
	peerID := r.URL.Query().Get("peer_id")
	if peerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 peer_id"})
		return
	}
	var found *netmapclient.Member
	for i := range c.NetMap().Members {
		if c.NetMap().Members[i].PeerID == peerID {
			found = &c.NetMap().Members[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "成员不存在或已下线"})
		return
	}
	m := *found
	writeJSON(w, http.StatusOK, map[string]any{
		"peer_id":      m.PeerID,
		"node_name":    m.Name,
		"virtual_ip":   m.VirtualIP,
		"virtual_host": m.Hostname,
		"os":           m.OS,
		"platform":     m.Platform,
		"version":      m.Version,
		"os_hostname":  m.OSHostname,
		"local_ips":    m.LocalIPs,
	})
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
	c.syncListenForwards(c.rootCtx)
}

// LANForwards 当前转发映射表。
func (c *Client) LANForwards() []LANForward {
	c.fwMu.RLock()
	defer c.fwMu.RUnlock()
	return append([]LANForward(nil), c.forwards...)
}
