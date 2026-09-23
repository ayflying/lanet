package tunnel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

// 隧道策略：同一群组成员之间建立流式连接。
// 1. 对端通告了可达地址 → 先直连（P2P，带宽不受中继限制）。
// 2. 直连失败 → 从 RelaySource 取候选中继，Circuit Relay v2 预约后经 /p2p-circuit 转发保底。
// 3. 建立连接后打开 /pvn/tunnel/1.0.0 流收发数据。

// GroupNetMap 隧道服务所需的 NetMap 能力；由 netmap.Client 实现。
type GroupNetMap interface {
	Resolve(virtualIP string) (netmapclient.Route, bool)
}

// RelaySource 提供可用中继候选；由 peersource.Client（控制面候选接口）实现。
type RelaySource interface {
	Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error)
}

type Service struct {
	mu          sync.Mutex
	self        host.Host
	netmapCli   GroupNetMap
	relays      RelaySource
	dialTimeout time.Duration
	relayUsed   map[string]pathRecord
	dials       map[peer.ID]*dialFlight
	activeDials int
	waiters     int
}

const (
	maxConcurrentDials = 8
	maxDialWaiters     = 32
	totalDialTimeout   = 20 * time.Second
	maxPathRecords     = 4096
	pathRecordTTL      = 30 * time.Minute
)

type pathRecord struct {
	relay bool
	at    time.Time
}

type dialFlight struct {
	done chan struct{}
	err  error
}

func New(self host.Host, netmapCli GroupNetMap, relays RelaySource) *Service {
	return &Service{
		self:        self,
		netmapCli:   netmapCli,
		relays:      relays,
		dialTimeout: 8 * time.Second,
		relayUsed:   make(map[string]pathRecord),
		dials:       make(map[peer.ID]*dialFlight),
	}
}

// OpenStreamToVirtualIP 按虚拟 IP 连接对端并打开隧道流。
// 返回流与是否经中继（用于状态展示与带宽诊断）。
func (s *Service) OpenStreamToVirtualIP(ctx context.Context, virtualIP string) (network.Stream, bool, error) {
	return s.OpenStreamToVirtualIPProtocol(ctx, virtualIP, protocol.Tunnel)
}

// OpenStreamToVirtualIPProtocol 同上，但指定应用层协议 ID
// （如 portfwd 端口转发），三段降级策略与隧道流一致。
func (s *Service) OpenStreamToVirtualIPProtocol(ctx context.Context, virtualIP string, proto libprotocol.ID) (network.Stream, bool, error) {
	return s.OpenStreamToVirtualIPProtocols(ctx, virtualIP, []libprotocol.ID{proto})
}

