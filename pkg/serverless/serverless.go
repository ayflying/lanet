// Package serverless 提供无控制面的群组成员发现：
//
//   - DHT（kad-dht，ModeAutoServer）：跨网段发现。每个节点把
//     「本网络 rendezvous key」作为 provider 记录发布到 DHT 网络，
//     同网络成员通过 FindProviders 互相找到。
//     key 由网络密钥（NetworkKey）派生，不知道密钥就无法定位网络（弱隐私边界）。
//   - 双 DHT（私有优先 + 公共兜底）：私有网络（NetworkKey 非空）在同一张
//     Host 上同时运行两张 DHT——私有 DHT 使用独立协议前缀（/lanet/kad/1.0.0）
//     与公共 /ipfs DHT 完全隔离，只有本网络节点参与，路由表小、发现快；
//     公共 DHT 作为兜底（私有引导节点全不可达时仍能经公共网络找到成员）。
//     发现顺序私有优先；同群成员一经确认即注入私有 DHT 路由表（互为种子）。
//   - mDNS：局域网零配置发现（service tag 派生自网络密钥，同网络才互见）。
//   - 节点即服务端：每个节点默认运行 relay service 与 DHT server 模式，
//     公网可达的成员自然成为网络内的引导与中继节点。
//
// 发现到同网络成员后主动建连并交换信息（/lanet/info/1.0.0），
// 本地维护成员表；对外实现 tunnel.GroupNetMap（按虚拟 IP 解析）
// 与 tunnel.RelaySource（中继候选），SDK 的 Dial/OnStream 语义不变。
package serverless

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ipfs/go-cid"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
	mh "github.com/multiformats/go-multihash"
)

// 公共 DHT 引导节点（libp2p 官方公共引导列表，dnsaddr 自动解析出节点列表）。
// 国内网络可达性需实测；不可达时可通过 Config.Bootstrap 指定任意
// 已在网成员的 multiaddr（每台节点都是潜在种子）。
const DefaultBootstrap = "/dnsaddr/bootstrap.libp2p.io"

// PrivateDHTPrefix 私有 DHT 的协议前缀。私有 DHT 的协议为
// /lanet/kad/1.0.0（ProtocolPrefix 补全），与公共 /ipfs/kad/1.0.0
// 完全隔离：只有本网络节点互相参与路由与 provider 记录。
const PrivateDHTPrefix = "/lanet"

// 默认成员回收时限与下限。过小会误删 NAT 重连慢的成员。
const (
	DefaultMemberTTL = 10 * time.Minute
	MinMemberTTL     = 2 * time.Minute
)

// ProtocolInfo 节点信息交换协议（建连后校验同群 + 交换名称）。
const ProtocolInfo = "/lanet/info/1.0.0"

// ErrNotApproved 对端拒绝本次握手：它不是不理你，而是明确告诉你
// 「本机尚未同意你的连接申请」。上层据此给出「等待对方同意」的提示，
// 而不是让用户面对一个看不懂的 EOF 错误。
//
// 注意：拒绝响应里不含名称、版本、主机名等任何身份信息，只有群指纹与
// 这个标记——未审批节点仍拿不到任何成员信息（完全隔离语义不变）。
var ErrNotApproved = errors.New("serverless: 对端尚未同意本次连接（需对方在控制台同意）")

// DefaultPublicDHTTimeout 公共 DHT 临时引导的默认最长运行时长。
// 超时无论是否发现同群成员都自动退出（省流量；重启可重新引导）。
const DefaultPublicDHTTimeout = 10 * time.Minute

// Config 发现服务配置。
type Config struct {
	// NetworkKey 网络密钥：相同密钥的节点组成同一张 P2P 网络。
	// 留空 = 按节点身份自动派生「本机专属默认网络」（见 DeriveDefaultNetworkKey），
	// 即开箱即用但默认自成一张网；要与他人互通须显式设置相同密钥。
	// 填写任意非空字符串 = 指定网络，只有持相同密钥的节点能互相发现与连接。
	NetworkKey string
	// LegacyDefaultKey 标记「本节点使用历史公共网络密钥（lanet/public）」。
	// 仅用于老网络迁移兼容：历史版本留空即等于此固定值，为避免老节点
	// 升级后突然脱离原网络，配置解析层识别到「显式配置了旧默认值」或
	// 「由旧配置迁移而来」时置为 true，此时留空按 PublicNetworkKey 处理。
	LegacyDefaultKey bool
	// Channel 分发渠道：参与群组密钥派生，用于把不同分发途径的程序隔离在
	// 不同网络（即使 NetworkKey 相同也不互通）。官方程序用 ChannelOfficial，
	// SDK 构建默认 ChannelSDK。留空 = ChannelOfficial（历史派生）。
	Channel string
	// Name 本节点名称（随 info 协议交换给同网络成员）。
	Name string
	// Bootstrap DHT 引导节点 multiaddr 列表。
	// 私有网络（NetworkKey 非空）下作为「私有 DHT 种子」：填任意已在网
	// 成员的 multiaddr 即可加速入网；填 DefaultBootstrap 会被识别为公共
	// 引导（不作为私有种子）。公共网络下即公共 DHT 引导列表。
	Bootstrap []string
	// EnablePublicFallback 启用公共 DHT 兜底（默认关闭）。默认关闭的
	// 原因：公共 DHT 是全公网共享网络，作为 server 节点要持续应答全网
	// 随机查询（实测空载上行 ~4MB/分钟），且国内连 bootstrap.libp2p.io
	// 不稳定，反复重试也会产生无效流量。开启后公共 DHT 作为「临时引导」：
	// 找到第一个「自己人」即自动退出（见 maybeRetirePublicDHT），最长
	// 运行 PublicDHTTimeout（默认 10 分钟）——超时即使没连上也自动退出。
	// 关闭时跨网冷启动需把已在网成员 multiaddr 配置为引导种子。
	EnablePublicFallback bool
	// PublicDHTTimeout 公共 DHT 临时引导的最长运行时长（默认 10 分钟，
	// 0 = 用默认）。超时无论是否发现同群成员都自动退出公共 DHT，避免
	// 开关忘关导致公共 DHT 长期挂载消耗流量。
	PublicDHTTimeout time.Duration
	// EnableMDNS 启用局域网 mDNS 自动发现。
	EnableMDNS bool
	// Interval 广播/发现周期，默认 30s。
	Interval time.Duration
	// MemberTTL 成员不活跃回收时限：超过该时长无任何真实通讯
	// （连通确认 / 信息交换 / 入向握手）的成员将从成员表移除，
	// 其虚拟 IP 派生占用随之释放。默认 10 分钟。
	// 注意：仅发现到（DHT 陈旧 provider 记录）不会续命——必须真正通讯过。
	// 最小值 2 分钟（过小会误删 NAT 重连慢的成员）；0 = 用默认。
	MemberTTL time.Duration
	// Version 本节点程序版本（info 协议交换给同网络成员，
	// 供 P2P 自更新统计全网版本分布）。空 = 不上报。
	Version string
	// Platform 本节点运行平台（GOOS/GOARCH，如 windows/amd64）。
	Platform string
	// OSHostname 本节点操作系统主机名（info 协议交换，供成员识别设备）。空 = 不上报。
	OSHostname string
	// LocalIPs 本节点非回环网卡 IP 列表（info 协议交换，供成员识别设备网段）。空 = 不上报。
	LocalIPs []string
	// Quiet 为 true 时不打日志。
	Quiet bool

	// ---- 连接审批（加好友式）与流量控制回调 ----
	// 均为可选：nil 时退化为「无审批」的历史行为（任何同群节点都直接互连），
	// 保证不传回调的既有调用方（测试、SDK 简单用法）行为不变。

	// IsTrusted 查询某个对端节点是否已被审批信任。
	// 返回 false 表示该节点是陌生节点：不接受其 info 握手、不写入成员表、
	// 不与其建立任何 overlay 关系（完全隔离），仅通过 OnPending 上报待审批。
	// nil = 不启用审批，全部放行。
	IsTrusted func(peerID string) bool
	// AutoAccept 陌生节点申请连接时的自动审批钩子（无人值守中央服务器用）。
	// 返回 true 表示本端已自动信任该节点，本次握手按已信任处理。
	// 仅当 IsTrusted 返回 false 时才会被调用；nil = 不自动审批。
	AutoAccept func(peerID string, addrs []string, name string) bool
	// OnPending 陌生节点申请连接（未信任且未自动放行）时上报，
	// 由上层持久化到待审批列表并在控制台展示。可能被并发调用。
	OnPending func(peerID string, addrs []string, name string)
	// HasKnownPeers 地址簿中是否有已知节点。返回 false（首次启动、地址簿为空）
	// 时跳过主动 DHT 查找（FindProviders），只保留自身 Provide 广播——
	// 让别人能找到我，但自己不产生查询流量。
	// nil = 不启用该优化（按历史行为每轮查找）。
	HasKnownPeers func() bool
}

