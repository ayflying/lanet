// Package lanet 是 Lanet 群组制 P2P 虚拟局域网的 Go SDK。
//
// 宿主程序只需十几行代码即可作为一个节点加入群组网格，并与
// 同群组成员建立流式连接（直连优先、中继兜底，与内置 agent
// 的三段隧道策略一致）：
//
//	client, err := lanet.New(lanet.Config{
//		CTLURL:     "http://ctl.example.com:8600",
//		Name:       "my-service",
//		InviteCode: code, // 留空则创建新群组
//		GroupName:  "my-group",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer client.Close()
//
//	// 接收入向流（可选）。
//	client.OnStream(func(stream lanet.Stream) {
//		defer stream.Close()
//		io.Copy(stream, stream) // echo 示例
//	})
//
//	// 按虚拟 IP 主动开流（可选）。
//	stream, viaRelay, err := client.Dial(ctx, "10.7.x.x")
//
//	client.Run(ctx) // 阻塞：周期刷新 NetMap / 通告地址 / 补充中继预约
//
// 浏览器/网页接入不走本包：网页 SDK 基于信令 + WebRTC（阶段 2）。
package lanet

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ayflying/pvn/pkg/peersource"
	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/ayflying/pvn/pkg/tundevice"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// Stream 是隧道流的对外视图：io.ReadWriteCloser + 链路信息。
type Stream interface {
	io.ReadWriteCloser
	// CloseWrite 半关闭写端：发送完毕必须调用，
	// 对端才能 ReadAll 判 EOF（Windows 语义尤其如此）。
	CloseWrite() error
	// Reset 强制中止流（异常场景，不等对端）。
	Reset() error
	// ViaRelay 本流是否经中继转发（false 即 P2P 直连）。
	ViaRelay() bool
	// Protocol 流上的协议 ID。
	Protocol() string
	// RemotePeer 对端 PeerID 文本。
	RemotePeer() string
}

// streamAdapter 把 libp2p network.Stream 适配为 SDK 的 Stream。
type streamAdapter struct {
	network.Stream
	viaRelay bool
}

func (s streamAdapter) CloseWrite() error { return s.Stream.CloseWrite() }
func (s streamAdapter) ViaRelay() bool    { return s.viaRelay }
func (s streamAdapter) Protocol() string  { return string(s.Stream.Protocol()) }

// RemotePeer 返回对端 PeerID 文本。
func (s streamAdapter) RemotePeer() string { return s.Stream.Conn().RemotePeer().String() }

// Handler 收到入向流时的回调。
type Handler func(stream Stream)

// 分发渠道常量（见 Config.Channel）：SDK 构建默认 ChannelSDK，
// 与官方发行版（ChannelOfficial）的网络互相隔离。
const (
	ChannelOfficial = serverless.ChannelOfficial
	ChannelSDK      = serverless.ChannelSDK
)

