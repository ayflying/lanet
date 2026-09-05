package lanet

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
)

//go:embed console/index.html
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", c.apiState)
	mux.HandleFunc("PUT /api/firewall", c.apiSetFirewall)
	mux.HandleFunc("PUT /api/forwards", c.apiSetForwards)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		page, _ := consoleFS.ReadFile("console/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})

	c.consoleSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := c.consoleSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			c.logf("控制台退出: %v", err)
		}
	}()
	c.logf("Web 控制台已启动：http://%s", ln.Addr())
	return nil
}

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
		Path      string `json:"path"`
	}
	members := []memberView{}
	for _, m := range c.NetMap().Members {
		members = append(members, memberView{
			PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP,
			Path: c.LastPathUsed(m.PeerID),
		})
	}
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