// Member 成员表中的一项。
type Member struct {
	PeerID    string    `json:"peer_id"`
	Name      string    `json:"name"`
	VirtualIP string    `json:"virtual_ip"`
	Addrs     []string  `json:"addrs"`
	Source    string    `json:"source"`     // dht / dht-private / mdns
	FirstSeen time.Time `json:"first_seen"` // 首次发现时间（即上线时间）
	LastSeen  time.Time `json:"last_seen"`  // 最近一次出现（发现/握手/入向信息）时间
	// Hostname 本成员的虚拟主机名（含 .lanet 后缀，如 yunloli.lanet）。
	// 由当前成员表确定性推导，重名自动追加后缀。
	Hostname string `json:"hostname,omitempty"`
	// Version 成员程序版本（info 协议交换；旧节点为空）。
	Version string `json:"version,omitempty"`
	// Platform 成员运行平台 GOOS/GOARCH。
	Platform string `json:"platform,omitempty"`
	// OSHostname 成员的操作系统主机名（info 协议交换；旧节点为空）。
	// 与 Name（节点配置名）互相独立，用于识别「这是哪台机器」。
	OSHostname string `json:"os_hostname,omitempty"`
	// LocalIPs 成员本机所有非回环网卡 IP（含掩码位数，如 192.168.50.100/24）。
	// info 协议交换，用于在控制台上识别设备归属网段；旧节点为空。
	LocalIPs []string `json:"local_ips,omitempty"`
}

// Discovered 新成员被发现（尚未连通也会触发；连通并确认同群后 Name 有效）。
type Discovered func(Member)

// Discovery 无服务器成员发现服务。
type Discovery struct {
	host     host.Host
	cfg      Config
	groupKey []byte
	selfIP   string

	dhtPrivate *kaddht.IpfsDHT // 私有 DHT（/lanet 前缀；仅私有网络非 nil）
	dhtPublic  *kaddht.IpfsDHT // 公共 DHT（/ipfs 前缀；兜底与公共网络）
	mdnsSvc    mdns.Service

	memberTTL time.Duration // 成员不活跃回收时限（Config.MemberTTL 归一化后）

	mu      sync.RWMutex
	members map[string]*Member // peerID -> member

	publicRetired      bool      // 公共 DHT 已退出（私有 DHT 就绪后省流量）
	publicRetireReason string    // 退出原因（连上同群成员 / 超时），控制台展示用
	publicStartedAt    time.Time // 公共 DHT 开启时刻（剩余时长展示用）

	onDiscovered []Discovered
}

