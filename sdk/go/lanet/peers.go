package lanet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// 自愈：清理指向本机自己的残留行（身份漂移的历史遗留，见 PruneSelf 注释）。
	// c.peerID 在本函数调用前已就绪。失败不阻塞启动。
	if n, err := db.PruneSelf(ctx, c.peerID); err == nil && n > 0 {
		c.logf("地址簿自愈：清理了 %d 条指向本机自己的残留记录", n)
	}
	// 自愈：按「可达判据」清掉结构上永远拨不通的地址（回环 / 链路本地 /
	// overlay / circuit）。写入侧过滤只管新增，存量得靠这一遍——老版本对端
	// 每轮都会把这类地址重新通告进来（见 peersdb.PruneUnreachableAddrs）。
	if n, err := db.PruneUnreachableAddrs(ctx); err != nil {
		c.logf("地址簿自愈：清理不可达地址失败: %v", err)
	} else if n > 0 {
		c.logf("地址簿自愈：清理了 %d 条不可达的节点地址（回环/链路本地/overlay/circuit）", n)
	}
	// 自愈：修剪每个节点累积的过期地址（地址簿只增不减，单个节点攒到几十条
	// 无用地址后每次连接都要逐条试，见 peersdb.PrunePeerAddrs 注释）。放在
	// 预热之前，让预热拿到的是修剪后的高价值地址。
	if n, err := db.PrunePeerAddrs(ctx, 0); err != nil {
		// ⚠️ 这里曾经静默吞错（`err == nil && n > 0`），于是「SQL 少一个右括号、
		// 修剪从未成功」这个缺陷被掩盖了几个月：单节点地址簿涨到 110 条，
		// 每次 probe 都在上百条不可达地址上并发拨号。错误必须可见。
		c.logf("地址簿自愈：修剪节点地址失败: %v", err)
	} else if n > 0 {
		c.logf("地址簿自愈：清理了 %d 条失效的节点地址（每节点保留最近可用的若干条）", n)
	}
	known, _ := db.ListPeers(ctx, false)
	c.logf("地址簿已打开：%s（已知节点 %d 个）", db.Path(), len(known))
	// 冷启动预热：地址簿里存着上次真正拨通过的地址，入网就绪后主动重连，
	// 不必干等 DHT 把成员表慢慢填回来（用户存地址簿的初衷就是「启动快速
	// 连接」）。异步、有界、一次性，不阻塞启动。
	c.startWarmup(ctx, db)
	// 设备名对账：成员表里的名字是内存态，重启或对端长期离线后就没了。定期
	// 落进本地库，列表在离线时也能显示「这是谁」；对端改名后本地跟随更新。
	c.startNameSync(ctx, db)
	// 种子表接线：入网就绪后启用「群内 / 全域」两域种子交换，并启动纯本地的种子
	// 自举与新鲜度核对。与预热一样异步、有界、一次性，不阻塞启动。
	// db 用入参而非 c.peers：此刻调用方尚未赋值 c.peers（返回后才赋）。
	c.startSeedWiring(ctx, db)
	c.startSeedVerifier(ctx, db)
	return db, nil
}

// ---- 冷启动预热（0.5.49）----
//
// 冷启动时若只靠 DHT/种子发现，成员表要几十秒到几分钟才回满（DHT 路由表重建
// + provider 记录传播），这段时间控制台是空的——正是用户反馈的「冷启动列表
// 空」。而 peer_addrs 里就存着上次拨通过的地址（KnownAddrs 按 ok_count DESC /
// last_ok DESC 排序），直接拨它们基本是秒连。所以入网就绪后按「已信任好友 →
// 有历史地址 → 最近见过优先」取前若干个错峰重连；成功路径回写 ok_count /
// last_ok，下一轮排序更准。
const (
	// warmupReadyWait 等「入网就绪」（c.disc 就位）的上限。
	warmupReadyWait = 2 * time.Minute
	// warmupInitialDelay 入网就绪后再等一会儿，避开启动期 DHT 广播/中继预约。
	warmupInitialDelay = 8 * time.Second
	// warmupMaxPeers 单次预热的节点数上限（地址簿很大时不做全量重连）。
	warmupMaxPeers = 12
	// warmupConcurrency 同时进行的拨号数上限。
	warmupConcurrency = 3
	// warmupStagger 相邻两次拨号之间的错峰间隔。
	warmupStagger = 250 * time.Millisecond
	// warmupDialTimeout 单个节点的拨号预算。
	warmupDialTimeout = 15 * time.Second
)