// Config SDK 配置。
type Config struct {
	// CTLURL 控制面地址（必填），如 http://127.0.0.1:8600。
	CTLURL string
	// Name 节点名称（必填）。
	Name string
	// OS 操作系统标识，留空取 runtime.GOOS。
	OS string
	// InviteCode 凭邀请码加入群组；留空则创建新群组。
	// 仅常规模式（CTLURL 非空）使用；Standalone 模式请改用 NetworkKey。
	InviteCode string
	// GroupName 创建模式下的群组名称（Join 模式忽略）。
	GroupName string
	// ListenAddrs 覆盖默认监听地址（默认 tcp/udp 全部随机端口）。
	ListenAddrs []string
	// WebRTC 启用 webrtc-direct 传输层（默认开启），浏览器节点可直连本节点。
	WebRTC *bool
	// NetMapInterval 周期任务间隔，默认 15s。
	NetMapInterval time.Duration
	// MemberTTL Standalone 模式成员不活跃回收时限：超过该时长无任何
	// 真实通讯的成员从成员表移除，虚拟 IP 派生占用随之释放。
	// 默认 10 分钟，最小 2 分钟；0 = 默认。DHT 陈旧记录不会续命。
	MemberTTL time.Duration
	// Version 本程序版本号（ldflags 注入），随 info 协议上报给同网络成员，
	// 供 P2P 自更新统计全网版本分布。空 = 不上报。
	Version string
	// Platform 本程序平台，默认 runtime.GOOS+"/"+runtime.GOARCH。
	Platform string
	// DialTimeout 开流超时，默认 8s。
	DialTimeout time.Duration
	// Quiet 为 true 时不打日志。
	Quiet bool
	// Standalone 无服务器模式：不依赖 ctl/relay，经 DHT + mDNS 自动发现
	// 同网络成员组网。开启后 CTLURL 可留空；网络归属由 NetworkKey 决定
	// （留空 = 公共网络）。每个节点同时运行 DHT server 与 relay service
	// （客户端即服务端），公网可达成员自动成为网络内的引导与中继节点。
	Standalone bool
	// NetworkKey 仅 Standalone 模式生效：网络密钥。
	//   - 留空：加入公共网络——所有未设置密钥的节点在同一张大网内互相可见可连；
	//   - 填写非空值（任意约定字符串）：加入私有网络，只有持相同密钥的节点
	//     能互相发现与连接（密钥经 SHA256 派生，不可反推）。
	// 注意：SDK 构建的程序与官方发行版程序默认互相隔离（渠道隔离），
	// 即使使用完全相同的 NetworkKey 也不在同一张网络内；如确需互通，
	// 将 Channel 显式设为与对方一致的渠道值（见 Channel 字段说明）。
	NetworkKey string
	// Channel 分发渠道（仅 Standalone 模式生效）：参与群组密钥派生，
	// 用于把不同分发途径的程序隔离在不同网络。留空默认 ChannelSDK
	// （第三方 SDK 构建与官方发行版互不相通）；官方发行版程序
	// 使用 ChannelOfficial。这是防止官方公共网络被 SDK 程序混入的
	// 软隔离边界：显式设置相同 Channel 值即可互通。
	Channel string
	// Bootstrap 仅 Standalone 模式生效：DHT 引导节点 multiaddr 列表。
	// 私有网络下作为「私有 DHT 种子」——填任意已在网成员的 multiaddr 可
	// 加速入网（每台节点都是种子）；填 DefaultBootstrap 会被识别为公共引导。
	// 私有网络默认同时挂公共 DHT 兜底：私有种子全不可达时仍能经公共 DHT
	// 找到第一个同群成员（跨网冷启动零配置）。
	Bootstrap []string
	// DisablePublicDHT 仅 Standalone 私有网络生效：关闭公共 DHT 兜底，
	// 只用私有 DHT + mDNS 发现。适用于不想接入公共网络、且能保证首次
	// 通过种子节点或局域网入网的场景。
	DisablePublicDHT bool
	// LANForwards 局域网端口转发初始映射表：入向请求端口命中 Listen 时，
	// 转发到 Target（本机所在真实局域网内的设备地址，如 192.168.1.100:5000）。
	// 运行中可经 Web 控制台热更新；入向转发始终受防火墙约束（默认全拒绝）。
	LANForwards []LANForward
	// ConsoleAddr 内置 Web 控制台监听地址，默认 127.0.0.1:8900
	// （仅本机可访问；端口被占用时自动向后尝试到 8910）；设为 "-" 关闭控制台。
	// 如需局域网/远程访问请显式设为 0.0.0.0:8900 并务必配合 ConsolePassword。
	ConsoleAddr string
	// ConsolePassword 控制台访问密码；非空时启用登录页 + 会话 Cookie（7 天有效），
	// 未登录访问全部跳转登录页，API 返回 401。空 = 无密码（默认，仅本机监听时安全）。
	ConsolePassword string
	// StateFile 控制台状态（防火墙规则 + 转发映射）持久化文件路径；
	// 空 = 仅内存，节点重启后回到 Config 初始值。
	StateFile string
	// ConsoleExtra 宿主程序向内置 Web 控制台追加的自定义路由。
	// key 为 Go 1.22 http.ServeMux 路由模式（如 "GET /api/node-config"），
	// value 为处理函数；与内置路由共存，路径冲突时启动报错。
	ConsoleExtra map[string]http.HandlerFunc
	// FirewallMode 防火墙初始模式：deny-all（默认，拒绝一切入向）/
	// allow-list（按 FirewallRules 放行）/ allow-all（全开）。
	// 统一管控三类入向暴露面：PortFWD 端口转发、TUN 虚拟网卡入向（IP 层）、
	// OnStream 应用流（协议 /pvn/tunnel/1.0.0 等）。
	// 运行中可经 Web 控制台或 SetFirewall 热更新。
	FirewallMode FirewallMode
	// FirewallRules 防火墙初始放行规则（allow-list 模式生效）。
	FirewallRules []FirewallRule
	// IdentityFile 节点身份密钥文件（Ed25519）。强烈建议配置（如 "node.key"）：
	// 未配置时每次启动随机生成身份，PeerID 与派生虚拟 IP 都会变化；
	// 配置后身份跨重启稳定，虚拟 IP 恒定，其他成员可始终按本节点名称连入。
	// 文件不存在时自动生成并写回（权限 0600）。
	IdentityFile string
	// Tun 创建 TUN 虚拟网卡，启用 IP 层互通：组内成员可通过虚拟 IP 直接
	// ping 本机、访问任意 TCP/UDP 端口（入向仍受防火墙约束）。
	// Windows 需管理员权限、Linux 需 /dev/net/tun + CAP_NET_ADMIN；
	// 创建失败时自动降级为仅应用层（Dial/PortFWD 不受影响），不阻断入网。
	Tun bool
	// TunName TUN 网卡名，默认 "lanet"。
	TunName string
}