// OpenStreamToVirtualIPProtocols 同 OpenStreamToVirtualIPProtocol，但一次
// 传入多个候选协议 ID（按优先级）。multistream 会用对端实际支持的第一个：
// 用于「同群新老版本混跑」过渡期（如探测协议既发派生 ID 又发历史固定 ID），
// 省去协商失败后再拨一次的往返。
func (s *Service) OpenStreamToVirtualIPProtocols(ctx context.Context, virtualIP string, protos []libprotocol.ID) (_ network.Stream, _ bool, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, totalDialTimeout)
	defer cancel()
	route, ok := s.netmapCli.Resolve(virtualIP)
	if !ok {
		return nil, false, fmt.Errorf("virtual IP %s not in group netmap", virtualIP)
	}
	target, err := peer.Decode(route.PeerID)
	if err != nil {
		return nil, false, fmt.Errorf("decode peer id %q: %w", route.PeerID, err)
	}

	// 同目标复用建连结果，不共享应用流；等待人数同样有界。
	s.mu.Lock()
	if flight := s.dials[target]; flight != nil {
		if s.waiters >= maxDialWaiters {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("tunnel dial wait limit reached")
		}
		s.waiters++
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.waiters--; s.mu.Unlock() }()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-flight.done:
		}
		// 首个调用的协议协商失败不代表此调用的协议也不受支持；
		// 只有本次上下文取消才停止，随后独立尝试所需协议。
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if s.self.Network().Connectedness(target) != network.Connected {
			if flight.err != nil {
				return nil, false, flight.err
			}
			return nil, false, fmt.Errorf("tunnel connection to %s lost before opening stream", target)
		}
		s.mu.Lock()
		if s.activeDials >= maxConcurrentDials {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("tunnel concurrent dial limit reached")
		}
		s.activeDials++
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.activeDials--; s.mu.Unlock() }()
		stream, err := s.openStream(ctx, target, protos...)
		if err != nil {
			return nil, false, err
		}
		return stream, hasCircuit(stream.Conn().RemoteMultiaddr()), nil
	}
	if s.activeDials >= maxConcurrentDials {
		s.mu.Unlock()
		return nil, false, fmt.Errorf("tunnel concurrent dial limit reached")
	}
	flight := &dialFlight{done: make(chan struct{})}
	s.dials[target] = flight
	s.activeDials++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		flight.err = resultErr
		delete(s.dials, target)
		s.activeDials--
		close(flight.done)
		s.mu.Unlock()
	}()

	dialCtx, cancel := context.WithTimeout(ctx, s.dialTimeout)
	defer cancel()

	// 1) 已有连接直接开流（可能由打洞/AutoRelay/此前 probe 建立）。
	//    必须最先尝试：对同一 peer 反复 Connect 会触发 libp2p dial
	//    backoff，且 addrs 里常混有 127.0.0.1/169.254 等不可达地址，
	//    逐个试会耗尽 dialTimeout——而此时连接明明是健康的。
	if s.self.Network().Connectedness(target) == network.Connected {
		stream, streamErr := s.openStream(dialCtx, target, protos...)
		if streamErr == nil {
			viaRelay := hasCircuit(stream.Conn().RemoteMultiaddr())
			s.markRelay(route.PeerID, viaRelay)
			return stream, viaRelay, nil
		}
	}

	// 2) 主动建连：不带 Addrs，让 libp2p 用 peerstore 中的全部已知地址
	//    （含此前 probe/identify 学到的内网直连地址）。带 Addrs 强制拨号
	//    会踩通告地址里的脏数据（观测到的 NAT 公网地址 hairpin 不通等），
	//    反而错过物理可达的内网路径。
	directErr := s.self.Connect(dialCtx, peer.AddrInfo{ID: target})
	if directErr == nil {
		stream, streamErr := s.openStream(dialCtx, target, protos...)
		if streamErr == nil {
			s.markRelay(route.PeerID, false)
			return stream, false, nil
		}
		directErr = streamErr
	}

	// 3) 中继保底：逐个候选 Relay 预约，成功即经中继转发。
	stream, relayErr := s.openViaRelay(ctx, target, protos...)
	if relayErr != nil {
		return nil, false, fmt.Errorf("direct and relay dial both failed: direct=%s relay=%s",
			describeDirect(route, directErr), compactError(relayErr))
	}
	s.markRelay(route.PeerID, true)
	return stream, true, nil
}

func (s *Service) openStream(ctx context.Context, target peer.ID, protos ...libprotocol.ID) (network.Stream, error) {
	return s.self.NewStream(ctx, target, protos...)
}

func (s *Service) openViaRelay(ctx context.Context, target peer.ID, protos ...libprotocol.ID) (network.Stream, error) {
	candidates, err := s.relays.Candidates(ctx, 2)
	if err != nil {
		return nil, fmt.Errorf("fetch relay candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no relay candidates available")
	}
	var lastErr error
	if len(candidates) > 2 {
		candidates = candidates[:2]
	}
	for _, candidate := range candidates {
		if candidate.ID == target || len(candidate.Addrs) == 0 {
			continue // 目标自己不能当中继（直连都失败了，自我中继无意义）
		}
		reserveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, reserveErr := client.Reserve(reserveCtx, s.self, candidate)
		cancel()
		if reserveErr != nil {
			lastErr = fmt.Errorf("reserve on %s: %w", candidate.ID, reserveErr)
			continue
		}
		circuitAddr := candidate.Addrs[0].
			Encapsulate(ma.StringCast("/p2p/" + candidate.ID.String())).
			Encapsulate(ma.StringCast("/p2p-circuit/p2p/" + target.String()))
		connectCtx, cancelConnect := context.WithTimeout(ctx, s.dialTimeout)
		connectErr := s.self.Connect(connectCtx, peer.AddrInfo{ID: target, Addrs: []ma.Multiaddr{circuitAddr}})
		cancelConnect()
		if connectErr != nil {
			lastErr = fmt.Errorf("connect via %s: %w", candidate.ID, connectErr)
			continue
		}
		stream, streamErr := s.openStream(ctx, target, protos...)
		if streamErr != nil {
			lastErr = fmt.Errorf("open stream via %s: %w", candidate.ID, streamErr)
			continue
		}
		return stream, nil
	}
	if lastErr == nil {
		// 候选全被排除（如唯一候选就是目标自身）等情况。
		lastErr = fmt.Errorf("no usable relay candidate (excluded target %s)", target)
	}
	return nil, lastErr
}

