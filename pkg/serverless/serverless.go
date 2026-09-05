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
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
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

// ProtocolInfo 节点信息交换协议（建连后校验同群 + 交换名称）。
const ProtocolInfo = "/lanet/info/1.0.0"

// Config 发现服务配置。
type Config struct {
	// NetworkKey 网络密钥：相同密钥的节点组成同一张 P2P 网络。
	// 留空 = 加入公共网络（所有留空节点互通，见 PublicNetworkKey）；
	// 填写任意非空字符串 = 私有网络，只有持相同密钥的节点能互相发现与连接。
	NetworkKey string
	// Name 本节点名称（随 info 协议交换给同网络成员）。
	Name string
	// Bootstrap DHT 引导节点 multiaddr 列表。
	// 私有网络（NetworkKey 非空）下作为「私有 DHT 种子」：填任意已在网
	// 成员的 multiaddr 即可加速入网；填 DefaultBootstrap 会被识别为公共
	// 引导（不作为私有种子）。公共网络下即公共 DHT 引导列表。
	Bootstrap []string
	// DisablePublicFallback 私有网络下关闭公共 DHT 兜底（纯私有发现：
	// 私有引导节点 + mDNS）。默认开启兜底：私有种子不可达时仍可经公共
	// DHT 找到同群成员。仅对 NetworkKey 非空时生效。
	DisablePublicFallback bool
	// EnableMDNS 启用局域网 mDNS 自动发现。
	EnableMDNS bool
	// Interval 广播/发现周期，默认 30s。
	Interval time.Duration
	// Quiet 为 true 时不打日志。
	Quiet bool
}

// Member 成员表中的一项。
type Member struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	VirtualIP string   `json:"virtual_ip"`
	Addrs     []string `json:"addrs"`
	Source    string   `json:"source"` // dht / dht-private / mdns
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

	mu      sync.RWMutex
	members map[string]*Member // peerID -> member

	onDiscovered []Discovered
}

// New 创建并启动发现服务：连引导节点、初始化 DHT、启动 mDNS。
func New(ctx context.Context, h host.Host, cfg Config) (*Discovery, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	d := &Discovery{
		host:     h,
		cfg:      cfg,
		groupKey: GroupKey(cfg.NetworkKey), // 空密钥按公共网络处理
		members:  make(map[string]*Member),
	}
	d.selfIP = DeriveVirtualIP(d.groupKey, h.ID().String())

	// 1. DHT：每台节点都是 server（客户端即服务端）。
	//    私有网络跑双 DHT：私有（/lanet 前缀，只有本网络节点）优先发现，
	//    公共（/ipfs 前缀）兜底——负责跨网冷启动时找到第一个「自己人」。
	//    公共网络（密钥留空）只跑公共 DHT，语义与此前一致。
	if cfg.NetworkKey != "" {
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
		if !cfg.DisablePublicFallback {
			pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pubParsed, perr := parseBootstrap(pubCtx, []string{DefaultBootstrap})
			cancel()
			if perr != nil {
				d.logf("公共引导解析失败，公共兜底暂不可用（下轮重试广播）: %v", perr)
			}
			d.dhtPublic, err = kaddht.New(h,
				kaddht.Mode(kaddht.ModeAutoServer),
				kaddht.BootstrapPeers(pubParsed...),
			)
			if err != nil {
				return nil, fmt.Errorf("serverless: init public dht: %w", err)
			}
		}
	} else {
		bootstraps, err := parseBootstrap(ctx, cfg.Bootstrap)
		if err != nil {
			return nil, err
		}
		d.dhtPublic, err = kaddht.New(h,
			kaddht.Mode(kaddht.ModeAutoServer),
			kaddht.BootstrapPeers(bootstraps...),
		)
		if err != nil {
			return nil, fmt.Errorf("serverless: init dht: %w", err)
		}
	}

	// 2. mDNS（可选）：NewMdnsService 创建即启动。
	if cfg.EnableMDNS {
		tag := MdnsTag(d.groupKey)
		d.mdnsSvc = mdns.NewMdnsService(h, tag, &mdnsNotifee{d: d})
	}
	return d, nil
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
		d.logf("双 DHT 模式：私有发现优先，公共 DHT 兜底")
	}
	d.host.SetStreamHandler(ProtocolInfo, d.handleInfo)
	return nil
}

// SelfVirtualIP 本节点在无服务器模式下的虚拟 IP。
func (d *Discovery) SelfVirtualIP() string { return d.selfIP }

// GroupKey 本群的群组密钥（由邀请码派生）。
func (d *Discovery) GroupKey() []byte { return d.groupKey }

// OnDiscovered 注册新成员回调。
func (d *Discovery) OnDiscovered(cb Discovered) { d.onDiscovered = append(d.onDiscovered, cb) }

// Peers 当前成员表快照（不含自身）。
func (d *Discovery) Peers() []Member {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Member, 0, len(d.members))
	for _, m := range d.members {
		out = append(out, *m)
	}
	return out
}

// Run 阻塞运行周期广播与发现，直到 ctx 取消。
func (d *Discovery) Run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	d.advertiseAndDiscover(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.advertiseAndDiscover(ctx)
		}
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

// addMember 记录成员并异步建连（连通后经 info 协议确认同群、拿名称）。
func (d *Discovery) addMember(id peer.ID, addrs []ma.Multiaddr, source string) {
	if id == d.host.ID() {
		return
	}
	d.mu.Lock()
	m, ok := d.members[id.String()]
	if !ok {
		m = &Member{
			PeerID:    id.String(),
			VirtualIP: DeriveVirtualIP(d.groupKey, id.String()),
			Source:    source,
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
	d.mu.Lock()
	if m, ok := d.members[id.String()]; ok {
		m.Name = info.Name
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
	Name  string `json:"name"`
	Group string `json:"group"`
	OS    string `json:"os"`
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
	resp := infoPayload{
		Name:  d.cfg.Name,
		Group: GroupFingerprint(d.groupKey),
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
	req := infoPayload{Name: d.cfg.Name, Group: GroupFingerprint(d.groupKey)}
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
