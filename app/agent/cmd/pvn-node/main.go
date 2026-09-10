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
	"strconv"
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
			"网络密钥：留空 = 按本机身份派生的专属默认网络（开箱即用但默认不与他人同网）；"+
				"填相同值才能与对方互通；不传则读配置文件")
		bootstrap = flag.String("bootstrap", envOr("LANET_BOOTSTRAP", ""),
			"引导节点：public = 公共 DHT / none = 仅 mDNS / 成员 multiaddr；不传则读配置文件")
		console = flag.String("console", envOr("LANET_CONSOLE", ""),
			"控制台监听地址；不传则读配置文件（默认 127.0.0.1:8900 仅本机，0.0.0.0:8900 = 允许远程）")
		consolePW = flag.String("console-password", envOr("LANET_CONSOLE_PASSWORD", ""),
			"控制台访问密码（远程访问时务必设置）；不传则读配置文件")
		fw = flag.String("fw", envOr("LANET_FW", ""),
			"防火墙模式：deny-all / allow-list / allow-all；不传则读配置文件")
		listen = flag.String("listen", envOr("LANET_LISTEN", ""),
			"覆盖监听地址（逗号分隔）；默认 tcp/ws/quic 全部随机端口")
		tun = flag.String("tun", "@@unset@@",
			"虚拟网卡 TUN（IP 层互通：ping/任意端口直达虚拟 IP）；true/false，缺省读配置文件（默认 true）")
		// 三态哨兵：未传 = 读配置文件；显式传 true/false = 覆盖配置文件。
		// 不能用 flag.Bool——它的零值 false 无法区分「用户显式传了 false」
		// 与「用户根本没传」，会让配置文件里的 true 被无声忽略。
		publicDHT = flag.String("public-dht", envOr("LANET_PUBLIC_DHT", "@@unset@@"),
			"启用公共 DHT 临时引导（true/false）；不传则读配置文件（默认关闭：省流量，跨网冷启动需成员引导种子）")
		publicDHTMin = flag.Int("public-dht-minutes", atoiOr(envOr("LANET_PUBLIC_DHT_MINUTES", ""), 0),
			"公共 DHT 临时引导最长运行分钟数（默认 10；连上同群成员立即退出，超时未连上也退出）")
		autoAccept = flag.String("auto-accept", envOr("LANET_AUTO_ACCEPT", "@@unset@@"),
			"自动同意所有连接申请（true/false，默认 false）：无人值守中央服务器用；"+
				"开启后陌生节点无需人工审批即可互连（等于放弃加好友这道安全边界）")
		requireApproval = flag.String("require-approval", envOr("LANET_REQUIRE_APPROVAL", "@@unset@@"),
			"是否要求连接审批（true/false，默认 true）：开启后陌生节点需在控制台同意后才能互连")
		dbPath = flag.String("db", envOr("LANET_DB", ""),
			"地址簿数据库路径（默认 exe 同目录 lanet.db；设为 - 关闭持久化，仅内存运行）")
		probe = flag.Duration("probe", 0, "成员探测间隔；不传则读配置文件（默认 20s）")
	)
	// ---- 开机自启路径识别：注册表 Run 键带 -autorun 参数拉起 ----
	// 必须在 flag.Parse 之前剔除 -autorun（flag 包对未定义 flag 会报错退出），
	// 再用环境变量向后续逻辑传递「本次是自启」。环境变量会被子进程继承，
	// 控制台重启（spawnSelf）后标记保留，语义正确。
	args := make([]string, 0, len(os.Args))
	for _, a := range os.Args {
		if a == "-autorun" || a == "--autorun" {
			_ = os.Setenv(autorunEnv, "1")
			continue
		}
		args = append(args, a)
	}
	os.Args = args

	flag.Parse()

	// ---- 日志：stderr + exe 同目录 lanet.log 双写（windowsgui 无黑框时靠文件看日志）----
	// 注意：不能用 io.MultiWriter(os.Stderr, lf)——windowsgui 下 stderr 是无效句柄，
	// 写入报错后 MultiWriter 提前返回，文件永远写不进。这里逐个写、忽略单点错误。
	if lf, err := os.OpenFile(filepath.Join(filepath.Dir(*config), "lanet.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{os.Stderr, lf}})
	}

	// ---- 配置文件：不存在则生成默认模板（双击启动的场景），存在则加载 ----
	nc, cfgCreated := loadNodeConfig(*config)
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
	effKey, effLegacyKey := resolveNetworkKey(*key, nc.NetworkKey)
	effBootstrap := firstNonEmpty(*bootstrap, nc.Bootstrap, "public")
	// 身份文件路径固定：exe 同目录 node.key（Windows）/ /data/node.key（其他平台），
	// 不读配置、不暴露到控制台；文件不存在即新用户，SDK 自动创建新身份。
	effIdentity := defaultIdentityPath(exeDir)
	effConsole := firstNonEmpty(*console, nc.Console, "127.0.0.1:8900")
	effConsolePW := firstNonEmpty(*consolePW, nc.ConsolePassword)
	effFW := firstNonEmpty(*fw, nc.Firewall, "allow-all")
	effListen := firstNonEmpty(*listen, nc.Listen)
	// TUN 默认开启：配置文件缺省字段（nil）视为 true，命令行显式 true/false 优先。
	effTun := nc.Tun == nil || *nc.Tun
	if *tun != "@@unset@@" {
		effTun = strings.EqualFold(*tun, "true") || *tun == "1"
	}
	// 公共 DHT 开关优先级：显式命令行/环境变量 > 配置文件 > 默认关闭。
	// 环境变量与命令行共用 "@@unset@@" 哨兵：只有真正传了值才覆盖配置，
	// 避免「配置文件写 false、命令行没传」时被误判为开启（或反之）。
	effPublic := resolvePublicDHT(*publicDHT, nc.EnablePublicDHT)
	// 公共 DHT 临时引导时长（分钟）：命令行/环境变量 > 配置文件 > 默认 10。
	// 未显式开启公共 DHT 时该值仍保存，下次开启即用。
	effPublicMin := *publicDHTMin
	if effPublicMin <= 0 {
		effPublicMin = nc.PublicDHTMinutes
	}
	if effPublicMin <= 0 {
		effPublicMin = 10
	}
	// .lanet DNS 强制开启（无开关）：内置 DNS + Windows NRPT 是基础能力，
	// 关闭只会造成「同版本下有的机器能 ping .lanet 有的不能」的困惑。
	// 连接审批：默认开启（需要用户同意才互连），可用配置文件 / 环境变量关闭。
	// AutoAccept 为「自动同意」，供无人值守中央服务器使用，默认关闭。
	effRequireApproval := resolveTriBool(*requireApproval, nc.RequireApproval, true)
	effAutoAccept := resolveTriBool(*autoAccept, nc.AutoAccept, false)
	// 地址簿路径：命令行/环境变量 > 配置文件 > 默认（exe 同目录 lanet.db）。
	effDBPath := firstNonEmpty(*dbPath, nc.DBPath)
	if effDBPath == "" {
		effDBPath = filepath.Join(filepath.Dir(*config), "lanet.db")
	}
	effProbe := *probe
	if effProbe <= 0 {
		if nc.ProbeSec > 0 {
			effProbe = time.Duration(nc.ProbeSec) * time.Second
		} else {
			// 20s -> 5s：探测是 P2P 连接的保温机制，间隔过长时 libp2p 连接
			// 进入空闲回收，TUN 数据面 streamTo 每包都要重拨（经历 backoff
			// 长达数秒），表现为 ping 偶发全丢或超高延迟。
			effProbe = 5 * time.Second
		}
	}
	// P2P 自动更新强制开启：签名信任锚保证安全，无需用户决策。
	// 仅 dev 构建与容器环境自动禁用（见 StartP2PUpdate 返回值）。
	eff := nodeRuntime{
		Name:             effName,
		NetworkKey:       effKey,
		Console:          effConsole,
		Listen:           effListen,
		Firewall:         effFW,
		EnablePublicDHT:  effPublic,
		PublicDHTMinutes: effPublicMin,
		Tun:              effTun,
		AutoAccept:       effAutoAccept,
		RequireApproval:  effRequireApproval,
		DBPath:           effDBPath,
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[node] 启动 name=%s key=%q fw=%s console=%s publicDHT=%v(%dm) tun=%v version=%s config=%s",
		effName, effKey, effFW, effConsole, effPublic, effPublicMin, effTun, version, *config)
	log.Printf("[node] 连接审批：require=%v autoAccept=%v db=%s",
		effRequireApproval, effAutoAccept, effDBPath)

	switch strings.TrimSpace(effBootstrap) {
	case "", "none":
		// 无引导节点（默认）：私有 DHT + mDNS 发现，不接触任何公共设施。
	case "public":
		nc.bootstrapAddrs = []string{serverless.DefaultBootstrap}
	default:
		for _, a := range strings.Split(effBootstrap, ",") {
			if a = strings.TrimSpace(a); a != "" {
				nc.bootstrapAddrs = append(nc.bootstrapAddrs, a)
			}
		}
	}
	// 更新 / 重启 / 退出 控制台接口（与节点配置同一组扩展路由）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	extra := nodeConfigRoutes(*config, eff)
	for pattern, handler := range updateRoutes(cancel) {
		extra[pattern] = handler
	}
	cfg := lanet.Config{
		Name:             effName,
		NetworkKey:       effKey,
		LegacyDefaultKey: effLegacyKey,
		Standalone:       true,
		Channel:          lanet.ChannelOfficial, // 官方发行渠道：与第三方 SDK 构建网络隔离
		Bootstrap:        nc.bootstrapAddrs,
		EnablePublicDHT:  effPublic,
		PublicDHTTimeout: time.Duration(effPublicMin) * time.Minute,
		IdentityFile:     effIdentity,
		ConsoleAddr:      effConsole,
		ConsolePassword:  effConsolePW,
		StateFile:        filepath.Join(filepath.Dir(*config), "state.json"),
		ConsoleExtra:     extra,
		Tun:              effTun,
		Version:          version,
		Platform:         runtime.GOOS + "/" + runtime.GOARCH,
		// 连接审批（加好友式）：陌生节点需用户同意后才能互连；
		// auto_accept 供无人值守中央服务器使用；地址簿落 lanet.db。
		AutoAccept:      effAutoAccept,
		RequireApproval: &effRequireApproval,
		DBPath:          effDBPath,
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

	node, err := lanet.New(ctx, cfg)
	if err != nil {
		log.Fatalf("[node] 入网失败: %v", err)
	}
	defer node.Close()
	info := node.Info()
	log.Printf("[node] 已入网 name=%s peerID=%s virtualIP=%s network=%s",
		effName, info.PeerID, info.VirtualIP, networkLabel(effKey))

	// ---- .lanet DNS 的系统路由（Windows NRPT 规则）----
	// 节点以管理员运行，把 *.lanet 查询定向到内置 DNS（127.0.0.1:53）。
	// 退出时移除规则，不留系统残留；注册失败仅降级（虚拟 IP 直连不受影响）。
	if err := ensureNRPTRule("127.0.0.1"); err != nil {
		log.Printf("[node] NRPT 规则注册失败（ping <成员名>.lanet 不可用，虚拟 IP 直连不受影响）: %v", err)
	} else {
		log.Printf("[node] NRPT 规则已就绪：*.lanet → 127.0.0.1（ping <成员名>.lanet 直达虚拟 IP）")
	}
	defer func() {
		if err := removeNRPTRule(); err != nil {
			log.Printf("[node] NRPT 规则移除失败（可用 PowerShell Get-DnsClientNrptRule 查看）: %v", err)
		}
	}()

	// 每次启动后台检查一次更新（预拉发行说明，控制台弹框即点即显）。
	StartUpdateCheck(firstNonEmpty(os.Getenv("GITHUB_TOKEN"), readConfigToken(*config)))

	// P2P 自动更新（去中心化分发；强制开启，仅 dev/容器环境自动禁用）。
	if reason := StartP2PUpdate(ctx, node, version, filepath.Dir(*config)); reason != "" {
		log.Printf("[p2p-update] 已禁用：%s", reason)
	}

	// ---- 托盘 + 自动打开控制台（Windows 图形界面模式）----
	// 配置文件仅在首次启动时创建；升级或重启时文件已存在，不再自动开新页签。
	// 开机自启（-autorun / 注册表 Run 键拉起）永远不自动开页签——开机场景
	// 用户没有交互预期，弹浏览器只会打扰；托盘照常启动，随时可手动打开。
	autorunLaunch := isAutorunLaunch()
	if consoleURL := node.ConsoleURL(); consoleURL != "" && runtime.GOOS == "windows" {
		startTray(func() string { return consoleURL }, cancel)
		if !autorunLaunch && shouldAutoOpenConsole(cfgCreated) && shouldOpenConsole(consoleURL) {
			openBrowser(consoleURL)
		} else {
			log.Printf("[node] 跳过自动打开控制台页签 (autorun=%v): %s", autorunLaunch, consoleURL)
		}
	}
	if autorunLaunch {
		log.Printf("[node] 本次为开机自启启动")
	}

	// 回显服务：收到什么回什么（供其他节点探测）。
	// 注意：不再向 OnStream 注册 Tunnel 协议 echo——TUN 开启时该协议是
	// IP 数据面（ping/任意端口直达虚拟 IP 的承载），应用层 echo 会与之
	// 抢流、吞掉入向 IP 包导致 ping 不通。探测统一走独立 echoProto。
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
	// （原 Tunnel 协议 OnStream echo 已删除：与 TUN 数据面冲突，见上）

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
			if !shouldProbe(self, m) {
				continue
			}
			probeOnce(ctx, node, m.Name, m.VirtualIP)
		}
	}
}

