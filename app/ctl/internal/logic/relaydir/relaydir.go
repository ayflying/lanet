// =================================================================================
// 中继目录 logic 实现：内存版候选目录，按评分排序供 agent 选择。
// =================================================================================

package relaydir

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/model"
	"github.com/ayflying/pvn/app/ctl/internal/service"
	"github.com/gogf/gf/v2/errors/gerror"
)

type RelayDirectory struct {
	mu         sync.RWMutex
	candidates []model.RelayCandidate
	updatedAt  time.Time
}

func NewRelayDirectory() *RelayDirectory {
	return &RelayDirectory{updatedAt: time.Now()}
}

// staleAfter 候选失联阈值：超过该时长没有心跳/注册刷新的候选视为失效，
// 不再返回给 agent（agent 拿到失效候选会在拨号上浪费时间）。
// relay 端心跳间隔 30s，阈值取其 5 倍以容忍偶发丢心跳。
const staleAfter = 150 * time.Second

func (d *RelayDirectory) List(ctx context.Context, limit int) ([]model.RelayCandidate, error) {
	if limit < 1 || limit > 10 {
		return nil, gerror.New("limit must be between 1 and 10")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	// 顺带清理失联候选（List 是最高频访问点，在这里惰性清理免维护后台协程）。
	alive := d.candidates[:0]
	for _, item := range d.candidates {
		if now.Sub(item.LastSeenAt) <= staleAfter {
			alive = append(alive, item)
		}
	}
	d.candidates = alive

	items := append([]model.RelayCandidate(nil), d.candidates...)
	sort.Slice(items, func(i, j int) bool { return items[i].Score > items[j].Score })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (d *RelayDirectory) Register(ctx context.Context, registration service.RelayRegistration) error {
	if registration.PeerID == "" || len(registration.Addrs) == 0 {
		return gerror.New("relay registration requires peer_id and addrs")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for index := range d.candidates {
		if d.candidates[index].PeerID == registration.PeerID {
			d.candidates[index].Addrs = append([]string(nil), registration.Addrs...)
			d.candidates[index].Region = registration.Region
			d.candidates[index].Score = registration.Score
			d.candidates[index].LastSeenAt = now
			d.updatedAt = now
			return nil
		}
	}
	d.candidates = append(d.candidates, model.RelayCandidate{
		PeerID:     registration.PeerID,
		Addrs:      append([]string(nil), registration.Addrs...),
		Region:     registration.Region,
		Score:      registration.Score,
		LastSeenAt: now,
	})
	d.updatedAt = now
	return nil
}

func (d *RelayDirectory) Heartbeat(ctx context.Context, heartbeat service.RelayHeartbeat) error {
	if heartbeat.PeerID == "" {
		return gerror.New("relay heartbeat requires peer_id")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for index := range d.candidates {
		if d.candidates[index].PeerID == heartbeat.PeerID {
			d.candidates[index].CircuitCount = heartbeat.CircuitCount
			d.candidates[index].Score = heartbeat.Score
			d.candidates[index].LastSeenAt = time.Now()
			d.updatedAt = d.candidates[index].LastSeenAt
			return nil
		}
	}
	return gerror.New("relay is not registered")
}

func (d *RelayDirectory) UpdatedAt() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.updatedAt
}
