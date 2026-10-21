package serverless

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

const discoveryWorkers = 8
const discoveryQueueSize = 128
const discoveryStateLimit = 4096

type discoveryCall struct {
	id             peer.ID
	done           chan struct{}
	err            error
	connectRequest bool
}
type discoveryRetry struct {
	failures int
	next     time.Time
}
type discoveryScheduler struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	once     sync.Once
	queue    chan *discoveryCall
	calls    map[peer.ID]*discoveryCall
	retry    map[peer.ID]discoveryRetry
	run      func(context.Context, peer.ID) error
	cooldown time.Duration
	now      func() time.Time
	jitter   func(time.Duration) time.Duration
}

var newDiscoveryMDNS = func(h host.Host, tag string, n mdns.Notifee) mdns.Service {
	return mdns.NewMdnsService(h, tag, n)
}

func newDiscoveryScheduler(parent context.Context, cooldown time.Duration, run func(context.Context, peer.ID) error) *discoveryScheduler {
	ctx, cancel := context.WithCancel(parent)
	return &discoveryScheduler{ctx: ctx, cancel: cancel, queue: make(chan *discoveryCall, discoveryQueueSize), calls: make(map[peer.ID]*discoveryCall), retry: make(map[peer.ID]discoveryRetry), run: run, cooldown: cooldown, now: time.Now, jitter: func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.8 + rand.Float64()*0.4)) }}
}

// 入队前去重；队满直接拒绝，不为等待容量派生协程。
func (s *discoveryScheduler) submit(id peer.ID, explicit bool) (*discoveryCall, error) {
	return s.submitMode(id, explicit, false)
}

func (s *discoveryScheduler) submitMode(id peer.ID, explicit, connectRequest bool) (*discoveryCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if call := s.calls[id]; call != nil {
		return call, nil
	}
	if !explicit && s.now().Before(s.retry[id].next) {
		return nil, nil
	}
	call := &discoveryCall{id: id, done: make(chan struct{}), connectRequest: connectRequest}
	select {
	case s.queue <- call:
		s.calls[id] = call
	default:
		return nil, errors.New("发现任务队列已满")
	}
	s.once.Do(func() {
		for i := 0; i < discoveryWorkers; i++ {
			go s.worker()
		}
	})
	return call, nil
}
func (s *discoveryScheduler) close() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		select {
		case call := <-s.queue:
			call.err = context.Canceled
			delete(s.calls, call.id)
			close(call.done)
		default:
			return
		}
	}
}

func (s *discoveryScheduler) worker() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case call := <-s.queue:
			err := s.ctx.Err()
			if err == nil {
				runCtx := s.ctx
				if call.connectRequest {
					runCtx = context.WithValue(runCtx, connectRequestKey{}, true)
				}
				err = s.run(runCtx, call.id)
			}
			s.mu.Lock()
			call.err = err
			delete(s.calls, call.id)
			r := s.retry[call.id]
			if err == nil {
				r = discoveryRetry{next: s.now().Add(s.cooldown)}
			} else {
				r.failures++
				if r.failures > 5 {
					r.failures = 5
				}
				delay := 30 * time.Second * time.Duration(1<<uint(r.failures-1))
				if delay > 5*time.Minute {
					delay = 5 * time.Minute
				}
				r.next = s.now().Add(s.jitter(delay))
			}
			if _, exists := s.retry[call.id]; exists || len(s.retry) < discoveryStateLimit {
				s.retry[call.id] = r
			} else {
				for id, v := range s.retry {
					if !s.now().Before(v.next) {
						delete(s.retry, id)
					}
				}
				if len(s.retry) < discoveryStateLimit {
					s.retry[call.id] = r
				}
			}
			close(call.done)
			s.mu.Unlock()
		}
	}
}
func (s *discoveryScheduler) request(ctx context.Context, id peer.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	connectRequest, _ := ctx.Value(connectRequestKey{}).(bool)
	for {
		call, err := s.submitMode(id, true, connectRequest)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-call.done:
			// 若先加入的是后台任务，等其结束后再提交真正申请，不能
			// 修改正在运行的任务，也不能把申请标记丢在后台单飞里。
			if connectRequest && !call.connectRequest {
				continue
			}
			return call.err
		}
	}
}
func (d *Discovery) scheduleIdentify(id peer.ID) {
	if d.scheduler != nil {
		_, _ = d.scheduler.submit(id, false)
	}
}
func (d *Discovery) identifyExplicit(ctx context.Context, id peer.ID) error {
	if d.scheduler == nil {
		return d.connectAndIdentifyContext(ctx, id)
	}
	return d.scheduler.request(ctx, id)
}
func cloneMember(m Member) Member {
	m.Addrs = append([]string(nil), m.Addrs...)
	m.LocalIPs = append([]string(nil), m.LocalIPs...)
	return m
}