// shouldProbe 判断某个成员是否值得做 echo 探测。
//
// 跳过两类无意义目标，避免每轮对必然失败的成员反复拨号（刷日志 + 白耗流量）：
//   - 自己（虚拟 IP 相同）或虚拟 IP 缺失；
//   - 「幽灵成员」：从未通过 info 协议握手成功（Version 为空）且没有任何
//     可用 underlay 地址。它们通常只是 DHT/mDNS 里的陈旧记录，对端早已下线。
//
// 一旦对端真正上线并完成握手（Version 被填上），会被自动纳入探测。
func shouldProbe(selfIP string, m netmapclient.Member) bool {
	if m.VirtualIP == "" || m.VirtualIP == selfIP {
		return false
	}
	if m.Version == "" && len(m.Addrs) == 0 {
		return false
	}
	return true
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

// atoiOr 解析十进制整数，失败或空串时返回 def。
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// publicDHTMinutesOr 公共 DHT 时长归一化：≤0（旧配置未设置）一律回落到默认 10 分钟。
func publicDHTMinutesOr(v int) int {
	if v <= 0 {
		return 10
	}
	return v
}

// resolvePublicDHT 解析公共 DHT 开关的最终生效值。
//
// flagVal 为命令行/环境变量的原始字符串，约定 "@@unset@@" 表示「本次未传」：
//   - 未传 → 采用配置文件的值（cfgVal）；
//   - 传了 → 用显式值覆盖配置（"1"/"true"（忽略大小写）为真，其余为假）。
//
// 这一点必须用哨兵而非 flag.Bool：flag.Bool 的零值 false 无法区分
// 「用户显式传了 false」与「用户根本没传」，会让配置文件里的 true
// 被无声忽略（历史 bug：`-public-dht` 曾是无条件 OR，传什么都会开启）。
func resolvePublicDHT(flagVal string, cfgVal bool) bool {
	if flagVal == "@@unset@@" {
		return cfgVal
	}
	return flagVal == "1" || strings.EqualFold(flagVal, "true")
}

// resolveNetworkKey 解析网络密钥，返回 (生效密钥, 是否历史公共网络)。
//
// 优先级：命令行/环境变量 > 配置文件 > 按身份派生默认网络。
// 迁移规则（为什么要区分「未设置」与「显式留空」）：
//   - 老版本留空 = 固定公共网络密钥 lanet/public，所有零配置节点同网。
//     若升级后直接改成「按身份派生」，老节点会静默脱离原网络，互相失联。
//   - 因此：配置文件从未写过 network_key（nil）→ 判定为老部署，保持
//     历史公共网络（legacy=true），升级后网络关系不变，零迁移。
//   - 而显式留空（""）视为新语义：使用按身份派生的本机专属默认网络，
//     避免所有零配置用户挤在一张超大网里。
//   - 非空值一律照用（自定义密钥不受影响）。
func resolveNetworkKey(flagVal string, cfgVal *string) (string, bool) {
	// 命令行/环境变量显式给了值（含显式空串）→ 按新语义处理：
	// 空串 = 本机专属默认网络（非 legacy）。
	if flagVal != "@@unset@@" {
		return flagVal, false
	}
	if cfgVal == nil {
		// 老配置文件没有该字段：保持历史公共网络，避免升级后失联。
		return "", true
	}
	return *cfgVal, false
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

// hasFlag 判断命令行是否包含指定 flag（不解析值，仅检测 -flag / --flag 形式）。
// osArgs 为可注入变量，便于单元测试。
var osArgs = os.Args

func hasFlag(name string) bool {
	prefix := "-" + name
	for _, arg := range osArgs[1:] {
		if arg == prefix || arg == "-"+prefix {
			return true
		}
	}
	return false
}

// exeDir 可执行文件所在目录（双击启动时配置/身份/状态文件都落在这里）。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// defaultIdentityPath 身份密钥固定路径：Windows 存裸文件名 node.key
// （按配置文件所在目录解析，整个文件夹移动/改名都不断链），
// 其他平台维持容器约定 /data/node.key。路径不写入配置、不对外展示，
// 文件不存在即视为新用户，由 SDK 自动创建新身份。
func defaultIdentityPath(exeDir string) string {
	if runtime.GOOS == "windows" {
		return "node.key"
	}
	return "/data/node.key"
}

// nodeConfig lanet.json 配置文件结构（字段与命令行参数一一对应）。
// 双击/零参数启动时全靠它；Web 控制台「节点配置」编辑的就是这个文件，
// 保存后重启程序生效（防火墙与转发映射在控制台里是热生效的，不在此列）。
type nodeConfig struct {
	Name string `json:"name"`
	// NetworkKey 网络密钥（*string 以便区分「未设置」与「显式留空」）：
	//   - nil   = 老配置文件从未写过该字段 → 视为历史公共网络（迁移兼容）；
	//   - ""     = 用户显式留空 → 使用按身份派生的本机专属默认网络；
	//   - 非空   = 用户指定的网络密钥。
	// 详见 resolveNetworkKey 的迁移规则。
	NetworkKey      *string `json:"network_key,omitempty"`
	Bootstrap       string  `json:"bootstrap"`
	Console         string  `json:"console"`
	ConsolePassword string  `json:"console_password,omitempty"`
	Firewall        string  `json:"firewall"`
	Listen          string  `json:"listen"`
	EnablePublicDHT bool    `json:"enable_public_dht"` // 公共 DHT 兜底开关（默认关闭，v0.5.16 起语义反转）
	// PublicDHTMinutes 公共 DHT 临时引导的最长运行分钟数（默认 10）。
	// 开启公共 DHT 后：连上第一个同群成员立即退出；超时仍未连上也退出。
	// 控制台可改，重启生效。
	PublicDHTMinutes int   `json:"public_dht_minutes,omitempty"`
	ProbeSec         int   `json:"probe_seconds"`
	Tun              *bool `json:"tun,omitempty"` // 虚拟网卡 TUN；nil = 默认开启（兼容旧配置文件）
	// dns 字段已移除：.lanet DNS 强制开启（无开关），旧配置遗留 dns 键被忽略。
	// self_update 字段已移除：P2P 自动更新强制开启（签名信任锚保证安全）。
	// 旧配置文件里遗留的 self_update 键会被 json 忽略，无副作用。
	GitHubToken string `json:"github_token,omitempty"` // 私有仓库检查更新用（contents:read）
	// Autorun 开机自启（仅控制台 PUT 请求体使用，GET 走注册表实时查询）。
	// 不落 lanet.json：真实状态在注册表 Run 键，避免两处状态不一致。
	Autorun *bool `json:"autorun,omitempty"`
	// RequireApproval 是否要求连接审批（默认 true）：开启后陌生节点必须经
	// 用户同意（类似加好友）才能互连；关闭则同网络密钥的节点可直接互连。
	// nil = 未设置（按默认 true）。
	RequireApproval *bool `json:"require_approval,omitempty"`
	// AutoAccept 自动同意连接申请（默认 false）：无人值守中央服务器用。
	// 开启后陌生节点无需人工审批即自动信任——会放弃「加好友」这道边界。
	AutoAccept *bool `json:"auto_accept,omitempty"`
	// DBPath 地址簿数据库路径（默认 exe 同目录 lanet.db，"-" = 仅内存）。
	DBPath string `json:"db_path,omitempty"`

	bootstrapAddrs []string `json:"-"` // 运行时由 Bootstrap 解析而来
}

// loadNodeConfig 读取配置文件；不存在或损坏时返回默认配置并尽力生成模板文件。
// 返回值 created 表示本次是否新生成了模板。
func loadNodeConfig(path string) (*nodeConfig, bool) {
	if data, err := os.ReadFile(path); err == nil {
		var nc nodeConfig
		if json.Unmarshal(data, &nc) == nil {
			return &nc, false
		}
		log.Printf("[node] 配置文件解析失败（按默认值运行，可删除该文件重新生成）: %s", path)
	}
	nc := defaultNodeConfig()
	if err := nc.save(path); err != nil {
		log.Printf("[node] 默认配置文件生成失败（不影响启动）: %v", err)
	}
	return nc, true
}

// defaultNodeConfig 开箱即用默认值：主机名作为节点名、无引导（私有 DHT +
// mDNS，不接触公共 DHT）、控制台全开。
func defaultNodeConfig() *nodeConfig {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "node"
	}
	return &nodeConfig{
		Name: name,
		// 显式写空串（而非省略字段）：新生成的配置文件明确表达「使用按身份
		// 派生的本机专属默认网络」，避免日后被迁移逻辑误判为老配置。
		// 要与他人互通，把这里改成双方约定的相同密钥即可。
		NetworkKey:       strPtr(""),
		Bootstrap:        "none",
		Console:          "127.0.0.1:8900",
		Firewall:         "allow-all",
		PublicDHTMinutes: 10,
		ProbeSec:         20,
		Tun:              boolPtr(true),
		// 连接审批默认开启：陌生节点需用户同意后才互连（类似加好友）。
		// 无人值守的中央服务器可把 auto_accept 置 true 免除人工审批。
		RequireApproval: boolPtr(true),
		AutoAccept:      boolPtr(false),
	}
}

// resolveTriBool 三态布尔解析：显式传值（"true"/"false"/"1"/"0"）优先，
// 其次配置文件（nil = 未设置），最后默认值。用于审批这类「默认开启、
// 允许关闭」的开关——不能用 flag.Bool，其零值 false 无法区分未传与显式 false。
func resolveTriBool(flagVal string, cfgVal *bool, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(flagVal)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	if flagVal != "@@unset@@" && strings.TrimSpace(flagVal) != "" {
		// 传了非法值：按默认处理，但明确提示，避免静默误配。
		log.Printf("[node] 布尔参数取值无法识别（%q），按默认 %v 处理", flagVal, def)
	}
	if cfgVal != nil {
		return *cfgVal
	}
	return def
}

// boolPtr 返回布尔指针（配置文件可选字段用）。
func boolPtr(v bool) *bool { return &v }

// strPtr 返回字符串指针（配置文件可选字段用）。
func strPtr(v string) *string { return &v }

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

// nodeRuntime 当前进程实际生效的运行参数。可能来自命令行/环境变量，
// 与 lanet.json 保存值不一致（配置页据此提示「重启后才切换」）。
type nodeRuntime struct {
	Name             string
	NetworkKey       string
	Console          string
	Listen           string
	Firewall         string
	EnablePublicDHT  bool
	PublicDHTMinutes int
	Tun              bool
	// 连接审批（加好友式）：RequireApproval 是否需要用户同意；
	// AutoAccept 自动同意（无人值守中央服务器）；DBPath 地址簿路径。
	RequireApproval bool
	AutoAccept      bool
	DBPath          string
}

// networkID 运行时网络标识（与 SDK/控制台页眉一致）：standalone- + 群组指纹。
// 官方程序固定使用官方渠道派生（与 GroupKey 历史结果一致，老网络不变）。
func networkID(networkKey string) string {
	return "standalone-" + serverless.GroupFingerprint(serverless.GroupKey(serverless.ChannelOfficial, networkKey))
}

// nodeConfigRoutes 节点配置 API：GET 读取 / PUT 保存（写回 lanet.json，重启生效）。
// GET 同时返回 runtime（当前进程实际生效值），前端据此提示与文件保存值的差异。
func nodeConfigRoutes(path string, eff nodeRuntime) map[string]http.HandlerFunc {
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
			tunOn := nc.Tun == nil || *nc.Tun
			requireApprovalOn := nc.RequireApproval == nil || *nc.RequireApproval
			autoAcceptOn := nc.AutoAccept != nil && *nc.AutoAccept
			writeJSONLocal(w, http.StatusOK, map[string]any{
				"config_path":        path,
				"name":               nc.Name,
				"network_key":        nc.NetworkKey,
				"bootstrap":          nc.Bootstrap,
				"console":            nc.Console,
				"has_password":       nc.ConsolePassword != "",
				"firewall":           nc.Firewall,
				"listen":             nc.Listen,
				"no_public_dht":      true, // 兼容旧前端字段，恒 true（v0.5.16 起默认关闭公共 DHT）
				"enable_public_dht":  nc.EnablePublicDHT,
				"public_dht_minutes": publicDHTMinutesOr(nc.PublicDHTMinutes),
				"probe_seconds":      nc.ProbeSec,
				"tun":                tunOn,
				"autorun":            isAutorunEnabled(),
				"autorun_supported":  autorunSupported(),
				"require_approval":   requireApprovalOn,
				"auto_accept":        autoAcceptOn,
				"db_path":            nc.DBPath,
				"runtime": map[string]any{
					"name":               eff.Name,
					"network_key":        eff.NetworkKey,
					"network_id":         networkID(eff.NetworkKey),
					"network_text":       networkLabel(eff.NetworkKey),
					"console":            eff.Console,
					"listen":             eff.Listen,
					"firewall":           eff.Firewall,
					"enable_public_dht":  eff.EnablePublicDHT,
					"public_dht_minutes": eff.PublicDHTMinutes,
					"tun":                eff.Tun,
					"require_approval":   eff.RequireApproval,
					"auto_accept":        eff.AutoAccept,
					"db_path":            eff.DBPath,
				},
			})
		},
		"PUT /api/node-config": func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				nodeConfig
				// PartialPublicDHT 标记「仅更新公共 DHT 开关及时长」的轻量请求
				// （控制台开关点击时调用：运行时已即时生效，这里只持久化配置意图）。
				PartialPublicDHT *bool `json:"partial_public_dht"`
				// PartialApproval 标记「仅更新连接审批开关」的轻量请求
				// （控制台复选框点击时调用：无需重启即可持久化，重启后生效）。
				PartialApproval *bool `json:"partial_approval"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "请求体非法: " + err.Error()})
				return
			}
			// 仅连接审批开关的轻量持久化（不改动其它字段）。
			if req.PartialApproval != nil && req.Name == "" && req.Console == "" {
				cur := read()
				if req.RequireApproval != nil {
					cur.RequireApproval = req.RequireApproval
				}
				if req.AutoAccept != nil {
					cur.AutoAccept = req.AutoAccept
				}
				if err := cur.save(path); err != nil {
					writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": "保存失败: " + err.Error()})
					return
				}
				writeJSONLocal(w, http.StatusOK, map[string]any{"saved": true, "restart_required": true})
				return
			}
			// 仅公共 DHT 开关的轻量持久化（不改动其它字段，不触发重启校验）。
			if req.PartialPublicDHT != nil && req.Name == "" && req.Console == "" {
				cur := read()
				cur.EnablePublicDHT = *req.PartialPublicDHT
				if req.PublicDHTMinutes > 0 {
					cur.PublicDHTMinutes = req.PublicDHTMinutes
				}
				if err := cur.save(path); err != nil {
					writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": "保存失败: " + err.Error()})
					return
				}
				writeJSONLocal(w, http.StatusOK, map[string]any{"saved": true, "restart_required": false})
				return
			}
			// autorun-only 请求（自启复选框即时切换）：只带 autorun 字段，
			// 不走完整表单校验，改完即返回（不触发重启提示）。
			if req.Autorun != nil && req.Name == "" && req.Console == "" {
				if !autorunSupported() {
					writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "当前平台不支持开机自启"})
					return
				}
				if err := setAutorunEnabled(*req.Autorun); err != nil {
					writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": "开机自启设置失败: " + err.Error()})
					return
				}
				writeJSONLocal(w, http.StatusOK, map[string]any{"saved": true, "restart_required": false})
				return
			}
			switch req.Firewall {
			case "deny-all", "allow-list", "allow-all", "":
				// 空 = 页面未提供（防火墙已移入独立页签配置），保留原值
			default:
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "firewall 必须是 deny-all / allow-list / allow-all"})
				return
			}
			if req.Firewall == "" {
				req.Firewall = read().Firewall
			}
			if req.Bootstrap == "" {
				req.Bootstrap = read().Bootstrap
			}
			if req.Console == "" || req.Name == "" {
				writeJSONLocal(w, http.StatusBadRequest, map[string]string{"error": "name 与 console 不能为空"})
				return
			}
			if req.ProbeSec < 0 {
				req.ProbeSec = 20
			}
			// 公共 DHT 时长：≤0 视为页面未提供（旧版控制台），保留原值/默认 10。
			if req.PublicDHTMinutes <= 0 {
				prev := read()
				req.PublicDHTMinutes = publicDHTMinutesOr(prev.PublicDHTMinutes)
			}
			prev := read()
			if req.Tun == nil {
				req.Tun = prev.Tun // 页面未提供（旧版控制台）时保留原值
			}
			// 连接审批：页面未提供（旧版控制台）时保留原值。
			if req.RequireApproval == nil {
				req.RequireApproval = prev.RequireApproval
			}
			if req.AutoAccept == nil {
				req.AutoAccept = prev.AutoAccept
			}
			if req.DBPath == "" {
				req.DBPath = prev.DBPath
			}
			// 开机自启：平台支持时按请求值切换（立即生效，无需重启）。
			if req.Autorun != nil && autorunSupported() {
				if err := setAutorunEnabled(*req.Autorun); err != nil {
					writeJSONLocal(w, http.StatusInternalServerError, map[string]string{"error": "开机自启设置失败: " + err.Error()})
					return
				}
			}
			// 密码语义：传了非空密码 → 覆盖；留空 → 保持不变（空密码 = 未设置）。
			// GET 不回传密码明文，所以 req.ConsolePassword 为空时不能当作"删除"。
			if req.ConsolePassword == "" {
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