// New 创建并启动发现服务：连引导节点、初始化 DHT、启动 mDNS。
func New(ctx context.Context, h host.Host, cfg Config) (*Discovery, error) {
	if cfg.NetworkKey == "" {
		// 密钥留空 = 按节点身份派生「本机专属默认网络」：
		//   - 每台机器开箱即用，但默认自成一张网，不会与所有零配置节点同网
		//     （避免超大网络带来的发现流量与成员表膨胀）；
		//   - 要与他人互通，必须显式设置相同的 NetworkKey（或互发连接种子）。
		// 迁移说明：历史版本留空派生的是固定值 PublicNetworkKey（lanet/public）。
		// 为不打断老网络，若本节点显式配置了旧的公共网络密钥（见 cfg 解析层
		// 的 LegacyDefaultKey 标记），此处保持旧值不变。
		if cfg.LegacyDefaultKey {
			cfg.NetworkKey = PublicNetworkKey
		} else {
			cfg.NetworkKey = DeriveDefaultNetworkKey(h.ID().String())
		}
	}
	// 关闭公共兜底时，公共引导地址不参与任何连接——连 Start 阶段的
	// 引导 Connect 也不去碰 bootstrap.libp2p.io（国内解析/连接常超时，
	// 白白产生重试流量）。
	if !cfg.EnablePublicFallback {
		kept := make([]string, 0, len(cfg.Bootstrap))
		for _, b := range cfg.Bootstrap {
			if b != DefaultBootstrap {
				kept = append(kept, b)
			}
		}
		cfg.Bootstrap = kept
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.PublicDHTTimeout <= 0 {
		cfg.PublicDHTTimeout = DefaultPublicDHTTimeout
	}
	switch {
	case cfg.MemberTTL <= 0:
		cfg.MemberTTL = DefaultMemberTTL
	case cfg.MemberTTL < MinMemberTTL:
		cfg.MemberTTL = MinMemberTTL
	}
	d := &Discovery{
		host:      h,
		cfg:       cfg,
		groupKey:  GroupKey(cfg.Channel, cfg.NetworkKey), // 空密钥按公共网络处理
		memberTTL: cfg.MemberTTL,
		members:   make(map[string]*Member),
	}
	d.selfIP = DeriveVirtualIP(d.groupKey, h.ID().String())

	// 1. DHT：每台节点都是 server（客户端即服务端）。
	//    双 DHT：私有（/lanet 前缀，只有本网络节点）优先发现，
	//    公共（/ipfs 前缀）兜底——负责跨网冷启动时找到第一个「自己人」。
	//    密钥留空的节点已在上方归一化为公共网络密钥，同样走此路径。
	privSeeds := make([]string, 0, len(cfg.Bootstrap))
	for _, b := range cfg.Bootstrap {
		if b != DefaultBootstrap { // 公共引导地址不作为私有种子
			privSeeds = append(privSeeds, b)
		}
	}
	privParsed, err := parseBootstrap(ctx, privSeeds)
	if err != nil {
		return nil, err
	}
	d.dhtPrivate, err = kaddht.New(h,
		kaddht.Mode(kaddht.ModeAutoServer),
		kaddht.BootstrapPeers(privParsed...),
		kaddht.ProtocolPrefix(PrivateDHTPrefix),
	)
	if err != nil {
		return nil, fmt.Errorf("serverless: init private dht: %w", err)
	}
	if cfg.EnablePublicFallback {
		pub, perr := newPublicDHT(ctx, h, d.logf)
		if perr != nil {
			return nil, fmt.Errorf("serverless: init public dht: %w", perr)
		}
		d.dhtPublic = pub
	}

	// 2. mDNS（可选）：NewMdnsService 创建即启动。
	if cfg.EnableMDNS {
		tag := MdnsTag(d.groupKey)
		d.mdnsSvc = mdns.NewMdnsService(h, tag, &mdnsNotifee{d: d})
	}
	return d, nil
}

// newPublicDHT 创建公共 IPFS DHT 实例（/ipfs 协议前缀）。
// 公共引导地址解析失败不致命：实例照建，后续每轮广播会重试自举。
func newPublicDHT(ctx context.Context, h host.Host, logf func(string, ...any)) (*kaddht.IpfsDHT, error) {
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	pubParsed, perr := parseBootstrap(pubCtx, []string{DefaultBootstrap})
	cancel()
	if perr != nil && logf != nil {
		logf("公共引导解析失败，公共兜底暂不可用（下轮重试广播）: %v", perr)
	}
	return kaddht.New(h,
		kaddht.Mode(kaddht.ModeAutoServer),
		kaddht.BootstrapPeers(pubParsed...),
	)
}

// Start 完成引导连接、DHT 自举与信息协议注册。非阻塞部分尽力而为。
func (d *Discovery) Start(ctx context.Context) error {
	for _, b := range parseBootstrapQuiet(ctx, d.cfg.Bootstrap) {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := d.host.Connect(dialCtx, b); err != nil {
			d.logf("引导节点连接失败（不影响运行）: %v", err)
		} else {
			d.logf("已连接引导节点 %s", b.ID.ShortString())
		}
		cancel()
	}
	if d.dhtPrivate != nil {
		if err := d.dhtPrivate.Bootstrap(ctx); err != nil {
			d.logf("私有 DHT 自举未完成（周期重试）: %v", err)
		}
	}
	if d.dhtPublic != nil {
		if err := d.dhtPublic.Bootstrap(ctx); err != nil {
			d.logf("公共 DHT 自举未完成（周期重试）: %v", err)
		}
	}
	if d.dhtPrivate != nil && d.dhtPublic != nil {
		d.logf("双 DHT 模式：私有发现优先，公共 DHT 临时引导（找到同群成员即退出，最长 %s）",
			d.cfg.PublicDHTTimeout)
	} else {
		d.logf("公共 DHT 兜底已关闭（私有 DHT + mDNS 发现；跨网冷启动需配置成员引导种子）")
	}
	// 公共 DHT 临时引导超时：到点无论是否发现同群成员都自动退出。
	// 与「找到即退」（maybeRetirePublicDHT）双保险，防止开关忘关导致
	// 公共 DHT 长期挂载消耗流量。
	if d.dhtPublic != nil {
		d.mu.Lock()
		d.publicStartedAt = time.Now()
		pub := d.dhtPublic
		d.mu.Unlock()
		d.armPublicDHTTimeout(ctx, pub, d.cfg.PublicDHTTimeout)
	}
	d.host.SetStreamHandler(ProtocolInfo, d.handleInfo)
	return nil
}

// armPublicDHTTimeout 为某个公共 DHT 实例装上超时退出计时器：到点若该实例
// 仍是当前实例（未被重开替换）则自动退出。重开时旧计时器因实例不匹配而失效。
func (d *Discovery) armPublicDHTTimeout(ctx context.Context, pub *kaddht.IpfsDHT, timeout time.Duration) {
	if pub == nil {
		return
	}
	go func() {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.mu.RLock()
			same := d.dhtPublic == pub
			d.mu.RUnlock()
			if same {
				d.retirePublicDHT(fmt.Sprintf("超时未发现同群成员（引导时限 %s）", timeout))
			}
		}
	}()
}

