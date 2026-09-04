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
	mh "github.com/multiformats/go-multihash"
)

// 公共 DHT 引导节点（libp2p 官方公共引导列表，dnsaddr 自动解析出节点列表）。
// 国内网络可达性需实测；不可达时可通过 Config.Bootstrap 指定任意
// 已在网成员的 multiaddr（每台节点都是潜在种子）。
const DefaultBootstrap = "/dnsaddr/bootstrap.libp2p.io"

// ProtocolInfo 节点信息交换协议（建连后校验同群 + 交换名称）。
const ProtocolInfo = "/lanet/info/1.0.0"

// Config 发现服务配置。
type Config struct {
	// InviteCode 必填：群组密钥来源（同群成员持同一邀请码）。
	InviteCode string
	// Name 本节点名称（随 info 协议交换给同群成员）。
	Name string
	// Bootstrap DHT 引导节点 multiaddr 列表；为空时仅 mDNS（纯局域网）。
	// 传入 DefaultBootstrap 可加入公共 DHT 网络。
	Bootstrap []string
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
	Source    string   `json:"source"` // dht / mdns
}

// Discovered 新成员被发现（尚未连通也会触发；连通并确认同群后 Name 有效）。
type Discovered func(Member)

// Discovery 无服务器成员发现服务。
type Discovery struct {
	host     host.Host
	cfg      Config
	groupKey []byte
	selfIP   string

	dht     *kaddht.IpfsDHT
	mdnsSvc mdns.Service

	mu      sync.RWMutex
	members map[string]*Member // peerID -> member

	onDiscovered []Discovered
}

// New 创建并启动发现服务：连引导节点、初始化 DHT、启动 mDNS。
func New(ctx context.Context, h host.Host, cfg Config) (*Discovery, error) {
	if cfg.InviteCode == "" {
		return nil, fmt.Errorf("serverless: InviteCode 必填")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	d := &Discovery{
		host:     h,
		cfg:      cfg,
		groupKey: GroupKey(cfg.InviteCode),
		members:  make(map[string]*Member),
	}
	d.selfIP = DeriveVirtualIP(d.groupKey, h.ID().String())

	// 1. DHT：默认每台节点都是 server（客户端即服务端）。
	bootstraps, err := parseBootstrap(cfg.Bootstrap)
	if err != nil {
		return nil, err
	}
	dhtOpts := []kaddht.Option{
		kaddht.Mode(kaddht.ModeAutoServer),
		kaddht.BootstrapPeers(bootstraps...),
	}
	d.dht, err = kaddht.New(ctx, h, dhtOpts...)
	if err != nil {
		return nil, fmt.Errorf("serverless: init dht: %w", err)
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
	for _, b := range parseBootstrapQuiet(d.cfg.Bootstrap) {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := d.host.Connect(dialCtx, b); err != nil {
			d.logf("引导节点连接失败（不影响运行）: %v", err)
		} else {
			d.logf("已连接引导节点 %s", b.ID.ShortString())
		}
		cancel()
	}
	if err := d.dht.Bootstrap(ctx); err != nil {
		d.logf("DHT 自举未完成（周期重试）: %v", err)
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
func (d *Discovery) advertiseAndDiscover(ctx context.Context) {
	key := d.providerKey()

	// 广播：把自身发布为群的 provider（TTL 由 DHT 管理，周期刷新）。
	advCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	if err := d.dht.Provide(advCtx, key, true); err != nil {
		d.logf("DHT 广播失败（下轮重试）: %v", err)
	}
	cancel()

	// 查找：同群 provider 列表。
	findCtx, cancel2 := context.WithTimeout(ctx, 25*time.Second)
	for pi := range d.dht.FindProvidersAsync(findCtx, key, 64) {
		if pi.ID == d.host.ID() {
			continue
		}
		d.addMember(pi.ID, pi.Addrs, "dht")
	}
	cancel2()
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
func parseBootstrap(addrs []string) ([]peer.AddrInfo, error) {
	out := make([]peer.AddrInfo, 0, len(addrs))
	for _, raw := range addrs {
		ma0, err := ma.NewMultiaddr(raw)
		if err != nil {
			return nil, fmt.Errorf("serverless: 引导地址 %q 非法: %w", raw, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma0)
		if err != nil || ai == nil {
			// dnsaddr 无 /p2p 后缀也可作为引导（解析时确定 ID）。
			out = append(out, peer.AddrInfo{Addrs: []ma.Multiaddr{ma0}})
			continue
		}
		out = append(out, *ai)
	}
	return out, nil
}

func parseBootstrapQuiet(addrs []string) []peer.AddrInfo {
	out, _ := parseBootstrap(addrs)
	return out
}

func toStrings(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}
