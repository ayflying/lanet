package serverless

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
)

const relayReservationStateLimit = 64

type relayReservationRecord struct {
	retry, expires time.Time
	failures       uint8
}
type relayReservationState struct {
	mu      sync.Mutex
	records map[peer.ID]relayReservationRecord
	cursor  int
}

func rotateReservationCandidates(candidates []peer.AddrInfo, cursor *int) []peer.AddrInfo {
	if len(candidates) == 0 {
		return nil
	}
	start := *cursor % len(candidates)
	ordered := append([]peer.AddrInfo(nil), candidates[start:]...)
	ordered = append(ordered, candidates[:start]...)
	*cursor = (start + 2) % len(candidates)
	return ordered
}

// reserveManaged 不每分钟无条件续租全部节点。有效预约在到期前两分钟才续租；
// 失败按分钟级退避，且每轮最多尝试两个新的/到期候选，避免控制协作放大拨号。
// 不主动清除旧预约：它们按服务端TTL自然回收，状态满时拒绝新增而非驱逐预算。
func (d *Discovery) reserveManaged(ctx context.Context, candidates []peer.AddrInfo) error {
	state := &d.relayReservations
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.records == nil {
		state.records = make(map[peer.ID]relayReservationRecord)
	}
	now := time.Now()
	for id, r := range state.records {
		if !r.expires.IsZero() && now.After(r.expires.Add(time.Hour)) || r.expires.IsZero() && now.After(r.retry.Add(time.Hour)) {
			delete(state.records, id)
		}
	}
	attempts, valid := 0, 0
	// 控制握手并发小于候选数，轮转避免末尾候选长期饥饿、证明过期。
	candidates = rotateReservationCandidates(candidates, &state.cursor)
	for _, ai := range candidates {
		if ctx.Err() != nil {
			break
		}
		// 即使预约尚有效也刷新短期同组证明，否则长期预约会在
		// 五分钟证明过期后无法转发。握手单独限频/并发，不续租或扩预约。
		d.queueControlMode(ai, false)
		r, exists := state.records[ai.ID]
		if now.Before(r.expires.Add(-2 * time.Minute)) {
			valid++
			continue
		}
		if now.Before(r.retry) || attempts >= 2 {
			continue
		}
		if !exists && len(state.records) >= relayReservationStateLimit {
			continue
		}
		attempts++
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		reservation, err := client.Reserve(cctx, d.host, ai)
		cancel()
		if err == nil {
			r = relayReservationRecord{expires: reservation.Expiration}
			valid++
		} else {
			if r.failures < 4 {
				r.failures++
			}
			r.retry = now.Add(time.Minute * time.Duration(1<<uint(r.failures-1)))
		}
		state.records[ai.ID] = r
	}
	if valid > 0 {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("no valid relay reservation (%d bounded attempts)", attempts)
}