// EnablePublicDHTRuntime 运行时开启公共 DHT 临时引导（控制台开关，立即生效）。
// 已开启则无操作。开启后同样受 PublicDHTTimeout 约束：连上同群成员立即退出，
// 超时未连上也退出。该操作不改动配置文件——下次启动是否自动开启由配置
// EnablePublicFallback 决定。
func (d *Discovery) EnablePublicDHTRuntime(ctx context.Context) error {
	d.mu.RLock()
	active := d.dhtPublic != nil
	d.mu.RUnlock()
	if active {
		return nil
	}
	pub, err := newPublicDHT(ctx, d.host, d.logf)
	if err != nil {
		return fmt.Errorf("serverless: enable public dht: %w", err)
	}
	d.mu.Lock()
	d.dhtPublic = pub
	d.publicRetired = false
	d.publicRetireReason = ""
	d.publicStartedAt = time.Now()
	timeout := d.cfg.PublicDHTTimeout
	d.mu.Unlock()
	if err := pub.Bootstrap(ctx); err != nil {
		d.logf("公共 DHT 自举未完成（周期重试）: %v", err)
	}
	d.logf("已开启公共 DHT 临时引导（最长 %s，连上同群成员立即退出）", timeout)
	d.armPublicDHTTimeout(ctx, pub, timeout)
	return nil
}

// DisablePublicDHTRuntime 运行时关闭公共 DHT（控制台开关手动关闭）。
func (d *Discovery) DisablePublicDHTRuntime(reason string) {
	d.retirePublicDHT(reason)
}

// DialSeed 运行时按连接种子（成员 multiaddr，需带 /p2p/<ID>）直接拨号并
// 建立成员关系：连通后经 info 协议确认同群、注入私有 DHT 路由表（互为
// 种子），从而完全不经公共 DHT 完成跨网入网。返回连通的对端节点 ID。
// 支持一次传入多个地址（同一成员的多个候选地址逐个尝试）。
func (d *Discovery) DialSeed(ctx context.Context, addrs []string) (string, error) {
	infos, err := parseBootstrap(ctx, addrs)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", fmt.Errorf("连接种子非法：需要形如 /ip4/1.2.3.4/tcp/4001/p2p/12D3Koo... 的完整地址")
	}
	var lastErr error
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, ai := range infos {
		if ai.ID == d.host.ID() {
			return "", fmt.Errorf("连接种子指向本机，无需连接")
		}
		d.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.PermanentAddrTTL)
		if err := d.host.Connect(dialCtx, ai); err != nil {
			lastErr = err
			continue
		}
		// 建连后立刻走 info 协议确认同群（拿到名称、校验渠道与密钥）。
		if err := d.connectAndIdentify(ai.ID); err != nil {
			// 对方明确拒绝（未审批）时原样上抛，保留可识别语义。
			if errors.Is(err, ErrNotApproved) {
				lastErr = err
				continue
			}
			lastErr = fmt.Errorf("已连通但同群校验失败（网络密钥或渠道不一致）: %w", err)
			continue
		}
		d.logf("已按连接种子直连成员 %s（未使用公共 DHT）", ai.ID.ShortString())
		return ai.ID.String(), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("连接失败")
	}
	return "", fmt.Errorf("连接种子拨号失败: %w", lastErr)
}

// ---- 连接审批（加好友式）辅助 ----

// trustPolicy 判定对端是否可建立成员关系。返回 trusted=true 表示放行。
// 未放行时（依赖 OnPending 非 nil 且实际调用了回调）返回 pending=true。
//
// 语义：
//   - 未配置 IsTrusted（nil）= 不启用审批，一律放行（历史行为）。
//   - 已信任 → 放行。
//   - 陌生 → 先问 AutoAccept（无人值守自动同意），命中则放行；
//     未命中则上报 OnPending 并拒绝（完全隔离）。
func (d *Discovery) trustPolicy(peerID string, addrs []string, name string) (trusted, pending bool) {
	if d.cfg.IsTrusted == nil {
		return true, false
	}
	if d.cfg.IsTrusted(peerID) {
		return true, false
	}
	if d.cfg.AutoAccept != nil && d.cfg.AutoAccept(peerID, addrs, name) {
		d.logf("已自动同意陌生节点 %s 的连接申请（auto_accept 开启）", shortID(peerID))
		return true, false
	}
	if d.cfg.OnPending != nil {
		d.cfg.OnPending(peerID, addrs, name)
		return false, true
	}
	return false, false
}

// knownPeers 地址簿是否有已知节点（决定是否值得发起 DHT 查找）。
// HasKnownPeers 为 nil 时返回 true（保持历史行为：每轮都查找）。
func (d *Discovery) knownPeers() bool {
	if d.cfg.HasKnownPeers == nil {
		return true
	}
	return d.cfg.HasKnownPeers()
}