// LoadOrCreateIdentity 加载（或首次生成）Ed25519 节点身份密钥。
func LoadOrCreateIdentity(path string) (crypto.PrivKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		return crypto.UnmarshalPrivateKey(data)
	}
	key, _, err := crypto.GenerateEd25519Key(cryptorand.Reader)
	if err != nil {
		return nil, fmt.Errorf("lanet: 生成节点身份: %w", err)
	}
	raw, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("lanet: 序列化节点身份: %w", err)
	}
	// 父目录不存在时自动创建（自定义路径/文件夹移动后的新位置都覆盖）。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("lanet: 创建身份文件目录 %s: %w", dir, err)
		}
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return nil, fmt.Errorf("lanet: 写入身份文件 %s: %w", path, err)
	}
	return key, nil
}

// LANForward 一条局域网端口转发映射。
type LANForward struct {
	// Listen 转发端口（群内成员看到的端口）。
	Listen int `json:"listen"`
	// Target 真实目标地址 "IP:端口"（如 192.168.1.100:5000）。
	Target string `json:"target"`
}

// Client 一个已入网的 SDK 节点。
type Client struct {
	cfg        Config
	node       host.Host
	peerSource *peersource.Client
	netmapCli  *netmapclient.Client
	tunnelSvc  *tunnel.Service
	disc       *serverless.Discovery // Standalone 模式的本地发现服务

	peerID  string
	groupID string
	group   string
	myIP    string
	created bool // 是否为本 SDK 创建的群组

	handlers []Handler

	fw           *firewall.Firewall // 入向转发防火墙（默认 deny-all）
	fwMu         sync.RWMutex
	forwards     []LANForward // 局域网转发映射表（热更新）
	statePath    string       // 状态持久化文件
	consoleSrv   *http.Server // 内置 Web 控制台
	consoleURL   string       // 控制台实际访问地址（端口回退后）
	sessionToken string       // 控制台会话令牌（设置 ConsolePassword 后生成）

	tunMu     sync.Mutex
	tunDevice tundevice.Device  // TUN 虚拟网卡（cfg.Tun 且创建成功时非 nil）
	tunRouter *tundevice.Router // TUN 数据面路由器（非 nil 时 Tunnel 协议已由 TUN 接管）
}

