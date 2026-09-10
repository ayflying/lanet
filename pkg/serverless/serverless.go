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

// DefaultPublicDHTTimeout 公共 DHT 临时引导的默认最长运行时长。
// 超时无论是否发现同群成员都自动退出（省流量；重启可重新引导）。
const DefaultPublicDHTTimeout = 10 * time.Minute

// Config 发现服务配置。
type Config struct {
	// NetworkKey 网络密钥：相同密钥的节点组成同一张 P2P 网络。
	// 留空 = 加入公共网络（所有留空节点互通，见 PublicNetworkKey）；
	// 填写任意非空字符串 = 私有网络，只有持相同密钥的节点能互相发现与连接。
	NetworkKey string
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
		// 密钥留空 = 默认公共网络密钥：群身份与历史「留空派生」完全一致
		//（GroupKey(channel, "") == GroupKey(channel, PublicNetworkKey)，零迁移），
		// 但统一走双 DHT 路径——私有优先 + 公共兜底，发现同群成员后自动
		// 退出公共 DHT 省流量。不再存在「无密钥公共网络模式」特例，
		// 为单机同时加入多个网络铺平道路：每个网络实例都有明确密钥。
		cfg.NetworkKey = PublicNetworkKey
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

// SelfVirtualIP 本节点在无服务器模式下的虚拟 IP。
func (d *Discovery) SelfVirtualIP() string { return d.selfIP }

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
func (d *Discovery) dhtRound(ctx context.Context, dht *kaddht.IpfsDHT, source string, advTimeout, findTimeout time.Duration) {
	key := d.providerKey()

	advCtx, cancel := context.WithTimeout(ctx, advTimeout)
	if err := dht.Provide(advCtx, key, true); err != nil {
		d.logf("DHT 广播失败（%s，下轮重试）: %v", source, err)
	}
	cancel()

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
func (d *Discovery) connectAndIdentify(id peer.ID) {
	connCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if d.host.Network().Connectedness(id) != network.Connected {
		if err := d.host.Connect(connCtx, peer.AddrInfo{ID: id}); err != nil {
			d.logf("连接成员 %s 失败: %v", id.ShortString(), err)
			return
		}
	}
	info, err := d.fetchInfo(connCtx, id)
	if err != nil {
		d.logf("成员信息交换失败 %s: %v", id.ShortString(), err)
		return
	}
	if info.Group != GroupFingerprint(d.groupKey) {
		d.logf("忽略异群节点 %s", id.ShortString())
		d.mu.Lock()
		delete(d.members, id.String())
		d.mu.Unlock()
		return
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
		return
	}
	d.mu.Unlock()
	d.mu.RLock()
	m := d.members[id.String()]
	if m != nil {
		d.emit(*m)
	}
	d.mu.RUnlock()
}

// infoPayload info 协议载荷。
type infoPayload struct {
	Name       string   `json:"name"`
	Group      string   `json:"group"`
	OS         string   `json:"os"`
	Version    string   `json:"version,omitempty"`     // 程序版本（P2P 自更新用）
	Platform   string   `json:"platform,omitempty"`    // GOOS/GOARCH
	OSHostname string   `json:"os_hostname,omitempty"` // 操作系统主机名（0.5.15 起）
	LocalIPs   []string `json:"local_ips,omitempty"`   // 本机非回环网卡 IP（0.5.15 起）
}

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
	// 对端主动来握手：同样视为活跃成员，顺带补齐名称与活跃时间
	// （本端出向 connectAndIdentify 失败时也能从这里拿到名称）。
	remote := s.Conn().RemotePeer()
	d.mu.Lock()
	if m, ok := d.members[remote.String()]; ok {
		if req.Name != "" {
			m.Name = req.Name
		}
		m.Version = req.Version
		m.Platform = req.Platform
		m.OSHostname = req.OSHostname
		if len(req.LocalIPs) > 0 {
			m.LocalIPs = req.LocalIPs
		}
		m.LastSeen = time.Now()
	}
	d.mu.Unlock()
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