func (s *Service) markRelay(peerID string, viaRelay bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var oldest string
	var oldestAt time.Time
	for id, record := range s.relayUsed {
		if now.Sub(record.at) >= pathRecordTTL {
			delete(s.relayUsed, id)
			continue
		}
		if oldest == "" || record.at.Before(oldestAt) {
			oldest, oldestAt = id, record.at
		}
	}
	if _, exists := s.relayUsed[peerID]; !exists && len(s.relayUsed) >= maxPathRecords {
		delete(s.relayUsed, oldest)
	}
	s.relayUsed[peerID] = pathRecord{relay: viaRelay, at: now}
}

// LastPathUsed 返回对端最近一次链路类型：direct / relay / unknown。
// LastPathUsed 返回到对端最近一次链路类型：direct / relay / offline / unknown。
// 有本端拨号记录时返回记录值；否则按当前实际连接状态实时判定
// （未连接 = offline，经 /p2p-circuit = relay，否则 direct）。
func (s *Service) LastPathUsed(peerID string) string {
	s.mu.Lock()
	if used, ok := s.relayUsed[peerID]; ok && time.Since(used.at) < pathRecordTTL {
		s.mu.Unlock()
		if used.relay {
			return "relay"
		}
		return "direct"
	}
	s.mu.Unlock()
	s.mu.Lock()
	if record, ok := s.relayUsed[peerID]; ok && time.Since(record.at) >= pathRecordTTL {
		delete(s.relayUsed, peerID)
	}
	s.mu.Unlock()
	return s.connPath(peerID)
}

// connPath 按当前连接实时判定链路类型（不依赖历史拨号记录，
// 被动入向连接、从未主动拨号过的成员也能正确显示）。
func (s *Service) connPath(peerID string) string {
	id, err := peer.Decode(peerID)
	if err != nil {
		return "unknown"
	}
	conns := s.self.Network().ConnsToPeer(id)
	if len(conns) == 0 {
		return "offline"
	}
	for _, c := range conns {
		if hasCircuit(c.RemoteMultiaddr()) {
			return "relay"
		}
	}
	return "direct"
}

func hasCircuit(address ma.Multiaddr) bool {
	if address == nil {
		return false
	}
	_, err := address.ValueForProtocol(ma.P_CIRCUIT)
	return err == nil
}

func describeDirect(route netmapclient.Route, err error) string {
	if err != nil {
		return fmt.Sprintf("peer=%s addrs=%d err=%s", route.PeerID, len(route.Addrs), compactError(err))
	}
	return fmt.Sprintf("peer=%s addrs=%d (no address or already connected)", route.PeerID, len(route.Addrs))
}

// compactError 把错误压成单行短摘要。libp2p 的 DialError 会把一次拨号的
// 每个失败地址展开为独立一行（"  * [addr] cause"），peerstore 里十几个地址
// 全部失败时单条日志就膨胀十几行，probe 每轮对不可达成员重试会持续刷屏。
// 这里压成一行：首行 + 失败地址数与「原因×次数」聚合摘要。
func compactError(err error) string {
	if err == nil {
		return "<nil>"
	}
	message := err.Error()
	if idx := strings.IndexByte(message, '\n'); idx >= 0 {
		head := strings.TrimSpace(message[:idx])
		detail := message[idx+1:]
		if n := strings.Count(detail, "* ["); n > 0 {
			message = fmt.Sprintf("%s（%d 个地址全部失败: %s）", head, n, summarizeDialCauses(detail))
		} else {
			message = head
		}
	}
	const maxLength = 512
	if len(message) > maxLength {
		message = message[:maxLength] + "..."
	}
	return message
}

// summarizeDialCauses 把逐地址失败明细聚合为「原因×次数」摘要，按次数降序。
func summarizeDialCauses(detail string) string {
	type causeCount struct {
		cause string
		n     int
	}
	counts := map[string]int{}
	for _, line := range strings.Split(detail, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "* [") {
			continue
		}
		if i := strings.Index(line, "] "); i >= 0 {
			if cause := strings.TrimSpace(line[i+2:]); cause != "" {
				counts[cause]++
			}
		}
	}
	list := make([]causeCount, 0, len(counts))
	for cause, n := range counts {
		list = append(list, causeCount{cause, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	parts := make([]string, 0, len(list))
	for _, it := range list {
		parts = append(parts, fmt.Sprintf("%s×%d", it.cause, it.n))
	}
	return strings.Join(parts, ", ")
}