// Info 节点入网后的身份信息。
type Info struct {
	PeerID    string `json:"peer_id"`
	GroupID   string `json:"group_id"`
	Group     string `json:"group"`
	VirtualIP string `json:"virtual_ip"`
	// Name 节点名称（Standalone 即配置名；常规模式为创建/加入时登记的名）。
	Name string `json:"name,omitempty"`
	// VirtualHost 本节点的虚拟地址（<规范化名>.lanet，组内重名自动带后缀）。
	VirtualHost string `json:"virtual_host,omitempty"`
	// Created 仅创建模式为 true（同时携带 InviteCode）。
	Created    bool   `json:"created,omitempty"`
	InviteCode string `json:"invite_code,omitempty"`
}

// New 创建节点并入网：
//   - 常规模式：CTLURL 必填；InviteCode 为空则创建新群组，否则凭码加入。
//   - Standalone 模式：CTLURL 留空；NetworkKey 留空 = 公共网络，
//     填写相同 NetworkKey 的节点组成私有网络，经 DHT + mDNS 自动发现。
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Standalone && cfg.CTLURL != "" {
		return nil, fmt.Errorf("lanet: Standalone 模式不需要 CTLURL")
	}
	if !cfg.Standalone && cfg.CTLURL == "" {
		return nil, fmt.Errorf("lanet: CTLURL 为必填项（或开启 Standalone 无服务器模式）")
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("lanet: Name 为必填项")
	}
	if cfg.OS == "" {
		cfg.OS = defaultOS()
	}
	if cfg.NetMapInterval <= 0 {
		cfg.NetMapInterval = 15 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 8 * time.Second
	}
	if cfg.Standalone {
		// 网络密钥：NetworkKey 优先；兼容回退 InviteCode（旧用法）；
		// 都为空 = 公共网络（所有留空节点互通）。
		if cfg.NetworkKey == "" {
			cfg.NetworkKey = cfg.InviteCode
		}
		// 渠道隔离：SDK 构建默认归属 sdk 渠道，与官方发行版网络互相隔离
		//（即使 NetworkKey 相同也不互通）。
		if cfg.Channel == "" {
			cfg.Channel = serverless.ChannelSDK
		}
	}

	c := &Client{cfg: cfg}

	// 1. libp2p Host。
	listenAddrs := cfg.ListenAddrs
	if len(listenAddrs) == 0 {
		// tcp + ws（浏览器可直连）+ quic；webrtc-direct 由 WebRTC 选项追加。
		listenAddrs = []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/tcp/0/ws",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		}
	}
	spec := p2pkit.HostSpec{
		UserAgent:   "lanet-sdk-go/1.1.0",
		ListenAddrs: listenAddrs,
		WebRTC:      cfg.WebRTC == nil || *cfg.WebRTC,
	}
	if cfg.IdentityFile != "" {
		identity, idErr := LoadOrCreateIdentity(cfg.IdentityFile)
		if idErr != nil {
			return nil, idErr
		}
		spec.Identity = identity
	}
	var disc *serverless.Discovery
	if cfg.Standalone {
		// 无服务器：节点即服务端（无条件启动 hop 中继），打洞直连优先，
		// 成员表充当 relay 候选。
		spec.RelayServiceAlways = true
		spec.HolePunching = true
	} else {
		c.peerSource = peersource.NewClient(cfg.CTLURL)
		spec.RelaySource = c.peerSource.AutoRelayPeerSource()
	}
	node, err := p2pkit.NewHost(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("lanet: create host: %w", err)
	}
	c.node = node
	c.peerID = node.ID().String()
	c.logf("节点 PeerID=%s", c.peerID)

	// 2. 入网。
	if cfg.Standalone {
		disc, err = serverless.New(ctx, node, serverless.Config{
			NetworkKey:            cfg.NetworkKey,
			Channel:               cfg.Channel,
			Name:                  cfg.Name,
			Bootstrap:             cfg.Bootstrap,
			DisablePublicFallback: cfg.DisablePublicDHT,
			EnableMDNS:            true,
			Interval:              cfg.NetMapInterval,
			MemberTTL:             cfg.MemberTTL,
			Version:               cfg.Version,
			Platform:              cfg.Platform,
			Quiet:                 cfg.Quiet,
		})
		if err == nil {
			err = disc.Start(ctx)
		}
		if err != nil {
			_ = node.Close()
			return nil, err
		}
		c.disc = disc
		c.groupID = "standalone"
		c.group = "standalone-" + serverless.GroupFingerprint(disc.GroupKey())
		c.myIP = disc.SelfVirtualIP()
		c.created = true
		if cfg.NetworkKey == "" {
			c.logf("无服务器模式入网（公共网络）：虚拟 IP=%s", c.myIP)
		} else {
			c.logf("无服务器模式入网（私有网络）：虚拟 IP=%s，网络密钥=%s", c.myIP, cfg.NetworkKey)
		}
	} else {
		c.netmapCli = netmapclient.NewClient(cfg.CTLURL, c.peerID)
		if cfg.InviteCode == "" {
			if err = c.createGroup(ctx); err != nil {
				_ = node.Close()
				return nil, err
			}
		} else {
			if err = c.joinGroup(ctx); err != nil {
				_ = node.Close()
				return nil, err
			}
		}
	}

	// 3. 隧道服务（standalone 用本地发现实现 NetMap/中继候选接口）。
	if cfg.Standalone {
		c.tunnelSvc = tunnel.New(node, disc, disc)
	} else {
		c.tunnelSvc = tunnel.New(node, c.netmapCli, c.peerSource)
	}
	// 4. 统一入向防火墙（默认 deny-all）：管控 PortFWD / TUN 入向 /
	// OnStream 应用流三类暴露面 + 局域网映射表。
	c.fw = firewall.New()
	c.fw.Set(cfg.FirewallMode, cfg.FirewallRules)
	c.forwards = append([]LANForward(nil), cfg.LANForwards...)
	c.statePath = cfg.StateFile
	c.loadState()
	c.enablePortFWD()
	// 4.5 TUN 虚拟网卡（IP 层互通，可选）：放在防火墙初始化之后，
	// Router 的入向 IP 包判定复用同一套防火墙规则。
	if cfg.Tun {
		c.startTUN(ctx)
	}
	// 5. 内置 Web 控制台。
	if err = c.startConsole(); err != nil {
		_ = node.Close()
		return nil, err
	}
	return c, nil
}

