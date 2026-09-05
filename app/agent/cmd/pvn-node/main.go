// pvn-node（发行名 lanet）长驻的 Standalone（无服务器）节点：
//
//   - 客户端与服务端一体：DHT + mDNS 自动发现，节点即服务端，无需部署任何东西；
//   - 双击/零参数即可启动：读取 exe 同目录 lanet.json 配置文件（不存在则自动生成），
//     之后全部在 Web 控制台（默认 http://127.0.0.1:8900）配置，保存后重启生效；
//   - 对外提供 /lanet/echo/1.0.0 回显应用流，并周期探测成员连通性；
//   - 内置 Web 控制台（成员 / 防火墙 / 端口转发 / 节点配置）。
//
// 用法示例：
//
//	lanet                            # 双击或直接运行：按 lanet.json 配置入网
//	lanet -name edge-a -key net-x -bootstrap public -console :8900
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/libp2p/go-libp2p/core/network"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// echoProto 节点间探测回显协议。
const echoProto = libprotocol.ID("/lanet/echo/1.0.0")

// version 由 CI 经 -ldflags "-X main.version=<VERSION 文件内容>" 注入。
var version = "dev"

func main() {
	exeDir := exeDir()
	var (
		config = flag.String("config", envOr("LANET_CONFIG", filepath.Join(exeDir, "lanet.json")),
			"配置文件路径（默认 exe 同目录 lanet.json，双击启动即靠它）")
		name = flag.String("name", envOr("LANET_NAME", ""),
			"节点名称（成员表中的虚拟域名）；不传则读配置文件，再退回主机名")
		key = flag.String("key", envOr("LANET_NETWORK_KEY", "@@unset@@"),
			"网络密钥（留空 = 公共网络）；不传则读配置文件")
		bootstrap = flag.String("bootstrap", envOr("LANET_BOOTSTRAP", ""),
			"引导节点：public = 公共 DHT / none = 仅 mDNS / 成员 multiaddr；不传则读配置文件")
		identity = flag.String("identity", envOr("LANET_IDENTITY", ""),
			"身份密钥文件路径；不传则读配置文件，再退回默认（Windows: exe 同目录 node.key）")
		console = flag.String("console", envOr("LANET_CONSOLE", ""),
			"控制台监听地址；不传则读配置文件（默认 127.0.0.1:8900 仅本机，0.0.0.0:8900 = 允许远程）")
		consolePW = flag.String("console-password", envOr("LANET_CONSOLE_PASSWORD", ""),
			"控制台访问密码（远程访问时务必设置）；不传则读配置文件")
		fw = flag.String("fw", envOr("LANET_FW", ""),
			"防火墙模式：deny-all / allow-list / allow-all；不传则读配置文件")
		listen = flag.String("listen", envOr("LANET_LISTEN", ""),
			"覆盖监听地址（逗号分隔）；默认 tcp/ws/quic 全部随机端口")
		noPublic = flag.Bool("no-public-dht",
			envOr("LANET_NO_PUBLIC_DHT", "") == "1" || strings.EqualFold(envOr("LANET_NO_PUBLIC_DHT", ""), "true"),
			"私有网络下关闭公共 DHT 兜底（纯私有种子 + mDNS）")
		probe = flag.Duration("probe", 0, "成员探测间隔；不传则读配置文件（默认 20s）")
	)
	flag.Parse()

	// ---- 日志：stderr + exe 同目录 lanet.log 双写（windowsgui 无黑框时靠文件看日志）----
	// 注意：不能用 io.MultiWriter(os.Stderr, lf)——windowsgui 下 stderr 是无效句柄，
	// 写入报错后 MultiWriter 提前返回，文件永远写不进。这里逐个写、忽略单点错误。
	if lf, err := os.OpenFile(filepath.Join(filepath.Dir(*config), "lanet.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{os.Stderr, lf}})
	}

	// ---- 配置文件：不存在则生成默认模板（双击启动的场景），存在则加载 ----
	nc, cfgCreated := loadNodeConfig(*config, exeDir)
	if cfgCreated {
		log.Printf("[node] 已生成默认配置文件 %s（可在 Web 控制台修改，重启生效）", *config)
	}

	// ---- 参数解析优先级：显式命令行 > 环境变量 > 配置文件 > 内置默认 ----
	flagSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })

	effName := nc.Name
	if *name != "" {
		effName = *name
	}
	if effName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			effName = h
		} else {
			effName = "node"
		}
	}
	effKey := nc.NetworkKey // 注意：空串是合法值（公共网络），不能用 "非空才覆盖" 逻辑
	if *key != "@@unset@@" {
		effKey = *key
	}
	effBootstrap := firstNonEmpty(*bootstrap, nc.Bootstrap, "public")
	effIdentity := firstNonEmpty(*identity, nc.Identity, defaultIdentityPath(exeDir))
	effConsole := firstNonEmpty(*console, nc.Console, "127.0.0.1:8900")
	effConsolePW := firstNonEmpty(*consolePW, nc.ConsolePassword)
	effFW := firstNonEmpty(*fw, nc.Firewall, "allow-all")
	effListen := firstNonEmpty(*listen, nc.Listen)
	effNoPublic := *noPublic || nc.NoPublicDHT
	effProbe := *probe
	if effProbe <= 0 {
		if nc.ProbeSec > 0 {
			effProbe = time.Duration(nc.ProbeSec) * time.Second
		} else {
			effProbe = 20 * time.Second
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[node] 启动 name=%s key=%q fw=%s console=%s noPublicDHT=%v version=%s config=%s",
		effName, effKey, effFW, effConsole, effNoPublic, version, *config)

	switch strings.TrimSpace(effBootstrap) {
	case "", "none":
		// 仅 mDNS 局域网发现（完全不接入公共 DHT）。
		effNoPublic = true
	case "public":
		nc.bootstrapAddrs = []string{serverless.DefaultBootstrap}
	default:
		for _, a := range strings.Split(effBootstrap, ",") {
			if a = strings.TrimSpace(a); a != "" {
				nc.bootstrapAddrs = append(nc.bootstrapAddrs, a)
			}
		}
	}
	cfg := lanet.Config{
		Name:             effName,
		NetworkKey:       effKey,
		Standalone:       true,
		Bootstrap:        nc.bootstrapAddrs,
		DisablePublicDHT: effNoPublic,
		IdentityFile:     effIdentity,
		ConsoleAddr:      effConsole,
		ConsolePassword:  effConsolePW,
		StateFile:        filepath.Join(filepath.Dir(*config), "state.json"),
		ConsoleExtra:     nodeConfigRoutes(*config),
	}
	switch effFW {
	case "allow-list":
		cfg.FirewallMode = lanet.FirewallModeAllowList
		// 探测与 echo 依赖应用流入向：放行全部来源的全部协议（测试语义）。
		cfg.FirewallRules = []lanet.FirewallRule{{Source: "*", Proto: lanet.FirewallProtoAny}}
	case "deny-all":
		cfg.FirewallMode = lanet.FirewallModeDenyAll
	default:
		cfg.FirewallMode = lanet.FirewallModeAllowAll
	}
	if effListen != "" {
		cfg.ListenAddrs = strings.Split(effListen, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	node, err := lanet.New(ctx, cfg)
	if err != nil {
		log.Fatalf("[node] 入网失败: %v", err)
	}
	defer node.Close()
	info := node.Info()
	log.Printf("[node] 已入网 name=%s peerID=%s virtualIP=%s network=%s",
		effName, info.PeerID, info.VirtualIP, networkLabel(effKey))

	// ---- 托盘 + 自动打开控制台（Windows 图形界面模式）----
	if consoleURL := node.ConsoleURL(); consoleURL != "" && runtime.GOOS == "windows" {
		startTray(func() string { return consoleURL }, cancel)
		openBrowser(consoleURL)
	}

	// 回显服务：收到什么回什么（供其他节点探测）。
	node.Host().SetStreamHandler(echoProto, func(s network.Stream) {
		defer s.Close()
		buf := make([]byte, 4096)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				if _, werr := s.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	// 同时保留 SDK Tunnel 协议 echo（兼容既有验证工具）。
	node.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = stream.Write([]byte("echo:" + stream.Protocol()))
	})

	go node.Run(ctx)

	// 周期探测：向成员表内所有其他成员发起 echo 往返。
	lastMembers := ""
	ticker := time.NewTicker(effProbe)
	defer ticker.Stop()
	first := time.After(5 * time.Second)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[node] 收到退出信号")
			return
		case <-first:
		case <-ticker.C:
		}
		members := node.NetMap().Members
		sig := membersSignature(members)
		if sig != lastMembers {
			log.Printf("[node] 成员表更新（%d 人）: %s", len(members), sig)
			lastMembers = sig
		}
		self := node.Info().VirtualIP
		for _, m := range members {
			if m.VirtualIP == self || m.VirtualIP == "" {
				continue
			}
			probeOnce(ctx, node, m.Name, m.VirtualIP)
		}
	}
}

