package lanet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// 默认地址簿文件名（与 exe 同目录，或由 Config.DBPath 指定）。
const defaultDBFile = "lanet.db"

// PeersDBPath 当前地址簿文件路径（未启用持久化时为空）。
func (c *Client) PeersDBPath() string {
	if c.peers == nil {
		return ""
	}
	return c.peers.Path()
}

// openPeersDB 打开地址簿（仅 Standalone）。DBPath 语义：
//   - "" → 默认路径：优先 exe 同目录（发行版约定），回退当前工作目录；
//   - "-" → 关闭持久化，返回 nil（调用方不设置 c.peers）；
//   - 其他 → 按给定路径。
func (c *Client) openPeersDB(ctx context.Context) (*peersdb.DB, error) {
	path := c.cfg.DBPath
	if path == "-" {
		return nil, nil
	}
	if path == "" {
		path = defaultDBFile
		if exe, err := os.Executable(); err == nil {
			path = filepath.Join(filepath.Dir(exe), defaultDBFile)
		}
	}
	db, err := peersdb.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	known, _ := db.ListPeers(ctx, false)
	c.logf("地址簿已打开：%s（已知节点 %d 个）", db.Path(), len(known))
	return db, nil
}

// requireApproval 是否要求连接审批（默认 true；显式 false 时退回旧行为）。
func (c *Client) requireApproval() bool {
	if c.cfg.RequireApproval != nil {
		return *c.cfg.RequireApproval
	}
	return true
}

// isTrustedPeer 供 serverless 审批门查询：该节点是否已被信任。
// 未启用审批（RequireApproval=false）或地址簿不可用时一律放行。
func (c *Client) isTrustedPeer(peerID string) bool {
	if !c.requireApproval() || c.peers == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := c.peers.IsTrusted(ctx, peerID)
	if err != nil {
		return false
	}
	return ok
}

// maybeAutoAccept 供 serverless 审批门使用：auto_accept 开启时自动信任，
// 并把节点写入地址簿（标记为「自动同意」来源，便于审计）。
func (c *Client) maybeAutoAccept(peerID string, addrs []string, name string) bool {
	if !c.cfg.AutoAccept {
		return false
	}
	c.rememberPeer(peerID, addrs, name, true)
	return true
}

// onPendingRequest 供 serverless 审批门使用：陌生节点申请连接时落库，
// 供控制台「待审批」列表展示。
func (c *Client) onPendingRequest(peerID string, addrs []string, name string) {
	if c.peers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.UpsertPeer(ctx, peersdb.Peer{PeerID: peerID, Name: name}, addrs); err != nil {
		c.logf("待审批节点落库失败: %v", err)
		return
	}
	if err := c.peers.AddPending(ctx, peersdb.PendingRequest{PeerID: peerID, Name: name, Addrs: addrs}); err != nil {
		c.logf("待审批记录落库失败: %v", err)
		return
	}
	c.logf("收到陌生节点 %s 的连接申请，已加入待审批列表（控制台可同意/拒绝）", shortPeer(peerID))
}