// Info 返回节点身份信息。
func (c *Client) Info() Info {
	return Info{
		PeerID: c.peerID, GroupID: c.groupID, Group: c.group,
		VirtualIP:   c.myIP,
		Name:        c.cfg.Name,
		VirtualHost: c.selfHostname(),
		Created:     c.created,
		InviteCode:  c.cfg.InviteCode,
	}
}

// selfHostname 本节点的虚拟地址：把自己并入当前成员表后按同一规则推导，
// 保证与远端节点计算出的本节点地址一致（组内重名时自动带 -xxxx 后缀）。
func (c *Client) selfHostname() string {
	if c.cfg.Name == "" {
		return ""
	}
	refs := []serverless.MemberRef{{PeerID: c.peerID, Name: c.cfg.Name, VirtualIP: c.myIP}}
	if c.disc != nil {
		for _, m := range c.disc.Peers() {
			refs = append(refs, serverless.MemberRef{PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP})
		}
	} else if c.netmapCli != nil {
		for _, m := range c.netmapCli.Current().Members {
			refs = append(refs, serverless.MemberRef{PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP})
		}
	}
	hosts := serverless.Hostnames(refs)
	if label := hosts[c.peerID]; label != "" {
		return label + "." + serverless.VirtualDomain
	}
	return ""
}

// OnStream 注册入向流处理器（协议 Tunnel）。可注册多个，按序调用。
//
// 注意：TUN 虚拟网卡创建成功时，/pvn/tunnel/1.0.0 协议已被 SDK 接管为
// IP 层数据面（入向流写回 TUN 网卡，承载 ping / TCP / UDP 等全部流量），
// 本方法注册的处理器在该协议上不再被调用——否则会与 TUN 数据面互相抢流
// （历史上 pvn-node 的 echo handler 曾因此吞掉全部入向 IP 包导致
// ping 双向 100% 丢包）。应用层入向流请用 DialProtocol 配自定义协议。
// TUN 未启用或创建失败（如 Windows 非管理员）时，行为与从前一致。
func (c *Client) OnStream(handler Handler) {
	c.handlers = append(c.handlers, handler)
	if c.tunActive() {
		c.logf("OnStream 已忽略：TUN 虚拟网卡已接管 /pvn/tunnel/1.0.0（IP 数据面），应用流处理器不再作用于该协议")
		return
	}
	if len(c.handlers) == 1 {
		c.node.SetStreamHandler(protocol.Tunnel, c.handleInbound)
	}
}

