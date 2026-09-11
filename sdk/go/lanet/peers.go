package lanet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/invitecode"
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

// IsPeerTrusted 导出封装：某节点是否为已信任好友。供 P2P 自更新分发源
// 在「混版本过渡期固定协议 ID handler」上做成员门——只有已确认好友能从
// 本机拉取二进制，公网扫描器与异群节点被挡在门外。
func (c *Client) IsPeerTrusted(peerID string) bool { return c.isTrustedPeer(peerID) }

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

// isUnfriendedPeer 供 serverless 审批门使用：本机是否主动删除过该节点
// （墓碑）。已删除节点再来握手时，协议层会回明确的 unfriended 拒绝标记，
// 让对方自动把本机从它的列表同步删除（双向删除的离线自愈路径）。
func (c *Client) isUnfriendedPeer(peerID string) bool {
	if c.peers == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := c.peers.IsUnfriended(ctx, peerID)
	if err != nil {
		return false
	}
	return ok
}

// onSeenUntrusted 供 serverless 被动发现使用：发现同群但未信任的节点时
// 落库到「附近」表，供控制台展示与一键申请连接。
func (c *Client) onSeenUntrusted(peerID string, addrs []string, source string) {
	if c.peers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.UpsertNearby(ctx, peersdb.Nearby{PeerID: peerID, Addrs: addrs, Source: source}); err != nil {
		c.logf("附近节点落库失败: %v", err)
	}
}

// onUnfriendReceived 供 serverless 使用：确认「对端已把本机删除好友」
// （在线收到 unfriend 推送，或出向握手收到 unfriended 拒绝标记）时，
// 本机同步移除该节点，完成双向删除。
//
// 移除后不记本机墓碑——是对方删的，本机可以重新申请；对方仍持有它的墓碑，
// 若本机申请连接，对方握手会再次回 unfriended，界面提示「对方已删除你，
// 需对方主动添加你」。
func (c *Client) onUnfriendReceived(peerID string) {
	if c.peers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.peers.SetTrusted(ctx, peerID, false)
	if err := c.peers.DeletePeer(ctx, peerID); err != nil {
		c.logf("双向删除：移除节点 %s 失败: %v", shortPeer(peerID), err)
		return
	}
	c.logf("已同步移除节点 %s（对方已将本机删除好友）", shortPeer(peerID))
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

// clearUnfriendedPeer 供 serverless 协议层使用：墓碑告知送达后消费掉，
// 避免删除退化成永久拉黑（对方日后的重新申请应能正常进入待审批）。
func (c *Client) clearUnfriendedPeer(peerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.clearUnfriended(ctx, peerID)
}

// clearUnfriended 清除某节点的删除墓碑（重新建立好友关系时调用）。
// 单独封装便于在多条「主动添加/同意」路径复用，失败只记日志不阻断流程。
func (c *Client) clearUnfriended(ctx context.Context, peerID string) {
	if c.peers == nil {
		return
	}
	if err := c.peers.ClearUnfriended(ctx, peerID); err != nil {
		c.logf("清除删除墓碑失败（不影响连接）: %v", err)
	}
}

// ConnectPeer 按节点 ID 发起连接（控制台统一输入框的后端）。
//
// address 可以是：
//   - 连接码（lanet://12D3Koo...@1.2.3.4:4001）：节点 ID + 直拨地址合一，
//     直接拨号，跳过一切查找（推荐，最快的连接方式）；
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
		return nil, fmt.Errorf("请输入对方连接码或节点 ID")
	}

	// 1) 连接码形式（lanet://<ID>@<ip>:<port>）：节点 ID + 直拨地址合一，
	//    直接拨号，跳过一切查找（最快的连接方式）。
	//    先在本机记为已信任（同「添加节点」语义），否则会被自己的审批门拦住。
	if invitecode.IsInviteCode(address) {
		id, addrs, derr := invitecode.ToMultiaddrs(address)
		if derr != nil {
			return nil, derr
		}
		if id.String() == c.peerID {
			return nil, fmt.Errorf("这是本机连接码，无需连接")
		}
		c.rememberPeer(id.String(), toStringAddrs(addrs), "", true)
		c.clearUnfriended(ctx, id.String())
		if len(addrs) == 0 {
			// 仅身份的连接码：退回按 ID 查找路径。
			return c.connectByID(ctx, id.String())
		}
		ai := peer.AddrInfo{ID: id, Addrs: addrs}
		connectedID, cerr := c.disc.RequestConnect(ctx, ai)
		if cerr != nil {
			return pendingOrError(connectedID, "manual", cerr)
		}
		c.noteDialSuccess(ctx, id.String(), addrs)
		res := c.resultFor(id.String(), "manual")
		res.Message = "已连接（连接码直拨）"
		return res, nil
	}

	// 2) multiaddr 形式（含 /p2p/<ID>）：直接按连接种子拨号。
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
		c.clearUnfriended(ctx, ai.ID.String())
		id, err := c.disc.DialSeed(ctx, []string{address})
		if err != nil {
			return pendingOrError(ai.ID.String(), "manual", err)
		}
		res := c.resultFor(id, "manual")
		res.Message = "已连接"
		return res, nil
	}

	// 2) 纯节点 ID：本地地址簿优先 → 私有 DHT 兜底。
	return c.connectByID(ctx, address)
}

