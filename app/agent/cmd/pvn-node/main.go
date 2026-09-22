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
	"errors"
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
	"sync"
	"syscall"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	lanetproto "github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/libp2p/go-libp2p/core/network"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// echoProto 节点间探测回显协议（历史固定 ID，0.5.34 起默认按群派生——
// 见 echoProtoFor；此常量保留作 LegacyProtocols 逃生与新老混跑兜底）。
const echoProto = libprotocol.ID("/lanet/echo/1.0.0")

// echoProtoFor 按本群密钥派生探测回显协议 ID（LegacyProtocols 时退回固定值）。
// 探测本质是成员间私有链路，固定 ID 会让任意公网扫描器都能触发回显消耗流量。
func echoProtoFor(groupKey []byte, legacy bool) libprotocol.ID {
	if legacy || len(groupKey) == 0 {
		return echoProto
	}
	return lanetproto.GroupProtoID(lanetproto.BaseEcho, groupKey)
}

// version 由 CI 经 -ldflags "-X main.version=<VERSION 文件内容>" 注入。
var version = "dev"

func main() {
	// 服务重启辅助进程必须先于 SCM 入口处理。它由正在运行的服务派生，等待
	// HTTP 响应送达后通过服务管理器执行 stop/start，避免绕过 SCM 拉起孤儿进程。
	if handled, err := handleWindowsServiceCommand(); handled {
		if err != nil {
			log.Printf("[service] Windows 服务控制失败: %v", err)
		}
		return
	}
	// 托盘伴侣进程（-tray）：由「用户登录时」计划任务拉起，跑在用户会话里，
	// 只画图标 + 读控制台状态，不创建节点（见 tray_mode_windows.go）。必须挡在
	// 服务接管与节点初始化之前——它既不建 TUN、也不该去抢单实例锁。
	if handled, err := runTrayCompanion(); handled {
		if err != nil {
			log.Printf("[tray] 托盘启动失败: %v", err)
		}
		return
	}
	// Windows 服务模式必须在解析普通命令行参数之前接管进程：SCM 启动
	// lanet.exe -service 后，由服务控制管理器负责 Start/Stop 生命周期。
	// 普通双击/命令行启动则直接进入交互模式。
	if handled, err := runAsSystemService(func(ctx context.Context) {
		runNode(ctx, true)
	}); handled {
		if err != nil {
			log.Printf("[service] Windows 服务运行失败: %v", err)
		}
		return
	}
	runNode(context.Background(), false)
}