// tunActive TUN 数据面是否已激活（创建成功且已接管 Tunnel 协议）。
func (c *Client) tunActive() bool {
	c.tunMu.Lock()
	defer c.tunMu.Unlock()
	return c.tunRouter != nil
}

// resolveVirtualIP 虚拟地址解析：目标可以是虚拟 IP、
// <成员名>.lanet 完整虚拟地址、短名或原始成员名（详见 serverless.ResolveTarget）。
// 解析基于实时成员表——成员重启后虚拟 IP 变化不影响按名字连入。
func (c *Client) resolveVirtualIP(target string) (string, error) {
	m, err := c.resolveMember(target)
	if err != nil {
		return "", err
	}
	return m.VirtualIP, nil
}

// resolveMember 把连接目标解析为成员（serverless.ResolveTarget 的两种入网模式适配）。
func (c *Client) resolveMember(target string) (serverless.MemberRef, error) {
	var members []serverless.MemberRef
	if c.disc != nil {
		for _, m := range c.disc.Peers() {
			members = append(members, serverless.MemberRef{PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP})
		}
	} else if c.netmapCli != nil {
		for _, m := range c.netmapCli.Current().Members {
			members = append(members, serverless.MemberRef{PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP})
		}
	}
	return serverless.ResolveTarget(members, target)
}

// Dial 按虚拟 IP 打开到对端的隧道流（直连优先，中继兜底）。
// 目标也支持虚拟域名（成员名称，见 resolveVirtualIP）。
// 返回流与是否经中继。用完必须 Close；发送完毕建议 CloseWrite。
func (c *Client) Dial(ctx context.Context, virtualIP string) (Stream, bool, error) {
	virtualIP, err := c.resolveVirtualIP(virtualIP)
	if err != nil {
		return nil, false, err
	}
	raw, viaRelay, err := c.tunnelSvc.OpenStreamToVirtualIP(ctx, virtualIP)
	if err != nil {
		return nil, false, err
	}
	return streamAdapter{Stream: raw, viaRelay: viaRelay}, viaRelay, nil
}

// DialProtocol 同 Dial，但指定应用层协议 ID（对端需已注册该协议处理器）。
// 用于自定义应用协议（如网关、端口转发之外的扩展场景）。目标支持虚拟域名。
func (c *Client) DialProtocol(ctx context.Context, virtualIP string, protoID string) (Stream, bool, error) {
	virtualIP, err := c.resolveVirtualIP(virtualIP)
	if err != nil {
		return nil, false, err
	}
	raw, viaRelay, err := c.tunnelSvc.OpenStreamToVirtualIPProtocol(ctx, virtualIP, libprotocol.ID(protoID))
	if err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, false, fmt.Errorf("lanet: 拨号 %s 返回空流（内部错误）", virtualIP)
	}
	return streamAdapter{Stream: raw, viaRelay: viaRelay}, viaRelay, nil
}

// LastPathUsed 返回到对端最近一次链路类型：direct / relay / offline / unknown。
func (c *Client) LastPathUsed(peerID string) string { return c.tunnelSvc.LastPathUsed(peerID) }

// NetMap 当前群组成员目录快照。Standalone 模式返回本地发现的成员表。
func (c *Client) NetMap() netmapclient.Snapshot {
	if c.disc != nil {
		members := make([]netmapclient.Member, 0)
		for _, m := range c.disc.Peers() {
			members = append(members, netmapclient.Member{
				PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP, Addrs: m.Addrs,
				Hostname: m.Hostname, FirstSeen: m.FirstSeen, LastSeen: m.LastSeen,
				Version: m.Version, Platform: m.Platform,
			})
		}
		return netmapclient.Snapshot{
			GroupID: c.groupID, GroupName: c.group,
			CIDR: "10.7.0.0/16", Members: members,
		}
	}
	return c.netmapCli.Current()
}

