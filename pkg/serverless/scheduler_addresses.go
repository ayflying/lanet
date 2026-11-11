package serverless

import (
	"sort"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const discoveryAddressWakeInterval = 30 * time.Second

// noteAddresses 给最新地址一次有限唤醒机会，绝不每个包/每条线索清退避。
// 只用有界清洗后的地址集合（忽略排序）去重；同一peer最多30秒一次，
// 保留失败次数以免地址轮换把长期离线节点的重试预算清零。
func (s *discoveryScheduler) noteAddresses(id peer.ID, addrs []ma.Multiaddr) {
	clean := p2pkit.CleanUnderlayAddrs(addrs)
	if len(clean) == 0 {
		return
	}
	keys := make([]string, 0, len(clean))
	for _, a := range clean {
		keys = append(keys, a.String())
	}
	sort.Strings(keys)
	fingerprint := strings.Join(keys, "\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r, exists := s.retry[id]
	if !exists && len(s.retry) >= discoveryStateLimit {
		return
	}
	if r.addresses == fingerprint {
		return
	}
	first := r.addresses == ""
	r.addresses = fingerprint
	if !first && !now.Before(r.wakeAfter) {
		r.generation++
		r.next = now
		r.wakeAfter = now.Add(discoveryAddressWakeInterval)
	} else if first {
		r.wakeAfter = now.Add(discoveryAddressWakeInterval)
	}
	s.retry[id] = r
}