func runNode(parent context.Context, serviceMode bool) {
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
			"引导节点：none（默认，仅私有 DHT + mDNS，不接触公共设施）/ 成员 multiaddr（私有种子）/ "+
				"public（公共引导，须同时开启公共 DHT 才生效）；不传则读配置文件")
		console = flag.String("console", envOr("LANET_CONSOLE", ""),
			"控制台监听地址；不传则读配置文件（默认 127.0.0.1:8900 仅本机，0.0.0.0:8900 = 允许远程）")
		consolePW = flag.String("console-password", envOr("LANET_CONSOLE_PASSWORD", ""),
			"控制台访问密码（远程访问时务必设置）；不传则读配置文件")
		fw = flag.String("fw", envOr("LANET_FW", ""),
			"防火墙模式：deny-all / allow-list / allow-all；不传则读配置文件")
		listen = flag.String("listen", envOr("LANET_LISTEN", ""),
			"覆盖监听地址（逗号分隔）；默认 tcp/ws/quic 全部随机端口")
		tun = flag.String("tun", envOr("LANET_TUN", "@@unset@@"),
			"虚拟网卡 TUN（true/false，可经 LANET_TUN 设置）；缺省读配置文件（默认 true）")
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
		legacyProto = flag.String("legacy-protocols", envOr("LANET_LEGACY_PROTOCOLS", "@@unset@@"),
			"退回历史固定协议 ID（true/false，默认 false）：仅在与未升级到 0.5.34 的老版本对端互通受阻时临时开启；"+
				"开启后重新暴露于跨群噪音，问题解决后应关闭")
		probe = flag.Duration("probe", envDurationOr("LANET_PROBE"),
			"成员探测间隔（可经 LANET_PROBE 设置，Go duration 如 20s/1m）；不传则读配置文件（默认 20s）")
		maxProcs = flag.Int("maxprocs", atoiOr(envOr("LANET_MAX_PROCS", ""), 0),
			"调度线程上限 GOMAXPROCS（LANET_MAX_PROCS）；0 = 自动：容器 CPU 配额优先（Go 1.25 起"+
				"原生读取 cgroup，配额 2 核即用 2 线程），无配额时默认 min(核数, 4)。"+
				"限制 CPU/线程占用，网络数据面走 epoll 不受影响")
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

	// ---- 调度线程上限：限制 CPU/线程数，不影响网络数据面 ----
	// Go 的网络 IO 走 epoll/kqueue（goroutine 阻塞在 poller 上，不占线程），
	// GOMAXPROCS 只约束「同时执行 Go 代码」的 OS 线程数——收紧它不会降低
	// 转发/隧道吞吐，却能避免「容器未配 CPU 限额时按宿主核数开满」的失控占用。
	// Go 1.25 起 GOMAXPROCS 默认已感知 cgroup CPU 配额：配置了 cpus 的容器
	// 自动生效；这里兜底处理「无配额」场景（默认 min(核数,4)）与显式覆盖。
	applyMaxProcs(*maxProcs)

	// ---- 日志：stderr + exe 同目录 lanet.log 双写（windowsgui 无黑框时靠文件看日志）----
	// 注意：不能用 io.MultiWriter(os.Stderr, lf)——windowsgui 下 stderr 是无效句柄，
	// 写入报错后 MultiWriter 提前返回，文件永远写不进。这里逐个写、忽略单点错误。
	// 日志文件按大小轮转（10MB × 保留 3 份），避免长期运行无限增长。
	if lf, err := newRotatingFile(filepath.Join(filepath.Dir(*config), "lanet.log"),
		logMaxSize, logMaxBackups); err == nil {
		log.SetOutput(tolerantWriter{[]io.Writer{os.Stderr, lf}})
	}

	// ---- 单实例：同一配置目录只允许一个节点进程（见 singleton.go）----
	// 必须放在开库、建 TUN、抢控制台端口之前：双开的两个进程会共用同一份
	// node.key / lanet.db / state.json / TUN 网卡，并在 8900 被占时静默回退到
	// 8901，表现为「控制台上凭空多出一个端口」而毫无报错。
	instLock, holder, lockErr := acquireSingleton(*config)
	switch {
	case lockErr == nil:
		defer instLock.release()
	case errors.Is(lockErr, errSingletonBusy):
		refuseSecondInstance(holder, serviceMode)
		return
	default:
		// 锁文件不可用（目录只读等）只降级警告，不阻断启动：单实例是保护性
		// 措施，不该因为权限/文件系统异常把节点彻底挡死。
		log.Printf("[node] 单实例锁不可用（继续启动，存在双开风险）: %v", lockErr)
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
	// 引导默认「none」：不接触任何公共设施（公共 DHT 默认关闭，跨网冷启动
	// 靠成员引导种子 / 控制台「连接种子」）。要临时用公共 DHT 兜底，需同时
	// 显式 bootstrap=public 且开启公共 DHT —— 二者缺一，公共引导都会被忽略。
	// 历史行为默认 "public"：公共 DHT 关闭时该地址本就会被剔除（等价 none），
	// 但字面值会误导用户以为「默认挂了公共网络」，故改为显式 none。
	effBootstrap := firstNonEmpty(*bootstrap, nc.Bootstrap, "none")
	// 身份文件路径固定：配置文件同目录 node.key（Windows）/ /data/node.key
	// （其他平台），不读配置、不暴露到控制台；文件不存在即新用户，SDK 自动
	// 创建新身份。锚点必须是配置目录的绝对路径：服务/计划任务的 CWD 不是
	// 程序目录，相对路径会让节点在 CWD 下新建身份（详见 defaultIdentityPath）。
	effIdentity := defaultIdentityPath(*config)
	effConsole := firstNonEmpty(*console, nc.Console, "127.0.0.1:8900")
	effConsolePW := firstNonEmpty(*consolePW, nc.ConsolePassword)
	effFW := firstNonEmpty(*fw, nc.Firewall, "allow-all")
	effListen := firstNonEmpty(*listen, nc.Listen)
	// TUN 默认开启：配置文件缺省字段（nil）视为 true，命令行显式 true/false 优先。
	effTun := nc.Tun == nil || *nc.Tun
	if *tun != "@@unset@@" {
		effTun = parseBoolLike(*tun)
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
	// 私有协议加固逃生开关（0.5.34）：默认 false = 派生协议 ID；置 true 退回固定 ID。
	effLegacyProto := resolveTriBool(*legacyProto, nc.LegacyProtocols, false)
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
		LegacyProtocols:  effLegacyProto,
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[node] 启动 name=%s key=%q fw=%s console=%s publicDHT=%v(%dm) tun=%v version=%s config=%s identity=%s",
		effName, effKey, effFW, effConsole, effPublic, effPublicMin, effTun, version, *config, effIdentity)
	log.Printf("[node] 连接审批：require=%v autoAccept=%v db=%s",
		effRequireApproval, effAutoAccept, effDBPath)

	switch strings.TrimSpace(effBootstrap) {
	case "", "none":
		// 无引导节点（默认）：私有 DHT + mDNS 发现，不接触任何公共设施。
	case "public":
		// 显式选择公共引导：仅当公共 DHT 开启时才真正参与连接
		// （关闭公共 DHT 时该地址会在 serverless 初始化被剔除）。
		nc.bootstrapAddrs = []string{serverless.DefaultBootstrap}
		if !effPublic {
			log.Printf("[node] bootstrap=public 但公共 DHT 未开启：公共引导地址将被忽略，节点只走私有 DHT + mDNS")
		}
	default:
		for _, a := range strings.Split(effBootstrap, ",") {
			if a = strings.TrimSpace(a); a != "" {
				nc.bootstrapAddrs = append(nc.bootstrapAddrs, a)
			}
		}
	}
	// 更新 / 重启 / 退出 控制台接口（与节点配置同一组扩展路由）。
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
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
		LegacyProtocols:  effLegacyProto,
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
	// 控制台端口可能向后回退，把**实际**地址补写进锁文件：后面被拒绝启动的
	// 实例靠它把用户直接送到正在跑的那份控制台上。
	instLock.refresh(effName, node.ConsoleURL())

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
	if consoleURL := node.ConsoleURL(); consoleURL != "" && runtime.GOOS == "windows" && !serviceMode {
		startTray(func() string { return consoleURL }, cancel)
		if !autorunLaunch && shouldAutoOpenConsole(cfgCreated) && shouldOpenConsole(consoleURL) {
			openBrowser(consoleURL)
		} else {
			log.Printf("[node] 跳过自动打开控制台页签 (autorun=%v): %s", autorunLaunch, consoleURL)
		}
	}
	if serviceMode {
		log.Printf("[service] 正以 Windows 系统服务运行（LocalSystem，无需用户登录；不启动托盘和浏览器）")
	} else if autorunLaunch {
		log.Printf("[node] 本次为用户登录后自启启动")
	}

	// 回显服务：收到什么回什么（供其他节点探测）。
	// 注意：不再向 OnStream 注册 Tunnel 协议 echo——TUN 开启时该协议是
	// IP 数据面（ping/任意端口直达虚拟 IP 的承载），应用层 echo 会与之
	// 抢流、吞掉入向 IP 包导致 ping 不通。探测统一走独立 echoProto。
	// 0.5.34：协议 ID 按群派生，异群/扫描器协商不上，不再浪费回显流量。
	// 混版本过渡：派生 ID 为主 handler；同群老版本（≤0.5.33）只认固定 ID，
	// 故固定 ID 也注册一个「仅已信任好友」的门禁版本，老→新探测不断。
	echoP := echoProtoFor(node.GroupKey(), cfg.LegacyProtocols)
	echoHandler := func(s network.Stream) {
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
	}
	node.Host().SetStreamHandler(echoP, echoHandler)
	if string(echoP) != string(echoProto) {
		node.Host().SetStreamHandler(echoProto, func(s network.Stream) {
			if !node.IsPeerTrusted(s.Conn().RemotePeer().String()) {
				_ = s.Reset()
				return
			}
			echoHandler(s)
		})
	}
	// （原 Tunnel 协议 OnStream echo 已删除：与 TUN 数据面冲突，见上）

	go node.Run(ctx)

	// 周期探测：向成员表内所有其他成员发起 echo 往返。
	//
	// probe 同时承担两个职责：① 连通性可见（控制台在线状态与 RTT）；
	// ② P2P 连接保温——间隔过长时 libp2p 连接会被空闲回收，TUN 数据面
	// 每包都要重拨。因此**成功路径不引入任何延迟**，只有连续失败的成员
	// 才退避（它们本来就没有连接需要保温）。
	//
	// 退避的必要性：地址簿被污染时，每个失败的成员每轮都要在几十条不可达
	// 地址上并发拨号，稳态下数千个拨号 goroutine 挂着，内存被撑到 1G。
	lastMembers := ""
	// 冷启动细粒度轮询：成员表首次同步（约 15s）一到就立即开探，不等稳态
	// tick；首轮全网探测全部成功或 60s 超时后回落到 effProbe 稳态节奏。
	ticker := time.NewTicker(coldProbeTick)
	defer ticker.Stop()
	cold := true
	coldStart := time.Now()
	backoff := newProbeBackoff()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[node] 收到退出信号")
			return
		case <-ticker.C:
		}
		members := node.NetMap().Members
		sig := membersSignature(members)
		if sig != lastMembers {
			log.Printf("[node] 成员表更新（%d 人）: %s", len(members), sig)
			lastMembers = sig
		}
		self := node.Info().VirtualIP
		now := time.Now()
		type probeTarget struct{ name, virtualIP string }
		var targets []probeTarget
		for _, m := range members {
			if !shouldProbe(self, m) {
				continue
			}
			if !backoff.due(m.VirtualIP, now) {
				continue
			}
			targets = append(targets, probeTarget{m.Name, m.VirtualIP})
		}
		results := make([]bool, len(targets))
		// 有界并发探测：串行时每个离线成员的拨号超时（8s）会把排在后面的
		// 在线成员拖住，10 成员 5 离线的首轮要 40s+。并发后坏成员只阻塞
		// 自己；同一对端不会重复探测（targets 唯一），跨对端拨号由 libp2p
		// swarm 的并发限制与 backoff 兜底，probeBackoff 自带锁可并发 record。
		var wg sync.WaitGroup
		sem := make(chan struct{}, probeWorkers)
		for i, t := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, t probeTarget) {
				defer wg.Done()
				defer func() { <-sem }()
				results[i] = probeOnce(ctx, node, t.name, t.virtualIP)
				backoff.record(t.virtualIP, results[i], effProbe)
			}(i, t)
		}
		wg.Wait()
		if cold {
			allOK := len(targets) > 0
			for _, ok := range results {
				if !ok {
					allOK = false
				}
			}
			if allOK || time.Since(coldStart) > time.Minute {
				cold = false
				ticker.Reset(effProbe)
				log.Printf("[node] 冷启动探测完成，进入稳态节奏（%s）", effProbe)
			}
		}
		// 成员表里已消失的目标不再保留退避状态，避免 map 随成员更替增长。
		backoff.retain(members)
	}
}