// hasKnownPeers 供 serverless 流量控制使用：地址簿里是否有已知节点。
// 为空（首次启动）时不做主动 DHT 查找，只保留自身广播。
func (c *Client) hasKnownPeers() bool {
	if c.peers == nil {
		// 无地址簿时退回历史行为：允许查找（避免完全失去发现能力）。
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := c.peers.ListPeers(ctx, false)
	if err != nil {
		return true
	}
	return len(list) > 0
}

// rememberPeer 把节点记入地址簿；trusted=true 表示直接标记为已信任。
func (c *Client) rememberPeer(peerID string, addrs []string, name string, trusted bool) {
	if c.peers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.UpsertPeer(ctx, peersdb.Peer{PeerID: peerID, Name: name}, addrs); err != nil {
		c.logf("节点落库失败: %v", err)
		return
	}
	if trusted {
		if err := c.peers.SetTrusted(ctx, peerID, true); err != nil {
			c.logf("信任标记失败: %v", err)
		}
	}
}

// ---- 对外 API ----

// ConnResult 一次连接申请的结果。
type ConnResult struct {
	// PeerID 对端节点 ID。
	PeerID string `json:"peer_id"`
	// Pending 为 true 表示已提交连接申请、等待对端同意（尚未连通）。
	Pending bool `json:"pending"`
	// Searching 为 true 表示本机已记录并信任该节点，但当前还没在私有 DHT
	// 里查到它的地址（冷启动路由表未建立 / 对方暂未在线）。这不是失败：
	// 周期发现会在 DHT 就绪后自动完成连接，无需用户反复点击。
	Searching bool `json:"searching,omitempty"`
	// Message 面向用户的结果说明。
	Message string `json:"message"`
	// Name 对端名称（已连通且交换过 info 时填充）。
	Name string `json:"name,omitempty"`
	// VirtualIP 对端虚拟 IP（已连通时填充）。
	VirtualIP string `json:"virtual_ip,omitempty"`
	// Via 本次地址来源：local（本地地址簿）/ dht（私有 DHT 查找）/ manual（直填地址）。
	Via string `json:"via,omitempty"`
}

// ConnectPeer 按节点 ID 发起连接（控制台统一输入框的后端）。
//
// address 可以是：
//   - 纯节点 ID（12D3Koo...）：先查本地地址簿（快、零流量），
//     未命中再走私有 DHT 查找（仅同网络密钥内可见）；
//   - 完整 multiaddr（/ip4/1.2.3.4/tcp/4001/p2p/12D3Koo...）：直接拨号。
//
// 返回 Pending=true 表示已提交申请、等对方在控制台同意（加好友语义）。
func (c *Client) ConnectPeer(ctx context.Context, address string) (*ConnResult, error) {
	if c.disc == nil {
		return nil, fmt.Errorf("连接节点功能仅在 Standalone 无服务器模式可用")
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("请输入对方节点 ID 或连接地址")
	}

	// 1) multiaddr 形式（含 /p2p/<ID>）：直接按连接种子拨号。
	//    先解析出对端 ID 并在本机记为已信任（同「添加节点」语义），
	//    否则会被自己的审批门拦住。
	if strings.Contains(address, "/p2p/") {
		m, mErr := ma.NewMultiaddr(address)
		if mErr != nil {
			return nil, fmt.Errorf("连接地址格式不正确: %w", mErr)
		}
		ai, aiErr := peer.AddrInfoFromP2pAddr(m)
		if aiErr != nil || ai == nil {
			return nil, fmt.Errorf("连接地址缺少有效的 /p2p/<节点ID> 段")
		}
		if ai.ID.String() == c.peerID {
			return nil, fmt.Errorf("这是本机节点 ID，无需连接")
		}
		c.rememberPeer(ai.ID.String(), []string{address}, "", true)
		id, err := c.disc.DialSeed(ctx, []string{address})
		if err != nil {
			if errors.Is(err, serverless.ErrNotApproved) {
				return &ConnResult{
					PeerID:  ai.ID.String(),
					Pending: true,
					Via:     "manual",
					Message: "已提交连接申请，等待对方在控制台同意后即可连通（同意一次即永久信任）",
				}, nil
			}
			return nil, err
		}
		res := c.resultFor(id, "manual")
		res.Message = "已连接"
		return res, nil
	}

	// 2) 纯节点 ID：本地地址簿优先 → 私有 DHT 兜底。
	id, err := peer.Decode(address)
	if err != nil {
		return nil, fmt.Errorf("节点 ID 或连接地址格式不正确：需要 12D3Koo… 形式的节点 ID，或 /ip4/…/p2p/… 完整地址")
	}
	if id.String() == c.peerID {
		return nil, fmt.Errorf("这是本机节点 ID，无需连接")
	}

	// 2.1 本地地址簿命中：直接用历史地址拨号，零 DHT 流量。
	var (
		ai  peer.AddrInfo
		via string
	)
	if c.peers != nil {
		dbCtx, dbCancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, gerr := c.peers.KnownAddrs(dbCtx, id.String())
		dbCancel()
		if gerr == nil && len(addrs) > 0 {
			if parsed := parseAddrs(addrs); len(parsed) > 0 {
				ai = peer.AddrInfo{ID: id, Addrs: parsed}
				via = "local"
			}
		}
	}
	// 2.2 地址簿未命中或地址失效：走私有 DHT 查找（仅同密钥内可见）。
	//     同步查找只给 6s「快速探测」：命中即走完整建连流程；未命中不硬等
	//     DHT 的 20s 超时——转入后台查找，UI 立刻得到「查找中」反馈。
	if len(ai.Addrs) == 0 {
		probeCtx, probeCancel := context.WithTimeout(ctx, 6*time.Second)
		found, ferr := c.disc.FindPeer(probeCtx, id.String())
		probeCancel()
		if ferr != nil {
			// 查不到 ≠ 连不上：私有 DHT 冷启动时路由表还没建立，对方的
			// provider 记录也可能尚未传播到本节点附近——一次查询失败是
			// 常态，不该当终局错误抛给用户（否则只能反复点「连接」碰运气）。
			// 正确姿势：
			//   1) 把对方记入地址簿并标记已信任（用户主动填 ID = 本机已同意，
			//      与建连成功路径同一语义）；
			//   2) 交给周期发现（每轮 FindProviders 群 provider key）自动补连，
			//      addMember 过审批门时因已信任而直接建连。
			// 地址簿有记录后 knownPeers 流量门放开，后台查找本来就会执行。
			c.rememberPeer(id.String(), nil, "", true)
			// 立刻触发一轮发现（不等 30s 周期）：对方 provider 记录已在
			// DHT 里时这一轮就能命中并自动建连。
			c.disc.TriggerDiscover()
			return &ConnResult{
				PeerID:    id.String(),
				Searching: true,
				Message: "已记录节点 " + shortPeer(id.String()) +
					"，正在网络中查找其地址（DHT 发现需要一点时间），查到后会自动连接，无需重复点击。" +
					"若长时间未连上：核对节点 ID 与网络密钥是否一致，或向对方索取「连接种子」直连。",
			}, nil
		}
		ai = found
		via = "dht"
	}

	// 3) 走审批门建连。
	//    语义确认：用户「主动填写对方节点 ID」这件事本身就是我这边的同意，
	//    所以先在本机把对方记为已信任，再由对方审批本机（加好友是双向的）。
	//    这样避免「我明明点了连接，却被自己的审批门拦住」的荒谬体验。
	c.rememberPeer(id.String(), toStringAddrs(ai.Addrs), "", true)

	connectedID, cerr := c.disc.RequestConnect(ctx, ai)
	if cerr != nil {
		// 对方尚未同意本机：这不算失败，是「申请已送达」的正常中间态。
		if errors.Is(cerr, serverless.ErrNotApproved) || strings.Contains(cerr.Error(), "等待对方同意") {
			return &ConnResult{
				PeerID:  connectedID,
				Pending: true,
				Via:     via,
				Message: "已提交连接申请，等待对方在控制台同意后即可连通（同意一次即永久信任）",
			}, nil
		}
		return nil, cerr
	}

	if c.peers != nil {
		for _, a := range toStringAddrs(ai.Addrs) {
			dbCtx, dbCancel := context.WithTimeout(ctx, 3*time.Second)
			_ = c.peers.NoteDialResult(dbCtx, id.String(), a, true)
			dbCancel()
			break // 记录首个成功地址即可
		}
	}
	res := c.resultFor(id.String(), via)
	res.Message = "已连接"
	return res, nil
}

// resultFor 组装连接成功的结果（尽量补上名称与虚拟 IP）。
func (c *Client) resultFor(peerID, via string) *ConnResult {
	res := &ConnResult{PeerID: peerID, Via: via, Message: "已连接"}
	if c.disc == nil {
		return res
	}
	for _, m := range c.disc.Peers() {
		if m.PeerID == peerID {
			res.Name = m.Name
			res.VirtualIP = m.VirtualIP
			break
		}
	}
	return res
}

// ---- 待审批 / 信任管理 ----

// PendingList 待审批列表（控制台展示）。
func (c *Client) PendingList() []peersdb.PendingRequest {
	if c.peers == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := c.peers.ListPending(ctx)
	if err != nil {
		c.logf("读取待审批列表失败: %v", err)
		return nil
	}
	return list
}

// ApprovePeer 同意某个节点的连接申请（首次审批，永久信任）。
func (c *Client) ApprovePeer(peerID string) error {
	if c.peers == nil {
		return fmt.Errorf("地址簿未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.SetTrusted(ctx, peerID, true); err != nil {
		return err
	}
	c.logf("已同意节点 %s 的连接申请（永久信任）", shortPeer(peerID))
	// 立刻尝试建连（对端可能正在等待）。
	if c.disc != nil {
		go func() {
			dialCtx, dialCancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer dialCancel()
			if _, err := c.ConnectPeer(dialCtx, peerID); err != nil {
				c.logf("批准后自动建连未成功（对方可能已离线，等待下轮重连）: %v", err)
			}
		}()
	}
	return nil
}

// RejectPeer 拒绝某个节点的连接申请（同时移出地址簿）。
func (c *Client) RejectPeer(peerID string) error {
	if c.peers == nil {
		return fmt.Errorf("地址簿未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.SetTrusted(ctx, peerID, false); err != nil {
		return err
	}
	if err := c.peers.DeletePeer(ctx, peerID); err != nil {
		return err
	}
	c.logf("已拒绝节点 %s 的连接申请", shortPeer(peerID))
	return nil
}

// TrustedPeers 已信任节点列表（地址簿，按最近连通时间排序）。
func (c *Client) TrustedPeers() []peersdb.Peer {
	if c.peers == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := c.peers.ListPeers(ctx, true)
	if err != nil {
		c.logf("读取地址簿失败: %v", err)
		return nil
	}
	return list
}

// RemovePeer 从地址簿移除节点（同时撤销信任）。
func (c *Client) RemovePeer(peerID string) error {
	if c.peers == nil {
		return fmt.Errorf("地址簿未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.SetTrusted(ctx, peerID, false); err != nil {
		return err
	}
	return c.peers.DeletePeer(ctx, peerID)
}

// ---- 辅助 ----

func parseAddrs(addrs []string) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	for _, raw := range addrs {
		if m, err := ma.NewMultiaddr(raw); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func toStringAddrs(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}