// connectByID 纯节点 ID 连接路径：本地地址簿优先 → 私有 DHT 兜底 →
// 未命中转入后台自动发现。
func (c *Client) connectByID(ctx context.Context, address string) (*ConnResult, error) {
	id, err := peer.Decode(address)
	if err != nil {
		return nil, fmt.Errorf("节点 ID 或连接地址格式不正确：需要 12D3Koo… 形式的节点 ID、连接码（lanet://…）或 /ip4/…/p2p/… 完整地址")
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
			c.clearUnfriended(ctx, id.String())
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
	c.clearUnfriended(ctx, id.String())

	connectedID, cerr := c.disc.RequestConnect(ctx, ai)
	if cerr != nil {
		return pendingOrError(connectedID, via, cerr)
	}

	c.noteDialSuccess(ctx, id.String(), ai.Addrs)
	res := c.resultFor(id.String(), via)
	res.Message = "已连接"
	return res, nil
}

// pendingOrError 把建连错误归一化：Pending 中间态转成友好的 ConnResult，
// 其余原样返回错误。
func pendingOrError(peerID, via string, err error) (*ConnResult, error) {
	// 对方已把本机删除好友：本机的同步移除已由协议层回调完成，
	// 这里给出明确下一步指引（需对方主动添加本机）。
	if errors.Is(err, serverless.ErrUnfriended) {
		return &ConnResult{
			PeerID:  peerID,
			Pending: true,
			Via:     via,
			Message: "对方已把本机删除好友——请把本机连接码或节点 ID 发给对方，让对方在「附近」列表中找到本机并申请连接（或直接添加本机节点 ID）。",
		}, nil
	}
	// 对方尚未同意本机：这不算失败，是「申请已送达」的正常中间态。
	if errors.Is(err, serverless.ErrNotApproved) || strings.Contains(err.Error(), "等待对方同意") {
		return &ConnResult{
			PeerID:  peerID,
			Pending: true,
			Via:     via,
			Message: "已提交连接申请，等待对方在控制台同意后即可连通（同意一次即永久信任）",
		}, nil
	}
	// 对端与本节点不在同一网络：终局结论而非中间态，转成带排查指引的
	// 友好错误文案返回（前端直接展示 res.error），不再是「EOF」。
	if errors.Is(err, serverless.ErrGroupMismatch) {
		return nil, errors.New(FriendlyGroupMismatchHint)
	}
	return nil, err
}

// FriendlyGroupMismatchHint 跨网络密钥拒绝的统一友好文案（0.5.35 起）。
// 历史上这里抛的是「信息交换失败: EOF」——对端按防泄漏设计对异群握手
// 静默关流，裸 EOF 对用户毫无信息量。
const FriendlyGroupMismatchHint = "对方与本节点不在同一个网络：双方的「网络密钥」（或分发渠道）不一致。" +
	"请与对方核对并设置成相同的网络密钥后重试；若对方是官方客户端而本机是 SDK/自研集成，请确认渠道（channel）一致。"

// friendlyDialErr 把 DialSeed/ConnectSeed 上抛的错误翻译成用户可读的文案
// （控制台「连接种子」入口与「连接其他节点」共用同一语义）。
func friendlyDialErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, serverless.ErrGroupMismatch) {
		return errors.New(FriendlyGroupMismatchHint)
	}
	if errors.Is(err, serverless.ErrUnfriended) {
		return errors.New("对方已把本机删除好友——请把本机连接码发给对方，让对方在「附近」列表中找到本机并申请连接。")
	}
	return err
}