// Host 暴露底层 libp2p Host（进阶用法：自定义协议等）。
func (c *Client) Host() host.Host { return c.node }

// startTUN 创建 TUN 虚拟网卡并启动 IP 包路由（IP 层互通：ping / 任意 TCP/UDP 直达虚拟 IP）。
// 创建或配置失败时仅降级为应用层模式并记日志，不阻断入网（常见原因：
// Windows 非管理员运行、Linux 无 /dev/net/tun 或 CAP_NET_ADMIN）。
func (c *Client) startTUN(ctx context.Context) {
	name := c.cfg.TunName
	if name == "" {
		name = "lanet"
	}
	device, err := tundevice.NewNative(name, 1400)
	if err != nil {
		c.logf("TUN 网卡创建失败（自动降级为仅应用层，虚拟 IP 不支持 ping；Windows 需管理员权限运行）: %v", err)
		return
	}
	if err = tundevice.ConfigureTUN(name, c.myIP, 24); err != nil {
		_ = device.Close()
		c.logf("TUN 网卡地址配置失败（自动降级为仅应用层）: %v", err)
		return
	}
	c.tunMu.Lock()
	c.tunDevice = device
	router := tundevice.New(device, c.tunnelSvc)
	router.SetFirewall(c.fw)
	c.tunRouter = router
	c.tunMu.Unlock()
	// 设备生命周期与 ctx 绑定：退出时关闭设备，Router.Run 随 Read 错误退出。
	go func() {
		<-ctx.Done()
		_ = device.Close()
	}()
	go router.Run(ctx)
	// 关键接线：TUN 已就绪时，/pvn/tunnel/1.0.0 入向流必须写回 TUN 网卡
	// （对端主动发来的 IP 包）。没有这行，本节点永远收不到对端的包
	// ——ping 双向 100% 丢包的核心根因（此前该协议被宿主的 echo handler
	// 吞掉，IP 包从未进入本机协议栈）。此时宿主的 OnStream 不再作用于
	// Tunnel 协议（见 OnStream 注释），应用流场景请改用 DialProtocol+自定义协议。
	c.node.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		router.ServeInboundStream(stream)
	})
	c.logf("TUN 网卡 %s 已就绪（虚拟 IP=%s）：组内成员可通过虚拟 IP 直接访问本机（ping/任意端口，入向受防火墙约束）", name, c.myIP)
}

// Run 阻塞运行周期任务，直到 ctx 取消。
//   - Standalone：周期 DHT 广播与发现（mDNS 持续运行）。
//   - 常规：刷新 NetMap / 通告地址 / 补充中继预约。
func (c *Client) Run(ctx context.Context) {
	if c.disc != nil {
		c.disc.Run(ctx)
		return
	}
	// 入网后先完成一次通告 + 中继预约（兜底链路关键步骤）。
	c.announce(ctx)
	if err := p2pkit.EnsureRelayReservation(ctx, c.node, c.peerSource.AutoRelayPeerSource(), 2); err != nil {
		c.logf("relay 预约暂未成功（将周期重试）: %v", err)
	}

	ticker := time.NewTicker(c.cfg.NetMapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logf("收到退出信号，正在关闭")
			return
		case <-ticker.C:
			if _, err := c.netmapCli.Refresh(ctx); err != nil {
				c.logf("刷新 NetMap: %v", err)
				continue
			}
			c.announce(ctx)
			// 预约有有效期，周期补充（已有时 Reserve 会刷新有效期）。
			if err := p2pkit.EnsureRelayReservation(ctx, c.node, c.peerSource.AutoRelayPeerSource(), 2); err != nil {
				c.logf("relay 预约补充失败（下个周期重试）: %v", err)
			}
		}
	}
}

