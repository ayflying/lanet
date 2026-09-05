package tunnel

import (
	"context"
	"fmt"
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
	relayUsed   map[string]bool // 对端最近一次是否经中继
}

func New(self host.Host, netmapCli GroupNetMap, relays RelaySource) *Service {
	return &Service{
		self:        self,
		netmapCli:   netmapCli,
		relays:      relays,
		dialTimeout: 8 * time.Second,
		relayUsed:   make(map[string]bool),
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
	route, ok := s.netmapCli.Resolve(virtualIP)
	if !ok {
		return nil, false, fmt.Errorf("virtual IP %s not in group netmap", virtualIP)
	}
	target, err := peer.Decode(route.PeerID)
	if err != nil {
		return nil, false, fmt.Errorf("decode peer id %q: %w", route.PeerID, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, s.dialTimeout)
	defer cancel()

	// 1) 直连尝试：用通告地址主动建连。
	var directErr error
	if len(route.Addrs) > 0 {
		addrs := make([]ma.Multiaddr, 0, len(route.Addrs))
		for _, raw := range route.Addrs {
			if addr, addrErr := ma.NewMultiaddr(raw); addrErr == nil {
				addrs = append(addrs, addr)
			}
		}
		if len(addrs) > 0 {
			directErr = s.self.Connect(dialCtx, peer.AddrInfo{ID: target, Addrs: addrs})
			if directErr == nil {
				stream, streamErr := s.openStream(dialCtx, target, proto)
				if streamErr == nil {
					s.markRelay(route.PeerID, false)
					return stream, false, nil
				}
				directErr = streamErr
			}
		}
	}

	// 2) 已有连接（可能由打洞/AutoRelay 建立）直接开流。
	if s.self.Network().Connectedness(target) == network.Connected {
		stream, streamErr := s.openStream(dialCtx, target, proto)
		if streamErr == nil {
			viaRelay := hasCircuit(stream.Conn().RemoteMultiaddr())
			s.markRelay(route.PeerID, viaRelay)
			return stream, viaRelay, nil
		}
	}

	// 3) 中继保底：逐个候选 Relay 预约，成功即经中继转发。
	stream, relayErr := s.openViaRelay(ctx, target, proto)
	if relayErr != nil {
		return nil, false, fmt.Errorf("direct and relay dial both failed: direct=%v relay=%v",
			describeDirect(route, directErr), relayErr)
	}
	s.markRelay(route.PeerID, true)
	return stream, true, nil
}

func (s *Service) openStream(ctx context.Context, target peer.ID, proto libprotocol.ID) (network.Stream, error) {
	return s.self.NewStream(ctx, target, proto)
}

func (s *Service) openViaRelay(ctx context.Context, target peer.ID, proto libprotocol.ID) (network.Stream, error) {
	candidates, err := s.relays.Candidates(ctx, 2)
	if err != nil {
		return nil, fmt.Errorf("fetch relay candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no relay candidates available")
	}
	var lastErr error
	for _, candidate := range candidates {
		if candidate.ID == target {
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
		stream, streamErr := s.openStream(ctx, target, proto)
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
	s.relayUsed[peerID] = viaRelay
}

// LastPathUsed 返回对端最近一次链路类型：direct / relay / unknown。
// LastPathUsed 返回到对端最近一次链路类型：direct / relay / offline / unknown。
// 有本端拨号记录时返回记录值；否则按当前实际连接状态实时判定
// （未连接 = offline，经 /p2p-circuit = relay，否则 direct）。
func (s *Service) LastPathUsed(peerID string) string {
	s.mu.Lock()
	if used, ok := s.relayUsed[peerID]; ok {
		s.mu.Unlock()
		if used {
			return "relay"
		}
		return "direct"
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
		return fmt.Sprintf("peer=%s addrs=%v err=%v", route.PeerID, route.Addrs, err)
	}
	return fmt.Sprintf("peer=%s addrs=%v (no address or already connected)", route.PeerID, route.Addrs)
}