func shortID(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

// SelfVirtualIP 本节点在无服务器模式下的虚拟 IP。
func (d *Discovery) SelfVirtualIP() string { return d.selfIP }

// FindPeer 按节点 ID 在本群内查找对端地址（仅私有 DHT，绝不出公网）。
//
// 查找顺序由调用方负责（本地地址簿优先）。本方法只做「私有 DHT 查找」这一
// 步：向私有 DHT 的 provider 记录求解。私钥网络下 provider key 由群密钥
// 派生，天然实现「仅同密钥内可查找」——密钥不同的节点根本不在同一张
// provider 表里，即使拿到对方完整 ID 也查不到位址。
//
// 找不到时返回明确错误，调用方据此提示用户核对 ID 与网络密钥。
func (d *Discovery) FindPeer(ctx context.Context, peerID string) (peer.AddrInfo, error) {
	id, err := peer.Decode(peerID)
	if err != nil {
		return peer.AddrInfo{}, fmt.Errorf("节点 ID 格式非法: %w", err)
	}
	if id == d.host.ID() {
		return peer.AddrInfo{}, fmt.Errorf("这就是本机节点 ID，无需连接")
	}
	if d.dhtPrivate == nil {
		return peer.AddrInfo{}, fmt.Errorf("私有 DHT 未就绪")
	}
	// 先看 peerstore 是否已有可用地址（此前连过、或对端主动握手留下）。
	if addrs := p2pkit.FilterUnderlayAddrs(d.host.Peerstore().Addrs(id)); len(addrs) > 0 {
		return peer.AddrInfo{ID: id, Addrs: addrs}, nil
	}
	findCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	pi, err := d.dhtPrivate.FindPeer(findCtx, id)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	pi.Addrs = p2pkit.FilterUnderlayAddrs(pi.Addrs)
	if len(pi.Addrs) == 0 {
		return peer.AddrInfo{}, fmt.Errorf("查到节点 %s 但无可用地址", shortID(peerID))
	}
	return pi, nil
}

// RequestConnect 按节点 ID 发起连接申请：先做信任判定，已信任/已自动放行
// 则直接建连并交换信息；陌生节点记入待审批列表由用户确认。
// 返回 (对端节点ID, error)。error 可能表示「已提交申请，等待对方同意」。
func (d *Discovery) RequestConnect(ctx context.Context, ai peer.AddrInfo) (string, error) {
	if ai.ID == d.host.ID() {
		return "", fmt.Errorf("这就是本机节点 ID，无需连接")
	}
	addrs := p2pkit.FilterUnderlayAddrs(ai.Addrs)
	if len(addrs) > 0 {
		d.host.Peerstore().AddAddrs(ai.ID, addrs, peerstore.PermanentAddrTTL)
	}
	trusted, pending := d.trustPolicy(ai.ID.String(), toStrings(addrs), "")
	if !trusted {
		if pending {
			return ai.ID.String(), fmt.Errorf("已向 %s 提交连接申请，等待对方同意后即可连通", shortID(ai.ID.String()))
		}
		return ai.ID.String(), fmt.Errorf("对方节点未被信任，无法连接")
	}
	// 已信任：按普通成员流程建连 + info 交换（失败即返回真实原因）。
	d.mu.Lock()
	if _, ok := d.members[ai.ID.String()]; !ok {
		d.members[ai.ID.String()] = &Member{
			PeerID:    ai.ID.String(),
			VirtualIP: DeriveVirtualIP(d.groupKey, ai.ID.String()),
			Source:    "manual",
			Addrs:     toStrings(addrs),
			FirstSeen: time.Now(),
			LastSeen:  time.Now(),
		}
	}
	d.mu.Unlock()
	if err := d.connectAndIdentify(ai.ID); err != nil {
		return ai.ID.String(), err
	}
	return ai.ID.String(), nil
}

// GroupKey 本群的群组密钥（由邀请码派生）。
func (d *Discovery) GroupKey() []byte { return d.groupKey }

// OnDiscovered 注册新成员回调。
func (d *Discovery) OnDiscovered(cb Discovered) { d.onDiscovered = append(d.onDiscovered, cb) }

// Peers 当前成员表快照（不含自身）。Hostname 按当前成员表推导。
func (d *Discovery) Peers() []Member {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Member, 0, len(d.members))
	refs := make([]MemberRef, 0, len(d.members))
	for _, m := range d.members {
		out = append(out, *m)
		refs = append(refs, MemberRef{PeerID: m.PeerID, Name: m.Name, VirtualIP: m.VirtualIP})
	}
	hosts := Hostnames(refs)
	for i := range out {
		if label := hosts[out[i].PeerID]; label != "" {
			out[i].Hostname = label + "." + VirtualDomain
		}
	}
	return out
}

// Run 阻塞运行周期广播与发现，直到 ctx 取消。
// 每轮结束执行一次成员回收：超期无真实通讯的成员移出成员表，
// 其虚拟 IP 派生占用随之释放（Roadmap：虚拟 IP 成员下线回收）。
func (d *Discovery) Run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	d.advertiseAndDiscover(ctx)
	d.reapExpired()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.advertiseAndDiscover(ctx)
			d.reapExpired()
		}
	}
}

// reapExpired 清理超过 memberTTL 无真实通讯的成员。
// 被移除的成员若仍实际在线，下一轮发现会重新入表（FirstSeen 重置）。
func (d *Discovery) reapExpired() {
	cutoff := time.Now().Add(-d.memberTTL)
	d.mu.Lock()
	var removed []Member
	for id, m := range d.members {
		if m.LastSeen.Before(cutoff) {
			removed = append(removed, *m)
			delete(d.members, id)
		}
	}
	d.mu.Unlock()
	if len(removed) > 0 && !d.cfg.Quiet {
		names := make([]string, 0, len(removed))
		for _, m := range removed {
			label := m.Name
			if label == "" {
				label = m.PeerID
				if len(label) > 10 {
					label = label[:10] + "…"
				}
			}
			names = append(names, fmt.Sprintf("%s(%s)", label, m.VirtualIP))
		}
		d.logf("成员回收：%s 超过 %s 无活跃通讯，已移出成员表", strings.Join(names, ", "), d.memberTTL)
	}
}

// advertiseAndDiscover 一轮：广播自身 + 查找同群成员 + 尝试建连。
// 私有 DHT 与公共 DHT 并行工作：私有命中快，公共兜底（跨网冷启动）。
func (d *Discovery) advertiseAndDiscover(ctx context.Context) {
	var wg sync.WaitGroup
	if d.dhtPrivate != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.dhtRound(ctx, d.dhtPrivate, "dht-private", 15*time.Second, 8*time.Second)
		}()
	}
	if d.dhtPublic != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.dhtRound(ctx, d.dhtPublic, "dht", 20*time.Second, 25*time.Second)
		}()
	}
	wg.Wait()
}

// dhtRound 单张 DHT 的一轮：把自身发布为群的 provider（TTL 由 DHT 管理，
// 周期刷新），再查找同群 provider 列表并逐个建连确认。
//
// 流量控制：若上层声明地址簿为空（首次启动、未添加过任何节点），则只做
// Provide 广播、不做 FindProviders 查找。理由——没有任何已知目标的查找
// 只会拿到一堆陌生 provider 记录，既浪费查询流量又必然被审批门拒掉；
// 此时保持「可被发现」即可（别人按 ID 找到我，我来应答）。一旦地址簿有
// 了内容（用户添加过节点），恢复每轮查找。
func (d *Discovery) dhtRound(ctx context.Context, dht *kaddht.IpfsDHT, source string, advTimeout, findTimeout time.Duration) {
	key := d.providerKey()

	advCtx, cancel := context.WithTimeout(ctx, advTimeout)
	if err := dht.Provide(advCtx, key, true); err != nil {
		d.logf("DHT 广播失败（%s，下轮重试）: %v", source, err)
	}
	cancel()

	if !d.knownPeers() {
		return // 地址簿为空：只广播不查找，省查询流量
	}

	findCtx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	for pi := range dht.FindProvidersAsync(findCtx, key, 64) {
		if pi.ID == d.host.ID() {
			continue
		}
		d.addMember(pi.ID, pi.Addrs, source)
	}
}

// publicRetireThreshold 达到该数量的同群节点后，公共 DHT 视为
// 「已完成冷启动使命」并退出（省流量）。
// 1 = 只要有一个同群种子就退（同群成员互为种子，后续发现可持续）。
const publicRetireThreshold = 1