// probeOnce 对单个成员做一次 echo 往返探测。
func probeOnce(ctx context.Context, node *lanet.Client, name, virtualIP string) {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, viaRelay, err := node.DialProtocol(pctx, virtualIP, string(echoProto))
	if err != nil {
		log.Printf("[probe] FAIL %s(%s): %v", name, virtualIP, err)
		return
	}
	defer stream.Close()
	payload := fmt.Sprintf("probe-%d", start.UnixMilli())
	if _, err = stream.Write([]byte(payload)); err != nil {
		log.Printf("[probe] FAIL %s(%s): write: %v", name, virtualIP, err)
		return
	}
	_ = stream.CloseWrite()
	buf := make([]byte, 4096)
	n, err := readAll(stream, buf)
	via := "direct"
	if viaRelay {
		via = "relay"
	}
	if err != nil || string(buf[:n]) != payload {
		log.Printf("[probe] FAIL %s(%s): 回显不匹配 (n=%d err=%v)", name, virtualIP, n, err)
		return
	}
	log.Printf("[probe] OK %s(%s) via=%s rtt=%s", name, virtualIP, via, time.Since(start).Round(time.Millisecond))
}

// readAll 读完直到 EOF 或缓冲满。
func readAll(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, nil // EOF 语义视为正常结束
		}
	}
	return total, nil
}