// warmupTarget 一个预热目标：节点标识 + 地址簿里记录的地址（已排序）。
type warmupTarget struct {
	PeerID string
	Name   string
	Addrs  []string
}

// pickWarmupTargets 选出冷启动预热对象：
//   - 只取已信任节点（未审批的拨过去只会给对方制造待审批噪音）；
//   - 必须有历史地址（纯 ID 记录没有可直拨地址，交给 DHT 发现即可）；
//   - peers 已按 last_seen DESC 排序（ListPeers 的排序），故「最近见到的好友」
//     优先；最多 max 个。
//
// addrsOf 注入地址查询，便于单测不依赖 SQLite。
func pickWarmupTargets(peers []peersdb.Peer, addrsOf func(string) []string, max int) []warmupTarget {
	if max <= 0 || addrsOf == nil {
		return nil
	}
	out := make([]warmupTarget, 0, max)
	for _, p := range peers {
		if !p.Trusted || p.PeerID == "" {
			continue
		}
		addrs := addrsOf(p.PeerID)
		if len(addrs) == 0 {
			continue
		}
		out = append(out, warmupTarget{PeerID: p.PeerID, Name: p.Name, Addrs: addrs})
		if len(out) >= max {
			break
		}
	}
	return out
}

// waitReady 阻塞直到入网就绪（c.disc 就位）。返回 false 表示超时或 ctx 取消。
func (c *Client) waitReady(ctx context.Context, limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-c.ready:
		return c.disc != nil && ctx.Err() == nil
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// startWarmup 冷启动预热重连（一次性任务，非周期巡检）。
//
// db 显式传入而不读 c.peers：openPeersDB 返回后调用方才赋值 c.peers，直接用
// 入参可避免依赖这段时序。
func (c *Client) startWarmup(ctx context.Context, db *peersdb.DB) {
	if db == nil {
		return
	}
	go func() {
		if !c.waitReady(ctx, warmupReadyWait) {
			return // 启动失败或被取消：不预热
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(warmupInitialDelay):
		}

		listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
		peers, err := db.ListPeers(listCtx, true)
		listCancel()
		if err != nil {
			c.logf("冷启动预热：读取地址簿失败: %v", err)
			return
		}
		targets := pickWarmupTargets(peers, func(id string) []string {
			aCtx, aCancel := context.WithTimeout(ctx, 3*time.Second)
			defer aCancel()
			addrs, aerr := db.KnownAddrs(aCtx, id)
			if aerr != nil {
				return nil
			}
			return addrs
		}, warmupMaxPeers)
		if len(targets) == 0 {
			return
		}
		c.logf("冷启动预热：从地址簿取 %d 个好友优先重连（不等 DHT 发现）", len(targets))

		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			good int
			bad  int
		)
		sem := make(chan struct{}, warmupConcurrency)
		for _, t := range targets {
			if ctx.Err() != nil {
				break
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(t warmupTarget) {
				defer wg.Done()
				defer func() { <-sem }()
				pid, perr := peer.Decode(t.PeerID)
				parsed := parseAddrs(t.Addrs)
				if perr != nil || len(parsed) == 0 {
					return
				}
				dCtx, dCancel := context.WithTimeout(ctx, warmupDialTimeout)
				defer dCancel()
				_, cerr := c.disc.RequestConnect(dCtx, peer.AddrInfo{ID: pid, Addrs: parsed})
				mu.Lock()
				if cerr == nil {
					good++
				} else {
					bad++
				}
				mu.Unlock()
				if cerr == nil {
					// 回写「真正用上的」那条地址（ok_count+1 / last_ok=now）。
					c.recordDialedAddr(dCtx, pid)
				}
			}(t)
			select {
			case <-ctx.Done():
			case <-time.After(warmupStagger):
			}
		}
		wg.Wait()
		mu.Lock()
		g, b := good, bad
		mu.Unlock()
		c.logf("冷启动预热完成：已连上 %d，未连上 %d（共 %d 个好友）", g, b, len(targets))
	}()
}

// recordDialedAddr 记录一次成功连接的「实际对端地址」到地址簿拨号统计，
// 让下一轮排序更准。取真实连接的对端 multiaddr 而非猜测列表首项（首项可能
// 已失效，把失效地址记成成功会永久带偏优先级）。中继地址（p2p-circuit）
// 不入库：它是经第三方的路径，不是该节点自身的可直拨地址。
func (c *Client) recordDialedAddr(ctx context.Context, pid peer.ID) {
	if c.peers == nil || c.node == nil {
		return
	}
	for _, conn := range c.node.Network().ConnsToPeer(pid) {
		raw := conn.RemoteMultiaddr()
		if strings.Contains(raw.String(), "p2p-circuit") {
			continue
		}
		if host := stripPeerSuffix(raw); host != nil {
			c.noteDialSuccess(ctx, pid.String(), []ma.Multiaddr{host})
		}
		return
	}
}

// stripPeerSuffix 去掉 multiaddr 尾部的 /p2p/<id>，只留传输地址本体（与地址
// 簿既有条目格式一致）。尾部不是 /p2p 时原样返回；只剩 /p2p 段（无传输地址）
// 时返回 nil。
//
// 走 Component 字节切片而不是字符串拼接：字符串法会把无值协议（quic-v1、ws）
// 拼成 ".../quic-v1/" 这种多出斜杠的非法形式，StringCast 直接 panic。
func stripPeerSuffix(m ma.Multiaddr) ma.Multiaddr {
	rest, last := ma.SplitLast(m)
	if last == nil {
		return nil
	}
	if last.Protocol().Code == ma.P_P2P {
		if len(rest) == 0 {
			return nil
		}
		return rest
	}
	if len(m) == 0 {
		return nil
	}
	return m
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
	exists, err := c.peers.HasPeers(ctx, false)
	if err != nil {
		return true
	}
	return exists
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
	if c.peers == nil || (c.rootCtx != nil && c.rootCtx.Err() != nil) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.UpsertNearby(ctx, peersdb.Nearby{PeerID: peerID, Addrs: addrs, Source: source}); err != nil {
		c.logf("附近节点落库失败: %v", err)
		return
	}
	if len(addrs) > 0 {
		c.nearbyMu.Lock()
		state := c.nearbyLive[peerID]
		if c.nearbyLive != nil && state.noAddress {
			state.noAddress = false
			state.nextAttempt = time.Time{}
			c.nearbyLive[peerID] = state
		}
		c.nearbyMu.Unlock()
	}
	c.wakeNearbyProbes()
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

// ---- 设备名持久化（本地留住「谁是谁」）----
//
// 运行时成员表里的名字是**内存态**：进程重启、或对端长期离线之后名字就没了，
// 列表退化成「12D3KooW…」+ 虚拟 IP，用户根本认不出是谁。做法是把成员表里的
// 名字定期对账进本地库（peers.name），展示时以本地库兜底；对端改了名字，
// 下一轮对账就跟着改（用户要求「对方改名，下次连上就本地改一下」）。
//
// 为什么不做成写入时同步：成员表刷新与 info 交换都在连接热路径上，多一次
// 写库就多一次潜在阻塞。慢速巡检既够用，也不给连接流程添负担。
const (
	// nameSyncInitialDelay 入网就绪后多久开始第一次对账（避开启动期噪声）。
	nameSyncInitialDelay = 15 * time.Second
	// nameSyncInterval 对账周期。名字变动极不频繁，慢速足够。
	nameSyncInterval = 30 * time.Second
	// nameSyncBudget 单条写库的时间预算。
	nameSyncBudget = 10 * time.Second
)

// startNameSync 后台定期把成员表里的设备名对账进本地库（异步、有界、可取消）。
func (c *Client) startNameSync(ctx context.Context, db *peersdb.DB) {
	if db == nil {
		return
	}
	go func() {
		if !c.waitReady(ctx, warmupReadyWait) {
			return // 启动失败或被取消
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(nameSyncInitialDelay):
		}
		for {
			c.syncMemberNames(ctx, db)
			select {
			case <-ctx.Done():
				return
			case <-time.After(nameSyncInterval):
			}
		}
	}()
}

// syncMemberNames 把成员表里的名字写进本地库（名字为空则跳过），返回写入条数。
//
// 只处理成员表（同群 + 已互信/已发现），不碰「附近」列表：附近节点的名字本来
// 就来自「对端主动申请」或历史记录，写回去会形成自我强化，把一个可能已经
// 失效的名字永久钉在那儿。
func (c *Client) syncMemberNames(ctx context.Context, db *peersdb.DB) int {
	members := c.NetMap().Members
	if len(members) == 0 {
		return 0
	}
	known, err := db.NameIndex(ctx)
	if err != nil {
		c.logf("设备名对账：读取本地名称索引失败: %v", err)
		return 0
	}
	updated := 0
	for _, m := range members {
		name := strings.TrimSpace(m.Name)
		if m.PeerID == "" || name == "" {
			continue
		}
		if cur, ok := known[m.PeerID]; ok && cur.Name == name {
			continue // 名字没变：不写库（每 30s 一次的巡检不该产生无谓写入）
		}
		wctx, cancel := context.WithTimeout(ctx, nameSyncBudget)
		werr := db.SetName(wctx, m.PeerID, name)
		cancel()
		if werr != nil {
			c.logf("设备名对账：写入 %s 的名称失败: %v", shortPeer(m.PeerID), werr)
			continue
		}
		updated++
	}
	if updated > 0 {
		c.logf("设备名对账：已更新 %d 个成员的本地名称（离线后仍可显示）", updated)
	}
	return updated
}

// SetPeerNotes 写入某节点的备注（控制台「备注」入口）。空串表示清除备注。
//
// 备注是本机用户手写的别名，与对端自报的 name 并存：展示时备注优先、真名
// 次之——对端把名字改成一串乱码也不影响列表可读性。
func (c *Client) SetPeerNotes(peerID, notes string) error {
	if c.peers == nil {
		return fmt.Errorf("地址簿未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peers.SetNotes(ctx, peerID, notes); err != nil {
		return err
	}
	if notes = strings.TrimSpace(notes); notes == "" {
		c.logf("已清除节点 %s 的备注", shortPeer(peerID))
	} else {
		c.logf("已设置节点 %s 的备注：%s", shortPeer(peerID), notes)
	}
	return nil
}

// PeerNameIndex 当前本地库的全量「节点 ID → 名称/备注」索引（展示兜底用）。
// 地址簿未启用时返回空 map（调用方无需判空）。
func (c *Client) PeerNameIndex() map[string]peersdb.NameInfo {
	if c.peers == nil {
		return map[string]peersdb.NameInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	idx, err := c.peers.NameIndex(ctx)
	if err != nil {
		c.logf("读取本地名称索引失败: %v", err)
		return map[string]peersdb.NameInfo{}
	}
	return idx
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
	// AlreadyMember 为 true 表示该节点本来就在成员表 / 地址簿里（重复添加）。
	// 连接仍会照常尝试（重连是合理需求），界面据此给出「无需重复添加」提示。
	AlreadyMember bool `json:"already_member,omitempty"`
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
//
// 目标本来就在成员表 / 地址簿里时（重复添加），结果里会带 AlreadyMember=true，
// 界面据此提示「无需重复添加」——但连接照常尝试，重连本身就是合理需求。
func (c *Client) ConnectPeer(ctx context.Context, address string) (*ConnResult, error) {
	// 必须在建连写入信任之前判断，否则首次添加也会被误报为重复。
	knownBefore := c.knownMemberAddress(address)
	res, err := c.connectPeerInner(ctx, address)
	if err != nil || res == nil {
		return res, err
	}
	if res.PeerID != "" && knownBefore {
		res.AlreadyMember = true
		if res.Message == "" {
			res.Message = "该设备已在成员列表中，无需重复添加（已按重新连接处理）"
		}
	}
	return res, nil
}

// knownMemberAddress 解析三种连接输入，在任何建连副作用发生前查询信任状态。
func (c *Client) knownMemberAddress(address string) bool {
	address = strings.TrimSpace(address)
	if invitecode.IsInviteCode(address) {
		id, _, err := invitecode.ToMultiaddrs(address)
		return err == nil && c.isKnownMember(id.String())
	}
	if strings.Contains(address, "/p2p/") {
		m, err := ma.NewMultiaddr(address)
		if err != nil {
			return false
		}
		ai, err := peer.AddrInfoFromP2pAddr(m)
		return err == nil && ai != nil && c.isKnownMember(ai.ID.String())
	}
	id, err := peer.Decode(address)
	return err == nil && c.isKnownMember(id.String())
}

// isKnownMember 该节点是否已在本地地址簿里（重复添加检测）。
//
// 与 isTrustedPeer 的区别很重要：后者在「关闭连接审批」时恒返回 true，
// 拿它做重复检测会把每一次连接都误报成「已在成员列表」。
func (c *Client) isKnownMember(peerID string) bool {
	if c.peers == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := c.peers.IsTrusted(ctx, peerID)
	if err != nil {
		return false
	}
	return ok
}

// connectPeerInner ConnectPeer 的实现体（语义见 ConnectPeer 的说明）。
func (c *Client) connectPeerInner(ctx context.Context, address string) (*ConnResult, error) {
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

// FriendlyGroupMismatchHint 跨网络密钥拒绝的统一友好文案（0.5.36 起）。
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
	// 同 NearbyList：待审批同样是持久化观察表，身份漂移会留下指向自己的行。
	// 展示给自己看既无意义，点「同意」还会把自己写进信任名单。
	out := list[:0]
	for _, p := range list {
		if p.PeerID == c.peerID {
			continue
		}
		out = append(out, p)
	}
	return out
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

// DHTRoutingPeers 私有 DHT 路由表里的其他节点 ID（不含本机，可能为空）。
//
// 私有 DHT 前缀自 0.5.48 起全网共享，所以这张路由表覆盖整个 lanet 私有 DHT
// 网络——不同网络密钥、未加好友、也未与本机建连的节点都在其中。
//
// P2P 自更新用它做候选来源（「只要在 DHT 网络里发现有新版本就更新」）：被动
// 发现到的节点既不建连也不进成员表，只看成员表永远覆盖不到它们。纯本地内存
// 读取，零网络开销。
func (c *Client) DHTRoutingPeers() []string {
	if c.disc == nil {
		return nil
	}
	return c.disc.DHTRoutingPeers()
}

const (
	nearbyProbeInterval = 15 * time.Second
	nearbyProbeTTL      = 90 * time.Second
	nearbyProbeSuccess  = 60 * time.Second
	nearbyProbeFailure  = 2 * time.Minute
	nearbyProbeMaxDelay = 15 * time.Minute
	nearbyProbeLegacy   = time.Hour
	nearbyProbeTimeout  = 10 * time.Second
	nearbyProbeWorkers  = 2
	nearbyProbeQueue    = 8
)

type nearbyProbeState struct {
	name        string
	expires     time.Time
	nextAttempt time.Time
	failures    uint8
	pending     bool
	noAddress   bool
}

// 失败采用有上限的指数退避；协议不支持的旧版设备降低到每小时尝试。
func nearbyNextAttempt(now time.Time, state nearbyProbeState, err error) nearbyProbeState {
	if err == nil {
		state.failures = 0
		state.nextAttempt = now.Add(nearbyProbeSuccess)
		return state
	}
	if errors.Is(err, serverless.ErrNearbyUnsupported) {
		state.nextAttempt = now.Add(nearbyProbeLegacy)
		return state
	}
	if errors.Is(err, serverless.ErrNearbyCooldown) {
		state.nextAttempt = now.Add(nearbyProbeFailure)
		return state
	}
	if state.failures < 4 {
		state.failures++
	}
	delay := nearbyProbeFailure << (state.failures - 1)
	if delay > nearbyProbeMaxDelay {
		delay = nearbyProbeMaxDelay
	}
	state.nextAttempt = now.Add(delay)
	return state
}

// startNearbyProbes 只从后台巡检持久化候选；HTTP 列表请求不发起网络拨号。
func (c *Client) startNearbyProbes() {
	c.nearbyMu.Lock()
	c.nearbyLive = make(map[string]nearbyProbeState)
	c.nearbyWake = make(chan struct{}, 1)
	wake := c.nearbyWake
	c.nearbyMu.Unlock()
	jobs := make(chan peersdb.Nearby, nearbyProbeQueue)
	c.nearbyWG.Add(nearbyProbeWorkers + 1)
	for range nearbyProbeWorkers {
		go func() {
			defer c.nearbyWG.Done()
			for {
				select {
				case <-c.rootCtx.Done():
					return
				case n := <-jobs:
					if c.rootCtx.Err() != nil {
						return
					}
					c.probeNearby(n)
				}
			}
		}()
	}
	go func() {
		defer c.nearbyWG.Done()
		ticker := time.NewTicker(nearbyProbeInterval)
		defer ticker.Stop()
		c.scanNearbyProbes(jobs)
		for {
			select {
			case <-c.rootCtx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
			c.scanNearbyProbes(jobs)
		}
	}()
}

func (c *Client) wakeNearbyProbes() {
	c.nearbyMu.Lock()
	wake := c.nearbyWake
	c.nearbyMu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (c *Client) scanNearbyProbes(jobs chan<- peersdb.Nearby) {
	if c.rootCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.rootCtx, 3*time.Second)
	defer cancel()
	list, err := c.peers.ListNearby(ctx)
	if err != nil {
		return
	}
	trusted, err := c.peers.TrustedIDs(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	c.nearbyMu.Lock()
	defer c.nearbyMu.Unlock()
	present := make(map[string]bool, len(list))
	// 在线重验优先于新候选，新候选优先于失败旧记录；避免持续新增把在线设备挤到 TTL 过期。
	for priority := 0; priority < 3; priority++ {
		for _, n := range list {
			if n.PeerID == c.peerID || trusted[n.PeerID] {
				continue
			}
			present[n.PeerID] = true
			state := c.nearbyLive[n.PeerID]
			if state.pending || now.Before(state.nextAttempt) {
				continue
			}
			_, observed := c.nearbyLive[n.PeerID]
			class := 2 // 已失败或过期的旧候选最后重试。
			if observed && now.Before(state.expires) {
				class = 0
			} else if !observed {
				class = 1
			}
			if priority != class {
				continue
			}
			if len(n.Addrs) == 0 {
				pid, err := peer.Decode(n.PeerID)
				if err != nil || (len(c.node.Peerstore().Addrs(pid)) == 0 && len(c.node.Network().ConnsToPeer(pid)) == 0) {
					state.noAddress = true
					c.nearbyLive[n.PeerID] = state
					continue
				}
			}
			state.noAddress = false
			select {
			case jobs <- n:
				state.pending = true
				c.nearbyLive[n.PeerID] = state
			default:
				// 队列满时下一轮再评估，不推迟未入队节点的下一次机会。
				c.nearbyLive[n.PeerID] = state
			}
		}
	}
	for id := range c.nearbyLive {
		if !present[id] {
			delete(c.nearbyLive, id)
		}
	}
}

func (c *Client) probeNearby(n peersdb.Nearby) {
	ctx, cancel := context.WithTimeout(c.rootCtx, nearbyProbeTimeout)
	defer cancel()
	trusted, trustErr := c.peers.IsTrusted(ctx, n.PeerID)
	name, err := "", trustErr
	if err == nil && !trusted && n.PeerID != c.peerID {
		pid, decodeErr := peer.Decode(n.PeerID)
		if decodeErr != nil {
			err = decodeErr
		} else {
			ai := peer.AddrInfo{ID: pid, Addrs: parseAddrs(n.Addrs)}
			if len(ai.Addrs) == 0 && len(c.node.Peerstore().Addrs(pid)) == 0 && len(c.node.Network().ConnsToPeer(pid)) == 0 {
				err = errors.New("附近设备无已观察地址")
			} else {
				result, probeErr := c.disc.ProbeNearby(ctx, ai)
				name, err = result.Name, probeErr
				if err == nil && !result.Alive {
					err = errors.New("附近设备未确认在线")
				}
			}
		}
	}
	if err == nil {
		trusted, err = c.peers.IsTrusted(ctx, n.PeerID)
	}
	if err == nil {
		// 候选可能在拨号期间被移出持久化观察表，不能复活已过期结果。
		list, listErr := c.peers.ListNearby(ctx)
		err = listErr
		found := false
		for _, item := range list {
			if item.PeerID == n.PeerID {
				found = true
				break
			}
		}
		if !found && err == nil {
			err = errors.New("附近候选已移除")
		}
	}
	c.nearbyMu.Lock()
	defer c.nearbyMu.Unlock()
	if c.rootCtx.Err() != nil {
		return
	}
	state, exists := c.nearbyLive[n.PeerID]
	if !exists {
		return // 扫描已删掉该候选，不能由进行中的探测把它重新写回。
	}
	state.pending = false
	now := time.Now()
	if trusted || n.PeerID == c.peerID {
		state.expires = time.Time{}
		state.name = ""
		state.nextAttempt = now.Add(nearbyProbeLegacy)
	} else if errors.Is(err, serverless.ErrNearbyCooldown) {
		state = nearbyNextAttempt(now, state, err)
	} else if err != nil {
		state.expires = time.Time{}
		state.name = ""
		state = nearbyNextAttempt(now, state, err)
	} else {
		state.expires = now.Add(nearbyProbeTTL)
		state.name = strings.TrimSpace(name)
		state = nearbyNextAttempt(now, state, nil)
	}
	c.nearbyLive[n.PeerID] = state
}

// NearbyList 只返回近期通过在线探测的未信任附近设备（含被删除的好友）。
// 持久化观察行本身不代表在线；旧版本无新协议、失败或 TTL 到期均隐藏，
// 但不删除地址簿中的候选，供后台下一轮重试。已信任及本机自身始终过滤。
func (c *Client) NearbyList() []peersdb.Nearby {
	if c.peers == nil || (c.rootCtx != nil && c.rootCtx.Err() != nil) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	list, err := c.peers.ListNearby(ctx)
	if err != nil {
		c.logf("读取附近列表失败: %v", err)
		return nil
	}
	trusted, err := c.peers.TrustedIDs(ctx)
	if err != nil {
		c.logf("读取信任列表失败（附近列表暂不展示）: %v", err)
		return nil
	}
	idx, ierr := c.peers.NameIndex(ctx)
	if ierr != nil {
		c.logf("读取本地名称索引失败（附近列表将只显示设备自报名）: %v", ierr)
	}
	c.nearbyMu.Lock()
	defer c.nearbyMu.Unlock()
	out := make([]peersdb.Nearby, 0, len(list))
	for _, n := range list {
		if n.PeerID == c.peerID || trusted[n.PeerID] {
			continue
		}
		state, ok := c.nearbyLive[n.PeerID]
		if !ok || !time.Now().Before(state.expires) {
			continue
		}
		// 名称只来自本轮成功探测；历史数据库名称不能伪装成设备自报。
		n.Name = state.name
		n.Notes = idx[n.PeerID].Notes
		out = append(out, n)
	}
	return out
}

// dropSelfNearby 剔除附近列表里指向本机自己的条目（保留原顺序）。
func dropSelfNearby(list []peersdb.Nearby, selfPeerID string) []peersdb.Nearby {
	if selfPeerID == "" {
		return list
	}
	out := list[:0]
	for _, n := range list {
		if n.PeerID == selfPeerID {
			continue
		}
		out = append(out, n)
	}
	return out
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