// maybeRetirePublicDHT 每确认一个同群成员就检查一次：攒够足够多的
// 「自己人」后，主动退出公共 DHT（停止广告、清空路由表、关闭流处理），
// 不再承担公共 DHT server 的应答流量（实测上行 ~4MB/分钟）。
// 判据按模式区分：
//   - 有私有 DHT（当前所有路径，含密钥留空默认归一化后）：私有 DHT
//     路由表中的同群节点数；
//   - 无私有 DHT（防御分支，为后续单机多网络重构保留）：成员表中
//     仍活跃（memberTTL 内有真实通讯）的同群成员数。
//
// 退出后不自动回归公共——mDNS 与成员表里的地址仍可重连；彻底失联时
// 重启节点即可重新冷启动。
func (d *Discovery) maybeRetirePublicDHT() {
	d.mu.Lock()
	if d.dhtPublic == nil || d.publicRetired {
		d.mu.Unlock()
		return
	}
	var n int
	if d.dhtPrivate != nil {
		n = len(d.dhtPrivate.RoutingTable().ListPeers())
	} else {
		cutoff := time.Now().Add(-d.memberTTL)
		for _, m := range d.members {
			if m.LastSeen.After(cutoff) {
				n++
			}
		}
	}
	if n < publicRetireThreshold {
		d.mu.Unlock()
		return
	}
	reason := fmt.Sprintf("已连接 %d 个同群成员", n)
	if d.dhtPrivate != nil {
		reason = fmt.Sprintf("私有 DHT 就绪（路由表 %d 个同群节点）", n)
	}
	d.mu.Unlock()
	if d.dhtPrivate != nil {
		d.logf("%s，退出公共 DHT 省流量", reason)
	} else {
		d.logf("%s，退出公共 DHT 省流量", reason)
	}
	go d.retirePublicDHT(reason)
}