// membersSignature 成员表摘要（稳定顺序）。
func membersSignature(members []netmapclient.Member) string {
	type row struct{ ip, name string }
	rows := make([]row, 0, len(members))
	for _, m := range members {
		rows = append(rows, row{m.VirtualIP, m.Name})
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].ip < rows[i].ip {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = r.name + "@" + r.ip
	}
	return strings.Join(parts, ", ")
}

func networkLabel(key string) string {
	if key == "" {
		return "public(公共网络)"
	}
	return "private(" + key + ")"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// tolerantWriter 逐个写出、忽略单个目标错误（io.MultiWriter 遇错即返回，
// windowsgui 下 stderr 无效会把整个日志写挂）。
type tolerantWriter struct{ ws []io.Writer }

func (w tolerantWriter) Write(p []byte) (int, error) {
	for _, wr := range w.ws {
		_, _ = wr.Write(p)
	}
	return len(p), nil
}

// firstNonEmpty 返回第一个非空值（全部为空则返回空串）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// exeDir 可执行文件所在目录（双击启动时配置/身份/状态文件都落在这里）。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// defaultIdentityPath 身份密钥默认路径：Windows 放 exe 同目录（好找好备份），
// 其他平台维持容器约定 /data/node.key。
func defaultIdentityPath(exeDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(exeDir, "node.key")
	}
	return "/data/node.key"
}

// nodeConfig lanet.json 配置文件结构（字段与命令行参数一一对应）。
// 双击/零参数启动时全靠它；Web 控制台「节点配置」编辑的就是这个文件，
// 保存后重启程序生效（防火墙与转发映射在控制台里是热生效的，不在此列）。
type nodeConfig struct {
	Name            string `json:"name"`
	NetworkKey      string `json:"network_key"`
	Bootstrap       string `json:"bootstrap"`
	Identity        string `json:"identity"`
	Console         string `json:"console"`
	ConsolePassword string `json:"console_password,omitempty"`
	Firewall        string `json:"firewall"`
	Listen          string `json:"listen"`
	NoPublicDHT     bool   `json:"no_public_dht"`
	ProbeSec        int    `json:"probe_seconds"`

	bootstrapAddrs []string `json:"-"` // 运行时由 Bootstrap 解析而来
}