// probeBackoffMax 连续失败成员的探测间隔上限。
//
// 取 60s 而非更长：probe 兼作连接保温，上限过高会让「其实已恢复、只是
// 探测连续失败」的成员久久不被重新纳入探测。
const probeBackoffMax = 60 * time.Second

// probeWorkers 单轮探测的并发上限。probeOnce 的拨号超时 8s，串行时一个
// 离线成员就把同一轮里排在后面的在线成员全拖住，冷启动首轮全网探测被
// 拖到几十秒。6 路并发下几十个成员 2~3 个拨号窗口内完成；同一对端仍由
// libp2p swarm 的拨号并发限制与 backoff 兜底，不会形成拨号风暴。
const probeWorkers = 6

// coldProbeTick 冷启动阶段的探测轮询间隔。成员表首次同步约在入网后 15s，
// 1s tick 让新成员到位后立即被探测，而不是再等一个稳态周期。首轮全网
// 探测全部成功或 60s 超时后 ticker.Reset(effProbe) 回落稳态节奏。
const coldProbeTick = time.Second

// probeBackoff 记录每个成员的探测退避状态（按虚拟 IP 归档——它是成员在
// 本网络里的稳定身份，换连接也不变）。
//
// 规则：连续失败 n 次的成员，下次探测延迟为 base × 2^(n-1)，上限
// probeBackoffMax。**第 1 次失败不延迟**：单次失败常是瞬时抖动，而 probe
// 还承担保温职责，不该因一次抖动就打乱已连通成员的探测节奏。
type probeBackoff struct {
	mu     sync.Mutex
	fails  map[string]int
	nextAt map[string]time.Time
}