// PublicDHTState 公共 DHT 临时引导的运行状态（控制台展示用）。
// Enabled=启动时开启了公共兜底；Active=当前仍挂在公共 DHT 上；
// Reason=退出原因（连上同群成员 / 超时），未退出时为空；
// StartedAt=开启时刻；Timeout=最长运行时长（前端可据此算剩余时间）。
type PublicDHTState struct {
	Enabled   bool      `json:"enabled"`
	Active    bool      `json:"active"`
	Retired   bool      `json:"retired"`
	Reason    string    `json:"reason,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Timeout   float64   `json:"timeout_seconds"`
}

func (d *Discovery) PublicDHTState() PublicDHTState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return PublicDHTState{
		Enabled:   d.cfg.EnablePublicFallback,
		Active:    d.dhtPublic != nil,
		Retired:   d.publicRetired,
		Reason:    d.publicRetireReason,
		StartedAt: d.publicStartedAt,
		Timeout:   d.cfg.PublicDHTTimeout.Seconds(),
	}
}

// retirePublicDHT 退出公共 DHT（幂等，可被「找到即退」与「超时退出」并发触发）：
//  1. Close 停掉 kad 协议流处理与周期刷新（不再应答公网随机查询）；
//  2. 断开全部公网 DHT 连接（识别哪些连接来自公共 DHT 不可行，直接
//     按「非同群成员」过滤——同群成员有成员表背书）；
//  3. 置 nil，后续 advertiseAndDiscover 不再跑公共轮。
//
// 私有 DHT / mDNS / 成员表直连不受影响。
func (d *Discovery) retirePublicDHT(reason string) {
	d.mu.Lock()
	if d.dhtPublic == nil || d.publicRetired {
		d.mu.Unlock()
		return
	}
	d.publicRetired = true // 先置位，防并发重复触发
	d.publicRetireReason = reason
	pub := d.dhtPublic
	d.mu.Unlock()
	if err := pub.Close(); err != nil {
		d.logf("公共 DHT 关闭失败（忽略）: %v", err)
	}
	d.mu.Lock()
	d.dhtPublic = nil
	// 同群成员快照（保留这些连接）。
	keep := make(map[string]struct{}, len(d.members))
	for id := range d.members {
		keep[id] = struct{}{}
	}
	d.mu.Unlock()

	self := d.host.ID()
	for _, c := range d.host.Network().Conns() {
		remote := c.RemotePeer()
		if _, ok := keep[remote.String()]; ok || remote == self {
			continue
		}
		_ = c.Close()
	}
	d.logf("已退出公共 DHT（%s；同群发现继续：mDNS + 成员表 + 私有 DHT，彻底失联时重启可重新冷启动）", reason)
}

// addMember 记录成员并异步建连（连通后经 info 协议确认同群、拿名称）。
//
// 活跃语义：只有真实通讯（connectAndIdentify 成功 / handleInfo 入向握手）
// 才刷新 LastSeen。DHT 的 provider 记录带 TTL（本端 Provide 后仍能在
// 查询结果里出现数分钟），已下线成员会以「陈旧记录」形式反复出现——
// 这里不把发现本身当作活跃证据，否则死成员永远不过期。
func (d *Discovery) addMember(id peer.ID, addrs []ma.Multiaddr, source string) {
	if id == d.host.ID() {
		return
	}
	// TUN 地址只能承载 overlay 流量，不能作为承载 libp2p 的 underlay 地址。
	// 旧节点可能已把 10.7/16 通告进 DHT；接收侧也必须过滤并清理 peerstore，
	// 否则拨号隧道时会再次进入同一隧道，形成递归拨号和队列堆积。
	addrs = p2pkit.FilterUnderlayAddrs(addrs)
	for _, addr := range d.host.Peerstore().Addrs(id) {
		if p2pkit.IsLanetOverlayAddr(addr) {
			d.host.Peerstore().SetAddr(id, addr, 0)
		}
	}

	// 审批门：陌生节点不进成员表、不建连（完全隔离）。
	// 但发现到的地址仍写入 peerstore —— 地址本身不是权限，连接与否由审批门
	// 决定；缓存地址的收益是「用户输入对方节点 ID 时能立刻查到地址」，
	// 不必等下一轮发现。这里**不**上报待审批：被动发现只是「知道有这个人」，
	// 不代表对方申请连接，记入待审批只会让列表被同网段无关节点刷屏。
	// 真正的申请由入向握手（handleInfo）或用户主动添加（RequestConnect）产生。
	if trusted, _ := d.trustPolicy(id.String(), toStrings(addrs), ""); !trusted {
		if len(addrs) > 0 {
			d.host.Peerstore().AddAddrs(id, addrs, time.Hour)
		}
		return
	}

	d.mu.Lock()
	m, ok := d.members[id.String()]
	if !ok {
		m = &Member{
			PeerID:    id.String(),
			VirtualIP: DeriveVirtualIP(d.groupKey, id.String()),
			Source:    source,
			FirstSeen: time.Now(),
			LastSeen:  time.Now(),
		}
		d.members[id.String()] = m
	}
	if len(addrs) > 0 {
		m.Addrs = toStrings(addrs)
	}
	d.mu.Unlock()

	// peerstore 记录地址，供 Connect / 隧道直连使用。
	if len(addrs) > 0 {
		d.host.Peerstore().AddAddrs(id, addrs, time.Hour)
	}
	if !ok {
		d.emit(*m)
	}
	go d.connectAndIdentify(id)
}

// connectAndIdentify 建连并交换成员信息；失败静默（下轮发现会重试）。
// 返回 error 供主动拨号场景（DialSeed）判成败；周期发现路径忽略返回值。
func (d *Discovery) connectAndIdentify(id peer.ID) error {
	// 审批门（出向）：未信任的陌生节点不建连。地址簿里的历史地址可以带过去
	// 一起上报待审批，方便用户在控制台看到「谁在申请连我」。
	if trusted, _ := d.trustPolicy(id.String(), toStrings(p2pkit.FilterUnderlayAddrs(d.host.Peerstore().Addrs(id))), ""); !trusted {
		d.mu.Lock()
		delete(d.members, id.String())
		d.mu.Unlock()
		return fmt.Errorf("对端尚未通过连接审批（已记入待审批列表）")
	}
	connCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if d.host.Network().Connectedness(id) != network.Connected {
		if err := d.host.Connect(connCtx, peer.AddrInfo{ID: id}); err != nil {
			d.logf("连接成员 %s 失败: %v", id.ShortString(), err)
			return fmt.Errorf("建连失败: %w", err)
		}
	}
	info, err := d.fetchInfo(connCtx, id)
	if err != nil {
		d.logf("成员信息交换失败 %s: %v", id.ShortString(), err)
		return fmt.Errorf("信息交换失败: %w", err)
	}
	// 对端明确拒绝：本机尚未被对方审批。返回可识别错误，供上层提示
	// 「已提交申请，等待对方同意」，而不是笼统的失败。
	if info.Rejected != "" {
		d.mu.Lock()
		delete(d.members, id.String())
		d.mu.Unlock()
		return ErrNotApproved
	}
	if info.Group != GroupFingerprint(d.groupKey) {
		d.logf("忽略异群节点 %s", id.ShortString())
		d.mu.Lock()
		delete(d.members, id.String())
		d.mu.Unlock()
		return fmt.Errorf("对方与本节点不在同一网络（网络密钥或分发渠道不一致）")
	}
	// 同群成员互为私有 DHT 种子：确认后立即进路由表，
	// 后续发现不再依赖公共 DHT（快路径生效）。
	if d.dhtPrivate != nil {
		_, _ = d.dhtPrivate.RoutingTable().TryAddPeer(id, false, false)
	}
	// 确认同群即检查公共 DHT 退出（公共网络模式无私有 DHT，同样适用）。
	d.maybeRetirePublicDHT()
	d.mu.Lock()
	if m, ok := d.members[id.String()]; ok {
		m.Name = info.Name
		m.Version = info.Version
		m.Platform = info.Platform
		m.OSHostname = info.OSHostname
		if len(info.LocalIPs) > 0 {
			m.LocalIPs = info.LocalIPs
		}
		m.LastSeen = time.Now()
	} else {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	d.mu.RLock()
	m := d.members[id.String()]
	if m != nil {
		d.emit(*m)
	}
	d.mu.RUnlock()
	return nil
}

// infoPayload info 协议载荷。
// Rejected 非空表示本端拒绝了这次握手（未通过连接审批）——此时其余字段
// 一律留空，不向对方泄漏任何身份信息，只告知「你还没被同意」。
type infoPayload struct {
	Name       string   `json:"name"`
	Group      string   `json:"group"`
	OS         string   `json:"os"`
	Version    string   `json:"version,omitempty"`     // 程序版本（P2P 自更新用）
	Platform   string   `json:"platform,omitempty"`    // GOOS/GOARCH
	OSHostname string   `json:"os_hostname,omitempty"` // 操作系统主机名（0.5.15 起）
	LocalIPs   []string `json:"local_ips,omitempty"`   // 本机非回环网卡 IP（0.5.15 起）
	Rejected   string   `json:"rejected,omitempty"`    // 非空 = 拒绝原因（连接审批）
}

// rejectionNotApproved 拒绝原因常量（info 协议内传递）。
const rejectionNotApproved = "not_approved"

// handleInfo 入向信息交换。
// 注意顺序：先回写响应再结束——libp2p 流半关闭（CloseWrite）后写端已关，
// 提前 CloseWrite 会导致响应丢失（对端读 EOF）。
func (d *Discovery) handleInfo(s network.Stream) {
	defer s.Close()
	var req infoPayload
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		return
	}
	if req.Group != GroupFingerprint(d.groupKey) {
		return
	}
	remote := s.Conn().RemotePeer()
	// 审批门：陌生节点一律不响应 info（不回写名称/版本/主机名，避免身份
	// 信息与网络拓扑泄漏），不进成员表、不进私有 DHT 路由表。
	// 但会明确回一个「未同意」标记——对端据此提示「等待对方同意」，
	// 而不是让用户面对一个沉默的 EOF 无从判断。
	if trusted, _ := d.trustPolicy(remote.String(), nil, req.Name); !trusted {
		d.logf("拒绝陌生节点 %s 的握手（未审批，已记入待审批列表）", remote.ShortString())
		_ = json.NewEncoder(s).Encode(infoPayload{
			Group:    GroupFingerprint(d.groupKey),
			Rejected: rejectionNotApproved,
		})
		return
	}
	// 对端主动来握手：同样视为活跃成员，顺带补齐名称与活跃时间
	// （本端出向 connectAndIdentify 失败时也能从这里拿到名称）。
	//
	// 注意必须支持「新增」而不只是「更新」：典型场景是对端先把本机加为好友
	// 并主动拨入，而本机此前从未成功建连过对方（出向曾因未审批被拒、
	// 成员表里没有它）。若只更新已有条目，就会出现「对方已握手成功、
	// 本机成员表却始终为空」的假象。
	var addrs []ma.Multiaddr
	if conn := s.Conn(); conn != nil {
		addrs = p2pkit.FilterUnderlayAddrs([]ma.Multiaddr{conn.RemoteMultiaddr()})
	}
	d.mu.Lock()
	m, ok := d.members[remote.String()]
	if !ok {
		m = &Member{
			PeerID:    remote.String(),
			VirtualIP: DeriveVirtualIP(d.groupKey, remote.String()),
			Source:    "inbound",
			Addrs:     toStrings(addrs),
			FirstSeen: time.Now(),
			LastSeen:  time.Now(),
		}
		d.members[remote.String()] = m
	}
	if req.Name != "" {
		m.Name = req.Name
	}
	m.Version = req.Version
	m.Platform = req.Platform
	m.OSHostname = req.OSHostname
	if len(req.LocalIPs) > 0 {
		m.LocalIPs = req.LocalIPs
	}
	if len(addrs) > 0 {
		m.Addrs = toStrings(addrs)
	}
	m.LastSeen = time.Now()
	snapshot := *m
	d.mu.Unlock()
	if !ok {
		d.emit(snapshot)
	}
	// 对端地址入 peerstore：后续建立隧道/直连要用（与成员表同步）。
	if len(addrs) > 0 {
		d.host.Peerstore().AddAddrs(remote, addrs, time.Hour)
	}
	if d.dhtPrivate != nil {
		_, _ = d.dhtPrivate.RoutingTable().TryAddPeer(remote, false, false)
	}
	d.maybeRetirePublicDHT()
	resp := infoPayload{
		Name:       d.cfg.Name,
		Group:      GroupFingerprint(d.groupKey),
		Version:    d.cfg.Version,
		Platform:   d.cfg.Platform,
		OSHostname: d.cfg.OSHostname,
		LocalIPs:   d.cfg.LocalIPs,
	}
	_ = json.NewEncoder(s).Encode(resp)
}

// fetchInfo 主动交换成员信息。
func (d *Discovery) fetchInfo(ctx context.Context, id peer.ID) (infoPayload, error) {
	stream, err := d.host.NewStream(ctx, id, ProtocolInfo)
	if err != nil {
		return infoPayload{}, err
	}
	defer stream.Close()
	req := infoPayload{
		Name:       d.cfg.Name,
		Group:      GroupFingerprint(d.groupKey),
		Version:    d.cfg.Version,
		Platform:   d.cfg.Platform,
		OSHostname: d.cfg.OSHostname,
		LocalIPs:   d.cfg.LocalIPs,
	}
	if err = json.NewEncoder(stream).Encode(req); err != nil {
		return infoPayload{}, err
	}
	_ = stream.CloseWrite()
	var resp infoPayload
	if err = json.NewDecoder(stream).Decode(&resp); err != nil {
		return infoPayload{}, err
	}
	return resp, nil
}

// Resolve 实现 tunnel.GroupNetMap：按虚拟 IP 解析成员。
func (d *Discovery) Resolve(virtualIP string) (netmapclient.Route, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, m := range d.members {
		if m.VirtualIP == virtualIP {
			return netmapclient.Route{VirtualIP: m.VirtualIP, PeerID: m.PeerID, Addrs: m.Addrs}, true
		}
	}
	return netmapclient.Route{}, false
}

// Candidates 实现 tunnel.RelaySource：成员表作为中继候选
// （每个 standalone 节点默认运行 relay service；不支持中继的候选会被
// tunnel 层 Reserve 失败后自动跳过）。
func (d *Discovery) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]peer.AddrInfo, 0, number)
	for _, m := range d.members {
		if m.PeerID == d.host.ID().String() {
			continue
		}
		id, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		addrs := d.host.Peerstore().Addrs(id)
		if len(addrs) == 0 {
			continue
		}
		out = append(out, peer.AddrInfo{ID: id, Addrs: addrs})
		if len(out) >= number {
			break
		}
	}
	return out, nil
}

// providerKey rendezvous key 的 DHT 记录形式。
func (d *Discovery) providerKey() cid.Cid {
	sum := sha256.Sum256([]byte(RendezvousKey(d.groupKey)))
	mhash, _ := mh.Encode(sum[:], mh.SHA2_256)
	return cid.NewCidV1(cid.Raw, mhash)
}

func (d *Discovery) emit(m Member) {
	for _, cb := range d.onDiscovered {
		func() {
			defer func() { recover() }()
			cb(m)
		}()
	}
}

func (d *Discovery) logf(format string, args ...any) {
	if d.cfg.Quiet {
		return
	}
	log.Printf("[lanet-serverless] "+format, args...)
}

// mdnsNotifee mDNS 发现回调。
type mdnsNotifee struct{ d *Discovery }

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	n.d.addMember(pi.ID, pi.Addrs, "mdns")
}

// parseBootstrap 解析引导节点地址。
// parseBootstrap 解析引导地址：支持普通 multiaddr 与 /dnsaddr/（经
// DNS TXT 记录解析出具体地址列表，含 /p2p 节点 ID，如官方公共 DHT）。
func parseBootstrap(ctx context.Context, addrs []string) ([]peer.AddrInfo, error) {
	out := make([]peer.AddrInfo, 0, len(addrs))
	for _, raw := range addrs {
		ma0, err := ma.NewMultiaddr(raw)
		if err != nil {
			return nil, fmt.Errorf("serverless: 引导地址 %q 非法: %w", raw, err)
		}
		resolved := []ma.Multiaddr{ma0}
		if _, errIsDNS := ma0.ValueForProtocol(ma.P_DNSADDR); errIsDNS == nil {
			rs, rerr := madns.DefaultResolver.Resolve(ctx, ma0)
			if rerr != nil {
				return nil, fmt.Errorf("serverless: dnsaddr %q 解析失败: %w", raw, rerr)
			}
			if len(rs) == 0 {
				continue
			}
			resolved = rs
		}
		for _, r := range resolved {
			ai, err := peer.AddrInfoFromP2pAddr(r)
			if err != nil || ai == nil {
				continue // 无 /p2p 组件的地址无法确定节点 ID，跳过
			}
			out = append(out, *ai)
		}
	}
	return out, nil
}

func parseBootstrapQuiet(ctx context.Context, addrs []string) []peer.AddrInfo {
	out, _ := parseBootstrap(ctx, addrs)
	return out
}

func toStrings(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}
