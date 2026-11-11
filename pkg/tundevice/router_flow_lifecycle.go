package tundevice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// InvalidatePath retires the current tunnel flow for target only if it still uses
// connID. Callers must use the actual probe stream's Conn().ID(), not a cached
// peer-level path label. Empty/unknown IDs, stale probes and closed routers are
// no-ops. It closes only the matching tunnel stream, never the peer connection
// or streams belonging to other protocols; Close/Reset follow retireGrace.
func (r *Router) InvalidatePath(target peer.ID, connID string) bool {
	if target == "" || connID == "" {
		return false
	}
	r.mu.Lock()
	flow := r.flows[target]
	if r.closed || flow == nil || flow.stream == nil {
		r.mu.Unlock()
		return false
	}
	conn := flow.stream.Conn()
	if conn == nil || conn.ID() != connID || !flow.retired.CompareAndSwap(false, true) {
		r.mu.Unlock()
		return false
	}
	// 核对路径、CAS 退役和摘表必须在同一个临界区：旧 probe 的迟到
	// 回调绝不能摘掉期间已经发布的新代流或它的 IPv4/IPv6 别名。
	delete(r.flows, target)
	for addr, owner := range r.flowByAddr {
		if owner == target {
			delete(r.flowByAddr, addr)
		}
	}
	r.mu.Unlock()
	r.closeRetiredFlow(flow, "path invalidated", false)
	return true
}

// reportFlowFailure feeds back only an unexpected I/O failure of the current
// live flow. EOF is a normal remote close; cancellation, local Reset and old
// generation cleanup must never quarantine a healthy shared transport path.
func (r *Router) reportFlowFailure(flow *peerFlow, cause error) bool {
	if r.tunnel == nil || cause == nil || errors.Is(cause, io.EOF) ||
		errors.Is(cause, context.Canceled) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || flow == nil || flow.retired.Load() || flow.localReset.Load() || r.flows[flow.peer] != flow {
		return false
	}
	return r.tunnel.ReportPathFailure(flow.stream, cause)
}

// acceptFlowSource binds an incoming IP source to the authenticated stream peer.
// Only the source is constrained: destination routing/forwarding and firewall
// decisions retain their existing semantics. Alias merges use r.mu as well.
func (r *Router) acceptFlowSource(flow *peerFlow, packet []byte) bool {
	var src netip.Addr
	if len(packet) >= 20 && packet[0]>>4 == 4 {
		src = netip.AddrFrom4([4]byte(packet[12:16]))
	} else if len(packet) >= 40 && packet[0]>>4 == 6 {
		src = netip.AddrFrom16([16]byte(packet[8:24]))
	} else {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if flow.retired.Load() {
		return false
	}
	for _, alias := range flow.addrs {
		if addr, err := netip.ParseAddr(alias); err == nil && addr == src {
			return true
		}
	}
	return false
}

// candidateWins 优先排除退役流，再按发起方仲裁；调用方持有 r.mu。
func (r *Router) candidateWins(existing, candidate *peerFlow) bool {
	if existing == nil {
		return true
	}
	if existing.peer != candidate.peer {
		return false
	}
	if existing.retired.Load() {
		return true
	}
	if candidate.initiator != existing.initiator {
		return candidate.initiator < existing.initiator
	}
	// 同发起方仅接收端接受重拨：发起端 single-flight 已先淘汰旧流，
	// 接收端旧 pump 可能还没退出。本机拨号结果不得反过来顶掉健康在位流。
	// 依赖同一发起端按代重拨；不支持跨连接的入向 handler 任意代际重排。
	return candidate.initiator == candidate.peer && candidate.initiator != r.selfID
}

func streamReadExitReason(stream network.Stream, err error) string {
	connection := "unknown"
	if conn := stream.Conn(); conn != nil {
		connection = conn.ID()
	}
	if errors.Is(err, io.EOF) {
		return fmt.Sprintf("closed by peer: %v (conn=%s stream=%s)", err, connection, stream.ID())
	}
	return fmt.Sprintf("read failed: %v (conn=%s stream=%s)", err, connection, stream.ID())
}