// Close 关闭节点与底层连接。
func (c *Client) Close() error {
	c.tunMu.Lock()
	if c.tunDevice != nil {
		_ = c.tunDevice.Close()
		c.tunDevice = nil
	}
	c.tunMu.Unlock()
	if c.consoleSrv != nil {
		_ = c.consoleSrv.Close()
	}
	return c.node.Close()
}

// handleInbound 入向流分发到已注册的 Handler。
// 先过统一防火墙（协议维度：/pvn/tunnel/1.0.0，默认 deny-all 拒绝）。
func (c *Client) handleInbound(stream network.Stream) {
	srcIP := c.virtualIPByPeer(stream.Conn().RemotePeer().String())
	if !c.fw.AllowStream(srcIP, string(protocol.Tunnel)) {
		c.logf("onstream 拒绝：来源=%s（%s）协议=%s 不在放行规则内",
			srcIP, shortPeer(stream.Conn().RemotePeer().String()), protocol.Tunnel)
		_ = stream.Reset()
		return
	}
	viaRelay := hasCircuit(stream.Conn().RemoteMultiaddr())
	for _, h := range c.handlers {
		h(streamAdapter{Stream: stream, viaRelay: viaRelay})
	}
}

// announce 通告本节点可达地址。
func (c *Client) announce(ctx context.Context) {
	addrs := make([]string, 0, len(c.node.Addrs()))
	for _, addr := range c.node.Addrs() {
		addrs = append(addrs, addr.String())
	}
	if err := c.netmapCli.Announce(ctx, addrs); err != nil {
		c.logf("通告地址失败（不影响运行，将周期重试）: %v", err)
	}
}

// createGroup 创建群组（本节点成为群主）。
func (c *Client) createGroup(ctx context.Context) error {
	var resp struct {
		Group struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"group"`
		Creator struct {
			VirtualIP string `json:"virtual_ip"`
		} `json:"creator"`
		InviteCode string `json:"invite_code"`
	}
	payload := map[string]any{
		"peer_id": c.peerID, "name": c.cfg.Name, "os": c.cfg.OS, "group_name": c.cfg.GroupName,
	}
	if err := c.post(ctx, "/v1/groups/create", payload, &resp); err != nil {
		return fmt.Errorf("lanet: 创建群组: %w", err)
	}
	c.groupID, c.group, c.myIP = resp.Group.ID, resp.Group.Name, resp.Creator.VirtualIP
	c.cfg.InviteCode = resp.InviteCode
	c.created = true
	c.logf("群组 %s 已创建，虚拟 IP=%s，邀请码=%s", c.group, c.myIP, resp.InviteCode)
	return nil
}

// joinGroup 凭邀请码加入群组。
func (c *Client) joinGroup(ctx context.Context) error {
	var resp struct {
		Group struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"group"`
		Member struct {
			VirtualIP string `json:"virtual_ip"`
		} `json:"member"`
	}
	payload := map[string]any{
		"invite_code": c.cfg.InviteCode, "peer_id": c.peerID, "name": c.cfg.Name, "os": c.cfg.OS,
	}
	if err := c.post(ctx, "/v1/groups/join", payload, &resp); err != nil {
		return fmt.Errorf("lanet: 加入群组: %w", err)
	}
	c.groupID, c.group, c.myIP = resp.Group.ID, resp.Group.Name, resp.Member.VirtualIP
	c.logf("已加入群组 %s，虚拟 IP=%s", c.group, c.myIP)
	return nil
}

// post 调用控制面 JSON 接口（gf 标准响应包装）。
func (c *Client) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.CTLURL, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Code != 0 {
		return fmt.Errorf("code %d: %s", envelope.Code, envelope.Message)
	}
	if len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

// hasCircuit 判断地址是否经过 /p2p-circuit（即中继链路）。
func hasCircuit(address interface{ ValueForProtocol(int) (string, error) }) bool {
	if address == nil {
		return false
	}
	_, err := address.ValueForProtocol(290) // P_CIRCUIT
	return err == nil
}

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Quiet {
		return
	}
	log.Printf("[lanet-sdk] "+format, args...)
}