// noteDialSuccess 把首个成功地址记入地址簿拨号统计。
func (c *Client) noteDialSuccess(ctx context.Context, peerID string, addrs []ma.Multiaddr) {
	if c.peers == nil || len(addrs) == 0 {
		return
	}
	for _, a := range toStringAddrs(addrs) {
		dbCtx, dbCancel := context.WithTimeout(ctx, 3*time.Second)
		_ = c.peers.NoteDialResult(dbCtx, peerID, a, true)
		dbCancel()
		break // 记录首个成功地址即可
	}
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

// RemovePeer 删除节点（控制台「删除好友」入口）——双向语义：
//  1. 本机立即移除：撤信任、出地址簿、出成员表、记下删除墓碑；
//  2. 对方在线：经 unfriend 协议即时通知，对方自动把本机从它的列表删除；
//  3. 对方离线：它下次来握手时收到 rejection=unfriended，自动同步删除
//     （自愈式，无需双方同时在线）。
//
// 被删节点若仍可被发现，会回到双方「附近」列表，可重新申请连接。
func (c *Client) RemovePeer(peerID string) error {
	if c.peers == nil {
		return fmt.Errorf("地址簿未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 墓碑先行：确保「对方来握手时能拿到明确拒绝」，哪怕本机随后掉线。
	if err := c.peers.AddUnfriended(ctx, peerID, ""); err != nil {
		c.logf("删除墓碑写入失败（继续本机移除）: %v", err)
	}
	if err := c.peers.SetTrusted(ctx, peerID, false); err != nil {
		return err
	}
	if err := c.peers.DeletePeer(ctx, peerID); err != nil {
		return err
	}
	// 本机成员表立即清除并断开，不等 TTL。
	if c.disc != nil {
		c.disc.Forget(peerID)
	}
	// 在线即时通知（尽力而为）。送达成功 → 墓碑立即消费（它的使命只是
	// 补达离线场景的告知，已送达就不该继续拦截对方日后的重新申请）；
	// 失败 → 保留墓碑，等对方下次握手时自愈送达并被协议层消费。
	if c.disc != nil {
		go func() {
			nctx, ncancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer ncancel()
			if err := c.disc.NotifyUnfriend(nctx, peerID); err == nil {
				c.logf("已通知节点 %s：本机已删除好友，对方将同步移除本机", shortPeer(peerID))
				dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer dcancel()
				c.clearUnfriended(dctx, peerID)
			}
		}()
	}
	c.logf("已删除节点 %s（本机移除 + 墓碑生效）", shortPeer(peerID))
	return nil
}

// NearbyList 附近节点：同网络密钥内可发现、但尚未成为好友的节点
// （含被删除过的好友——可重新申请连接）。
func (c *Client) NearbyList() []peersdb.Nearby {
	if c.peers == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := c.peers.ListNearby(ctx)
	if err != nil {
		c.logf("读取附近列表失败: %v", err)
		return nil
	}
	return list
}

// ReconnectPeer 向「附近」列表中的节点重新申请连接（好友被删除后的恢复
// 入口）。优先用附近记录里缓存的地址直接拨号（免查 DHT、秒发申请），
// 没有地址则退回纯 ID 查找路径。
//
// 结果语义（复用 ConnectPeer 主链路）：
//   - 已连通 → Connected；
//   - 对方未审批 → Pending（它的待审批列表会出现本机）；
//   - 对方墓碑生效（它删过本机）→ Pending + 明确提示「需对方主动添加你」
//     （本机侧的同步移除已由协议层 OnUnfriendReceived 完成）。
func (c *Client) ReconnectPeer(ctx context.Context, peerID string) (*ConnResult, error) {
	peerID = strings.TrimSpace(peerID)
	addr := peerID
	if c.peers != nil && !strings.Contains(peerID, "/p2p/") {
		dbCtx, dbCancel := context.WithTimeout(ctx, 3*time.Second)
		list, _ := c.peers.ListNearby(dbCtx)
		dbCancel()
		for _, n := range list {
			if n.PeerID != peerID {
				continue
			}
			for _, a := range n.Addrs {
				full := a
				if !strings.Contains(full, "/p2p/") {
					full = strings.TrimSuffix(full, "/") + "/p2p/" + peerID
				}
				if m, err := ma.NewMultiaddr(full); err == nil {
					if _, err := peer.AddrInfoFromP2pAddr(m); err == nil {
						addr = full
						break
					}
				}
			}
			break
		}
	}
	return c.ConnectPeer(ctx, addr)
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