// loadNodeConfig 读取配置文件；不存在或损坏时返回默认配置并尽力生成模板文件。
// 返回值 created 表示本次是否新生成了模板。
func loadNodeConfig(path, exeDir string) (*nodeConfig, bool) {
	if data, err := os.ReadFile(path); err == nil {
		var nc nodeConfig
		if json.Unmarshal(data, &nc) == nil {
			return &nc, false
		}
		log.Printf("[node] 配置文件解析失败（按默认值运行，可删除该文件重新生成）: %s", path)
	}
	nc := defaultNodeConfig(exeDir)
	if err := nc.save(path); err != nil {
		log.Printf("[node] 默认配置文件生成失败（不影响启动）: %v", err)
	}
	return nc, true
}

// defaultNodeConfig 开箱即用默认值：主机名作为节点名、公共 DHT 引导、控制台全开。
func defaultNodeConfig(exeDir string) *nodeConfig {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "node"
	}
	return &nodeConfig{
		Name:       name,
		NetworkKey: "",
		Bootstrap:  "public",
		Identity:   defaultIdentityPath(exeDir),
		Console:    "127.0.0.1:8900",
		Firewall:   "allow-all",
		ProbeSec:   20,
	}
}

// save 原子写入配置文件。
func (nc *nodeConfig) save(path string) error {
	data, err := json.MarshalIndent(nc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nodeConfigRoutes 节点配置 API：GET 读取 / PUT 保存（写回 lanet.json，重启生效）。
func nodeConfigRoutes(path string) map[string]http.HandlerFunc {
	read := func() nodeConfig {
		var nc nodeConfig
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &nc)
		}
		return nc
	}
	return map[string]http.HandlerFunc{
		"GET /api/node-config": func(w http.ResponseWriter, r *http.Request) {
			nc := read()
			writeJSONLocal(w, http.StatusOK, map[string]any{
				"config_path":   path,
				"name":          nc.Name,
				"network_key":   nc.NetworkKey,
				"bootstrap":     nc.Bootstrap,
				"identity":      nc.Identity,
				"console":       nc.Console,
				"has_password":  nc.ConsolePassword != "",
				"firewall":      nc.Firewall,
				"listen":        nc.Listen,
				"no_public_dht": nc.NoPublicDHT,
				"probe_seconds": nc.ProbeSec,
			})
		},
		"PUT /api/node-config": func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				nodeConfig
				ClearPassword bool `json:"clear_password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
				return
			}
			switch req.Firewall {
			case "deny-all", "allow-list", "allow-all":
			default:
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "firewall 必须是 deny-all / allow-list / allow-all"})
				return
			}
			if req.Console == "" || req.Name == "" {
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "name 与 console 不能为空"})
				return
			}
			if req.ProbeSec < 0 {
				req.ProbeSec = 20
			}
			prev := read()
			req.Identity = prev.Identity // 身份文件路径只读，防止控制台误改导致身份丢失
			// 密码语义：clear_password=true → 清除；传了非空密码 → 覆盖；否则保持不变。
			// GET 不回传密码明文，所以 req.ConsolePassword 为空时不能当作"删除"。
			switch {
			case req.ClearPassword:
				req.ConsolePassword = ""
			case req.ConsolePassword == "":
				req.ConsolePassword = prev.ConsolePassword
			}
			if err := req.save(path); err != nil {
				writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": "保存失败: " + err.Error()})
				return
			}
			log.Printf("[node] 节点配置已保存到 %s（重启程序后生效）", path)
			writeJSONLocal(w, http.StatusOK, map[string]any{"saved": true, "restart_required": true})
		},
	}
}

// writeJSONLocal 统一 JSON 响应（节点配置 API 用）。
func writeJSONLocal(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
