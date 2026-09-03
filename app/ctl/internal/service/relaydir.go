package service

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/gogf/gf/v2/errors/gerror"
)

type RelayCandidate struct {
	PeerID       string    `json:"peer_id"`
	Addrs        []string  `json:"addrs"`
	Region       string    `json:"region"`
	Score        int       `json:"score"`
	CircuitCount int       `json:"circuit_count"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

type RelayRegistration struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
	Region string   `json:"region"`
	Score  int      `json:"score"`
}

type RelayHeartbeat struct {
	PeerID       string `json:"peer_id"`
	CircuitCount int    `json:"circuit_count"`
	Score        int    `json:"score"`
}

type RelayDirectory struct {
	mu         sync.RWMutex
	candidates []RelayCandidate
	updatedAt  time.Time
}

func NewRelayDirectory() *RelayDirectory {
	return &RelayDirectory{updatedAt: time.Now()}
}

func (d *RelayDirectory) List(ctx context.Context, limit int) ([]RelayCandidate, error) {
	if limit < 1 || limit > 10 {
		return nil, gerror.New("limit must be between 1 and 10")
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	items := append([]RelayCandidate(nil), d.candidates...)
	sort.Slice(items, func(i, j int) bool { return items[i].Score > items[j].Score })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (d *RelayDirectory) Replace(ctx context.Context, candidates []RelayCandidate) error {
	for _, candidate := range candidates {
		if candidate.PeerID == "" || len(candidate.Addrs) == 0 {
			return gerror.New("relay candidate requires peer_id and addrs")
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.candidates = append([]RelayCandidate(nil), candidates...)
	d.updatedAt = time.Now()
	return nil
}

func (d *RelayDirectory) Register(ctx context.Context, registration RelayRegistration) error {
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
	d.candidates = append(d.candidates, RelayCandidate{
		PeerID:     registration.PeerID,
		Addrs:      append([]string(nil), registration.Addrs...),
		Region:     registration.Region,
		Score:      registration.Score,
		LastSeenAt: now,
	})
	d.updatedAt = now
	return nil
}

func (d *RelayDirectory) Heartbeat(ctx context.Context, heartbeat RelayHeartbeat) error {
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