func newProbeBackoff() *probeBackoff {
	return &probeBackoff{fails: make(map[string]int), nextAt: make(map[string]time.Time)}
}

// due 报告目标当前是否到达可探测时间（无记录即视为到期）。
func (b *probeBackoff) due(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.nextAt[key]
	return !ok || !now.Before(at)
}

// record 记录一次探测结果：成功立即清零，失败推进退避窗口。
func (b *probeBackoff) record(key string, ok bool, base time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		delete(b.fails, key)
		delete(b.nextAt, key)
		return
	}
	n := b.fails[key] + 1
	b.fails[key] = n
	if n < 2 {
		return // 首次失败不延迟，下一轮照常探测
	}
	delay := base << (n - 1)
	if delay <= 0 || delay > probeBackoffMax {
		delay = probeBackoffMax
	}
	b.nextAt[key] = time.Now().Add(delay)
}

// retain 丢弃已不在成员表里的目标的退避状态。
func (b *probeBackoff) retain(members []netmapclient.Member) {
	alive := make(map[string]bool, len(members))
	for _, m := range members {
		if m.VirtualIP != "" {
			alive[m.VirtualIP] = true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.fails {
		if !alive[k] {
			delete(b.fails, k)
		}
	}
	for k := range b.nextAt {
		if !alive[k] {
			delete(b.nextAt, k)
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

// probeOnce 对单个成员做一次 echo 往返探测，返回是否成功（失败驱动退避）。
func probeOnce(ctx context.Context, node *lanet.Client, name, virtualIP string) bool {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 出向候选：派生 ID 优先（与新版对端匹配）；同群老版本只认固定 ID，
	// 因此追加固定 ID 兜底（与 serverless/selfupdate 的迁移策略一致）。
	protos := []string{string(echoProtoFor(node.GroupKey(), node.LegacyProtocols()))}
	if p := string(echoProto); p != protos[0] {
		protos = append(protos, p)
	}
	stream, viaRelay, err := node.DialProtocols(pctx, virtualIP, protos)
	if err != nil {
		log.Printf("[probe] FAIL %s(%s): %v", name, virtualIP, err)
		return false
	}
	defer stream.Close()
	payload := fmt.Sprintf("probe-%d", start.UnixMilli())
	if _, err = stream.Write([]byte(payload)); err != nil {
		log.Printf("[probe] FAIL %s(%s): write: %v", name, virtualIP, err)
		return false
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
		return false
	}
	log.Printf("[probe] OK %s(%s) via=%s rtt=%s", name, virtualIP, via, time.Since(start).Round(time.Millisecond))
	return true
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

// envDurationOr 读取环境变量并按 Go duration 解析（如 20s/1m）；
// 未设置或解析失败返回 0（视为「未显式指定」，回落配置文件/内置默认）。
func envDurationOr(k string) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("[node] 环境变量 %s=%q 无法解析为时长，忽略", k, v)
		return 0
	}
	return d
}

// parseBoolLike 宽松布尔解析：true/1/yes/on 为真，false/0/no/off 为假，
// 其余按真处理前已由调用方哨兵值拦截，这里兜底返回原字符串非空的真值语义。
func parseBoolLike(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
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

// defaultIdentityPath 身份密钥固定路径：Windows 放**配置文件同目录**的
// node.key（与 lanet.db / lanet.log 同锚点，整个文件夹移动/改名都不断链），
// 其他平台维持容器约定 /data/node.key。路径不写入配置、不对外展示，
// 文件不存在即视为新用户，由 SDK 自动创建新身份。
//
// 必须返回**绝对路径**：Windows 服务（SCM 拉起，CWD=System32）、计划任务、
// 以及一切从别的目录启动 exe 的场景，进程 CWD 都不是程序目录。返回裸相对名
// "node.key" 会让节点在 CWD 下新建一个全新身份——PeerID 与虚拟 IP 双双漂移，
// 成员表 / 防火墙 / 转发规则 / 已审批信任全部对不上。0.5.39 及以前的服务模式
// 实测如此：身份落到了 C:\Windows\System32\node.key。
func defaultIdentityPath(configPath string) string {
	if runtime.GOOS != "windows" {
		return "/data/node.key"
	}
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "node.key")
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
	// LegacyProtocols 迁移逃生开关（0.5.34 起）：置 true 退回历史固定协议 ID
	// （/lanet/info、/lanet/unfriend、/lanet/kad 等），用于与未升级的老版本
	// 对端互通。默认 false = 启用按群派生的私有协议（跨群噪音在协商层归零）。
	// nil = 未设置（按默认 false）。
	LegacyProtocols *bool `json:"legacy_protocols,omitempty"`

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

// defaultNodeConfig 开箱即用默认值：节点名、无引导（私有 DHT + mDNS，
// 不接触公共 DHT）、控制台全开。
//
// 节点名优先取环境变量 LANET_NAME（容器编排里显式指定的名字），否则退回主机名。
// 容器里 os.Hostname() 通常是容器短 ID（12 位十六进制，如 52b57e329f43），既没
// 可读性，又会与运行时生效名（被 LANET_NAME / -name 覆盖）不一致——表现为控制台
// 页眉显示 fnos、节点配置页却显示 52b57e329f43。有 LANET_NAME 时直接以它落盘，
// 文件值与生效值从第一次启动就一致。
func defaultNodeConfig() *nodeConfig {
	name := strings.TrimSpace(os.Getenv("LANET_NAME"))
	if name == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			name = h
		} else {
			name = "node"
		}
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
	// LegacyProtocols 迁移逃生开关（0.5.34）：true = 退回历史固定协议 ID。
	LegacyProtocols bool
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
				"config_path": path,
				"name":        nc.Name,
				// name_env 标记当前进程的节点名来自环境变量 LANET_NAME（容器编排
				// 常见，优先级高于配置文件）：此时文件里的 name 往往是首次启动
				// 写入的容器主机名（容器短 ID），与生效名不一致。前端据此把名称
				// 输入框回填为实际生效值并说明来源，避免页眉与配置页各显示一个名字。
				"name_env":    os.Getenv("LANET_NAME") != "",
				"network_key": nc.NetworkKey,
				// network_key_env 标记当前进程的网络密钥来自环境变量
				// LANET_NETWORK_KEY（容器编排常见）：lanet.json 里可能没有该值，
				// 前端据此把输入框回填为实际生效值，避免「容器里填了密钥、页面上是空的」。
				"network_key_env":    os.Getenv("LANET_NETWORK_KEY") != "",
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
				"autorun_kind":       autorunKind(),
				"require_approval":   requireApprovalOn,
				"auto_accept":        autoAcceptOn,
				"db_path":            nc.DBPath,
				// 私有协议逃生开关（0.5.34）：true = 历史固定协议 ID（老版本互通），
				// 默认 false = 按群派生（跨群噪音协商层归零）。控制台不展示开关，
				// 仅编辑 lanet.json / 环境变量 LANET_LEGACY_PROTOCOLS 可改。
				"legacy_protocols": nc.LegacyProtocols != nil && *nc.LegacyProtocols,
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
			// autorun-only 请求（Windows 服务自启复选框即时切换）：只带
			// autorun 字段，不走完整表单校验，改完即返回。
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
			// 私有协议逃生开关：页面未提供（无该控件）时保留原值。
			if req.LegacyProtocols == nil {
				req.LegacyProtocols = prev.LegacyProtocols
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
