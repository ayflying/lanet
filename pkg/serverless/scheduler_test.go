package serverless

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

type schedulerMDNS struct {
	started, closed bool
	fail            bool
}

func (m *schedulerMDNS) Start() error {
	m.started = true
	if m.fail {
		return errors.New("模拟启动失败")
	}
	return nil
}
func (m *schedulerMDNS) Close() error { m.closed = true; return nil }
func TestDiscoveryMDNSStartRollback(t *testing.T) {
	old := newDiscoveryMDNS
	defer func() { newDiscoveryMDNS = old }()
	for _, fail := range []bool{false, true} {
		m := &schedulerMDNS{fail: fail}
		newDiscoveryMDNS = func(host.Host, string, mdns.Notifee) mdns.Service { return m }
		d, err := New(context.Background(), testHost(t, false), Config{NetworkKey: "mdns-test", EnableMDNS: true, Quiet: true})
		if !m.started {
			t.Fatal("未启动mDNS")
		}
		if fail {
			if err == nil || !m.closed {
				t.Fatal("mDNS启动失败未回滚")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			_ = d.Close()
			if !m.closed {
				t.Fatal("mDNS未关闭")
			}
		}
	}
}

func TestDiscoverySchedulerBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, discoveryWorkers)
	var active, peak, calls atomic.Int32
	s := newDiscoveryScheduler(ctx, time.Minute, func(ctx context.Context, id peer.ID) error {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	for i := 0; i < discoveryWorkers; i++ {
		if _, err := s.submit(peer.ID(string(rune(i+1))), false); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < discoveryWorkers; i++ {
		<-started
	}
	for i := 0; i < 1000; i++ {
		_, _ = s.submit(peer.ID("same"), false)
	}
	for i := 0; i < 1000; i++ {
		_, _ = s.submit(peer.ID("new"+string(rune(i))), false)
	}
	s.mu.Lock()
	pending := len(s.calls)
	queued := len(s.queue)
	s.mu.Unlock()
	if pending != 136 || queued != 128 || peak.Load() != 8 || calls.Load() != 8 {
		t.Fatalf("资源计数不符 pending=%d queue=%d peak=%d calls=%d", pending, queued, peak.Load(), calls.Load())
	}
	t.Logf("1000重复+1000不同目标: worker峰值=%d, 排队=%d, 总保留=%d", peak.Load(), queued, pending)
	cancel()
	if _, err := s.submit("closed", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("关闭后仍接收: %v", err)
	}
}

func TestDiscoverySchedulerCloseReleasesQueuedCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, discoveryWorkers)
	s := newDiscoveryScheduler(ctx, time.Minute, func(ctx context.Context, _ peer.ID) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	calls := make([]*discoveryCall, 0, discoveryWorkers+discoveryQueueSize)
	for i := 0; i < discoveryWorkers; i++ {
		call, err := s.submit(peer.ID(string(rune(i+1))), false)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	for i := 0; i < discoveryWorkers; i++ {
		<-started
	}
	for i := 0; i < discoveryQueueSize; i++ {
		call, err := s.submit(peer.ID("queued"+string(rune(i))), false)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	s.close()
	for _, call := range calls {
		select {
		case <-call.done:
			if !errors.Is(call.err, context.Canceled) {
				t.Fatalf("%s 关闭结果: %v", call.id, call.err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s 关闭后未结束", call.id)
		}
	}
	s.mu.Lock()
	remaining, queued := len(s.calls), len(s.queue)
	s.mu.Unlock()
	if remaining != 0 || queued != 0 {
		t.Fatalf("关闭后引用未释放: calls=%d queue=%d", remaining, queued)
	}
}

func TestDiscoverySchedulerExplicitQueueFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, discoveryWorkers)
	s := newDiscoveryScheduler(ctx, time.Minute, func(ctx context.Context, _ peer.ID) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	for i := 0; i < discoveryWorkers; i++ {
		if _, err := s.submit(peer.ID(string(rune(i+1))), false); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < discoveryWorkers; i++ {
		<-started
	}
	for i := 0; i < discoveryQueueSize; i++ {
		if _, err := s.submit(peer.ID("queued"+string(rune(i))), false); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	err := s.request(ctx, "different-explicit-peer")
	if err == nil || err.Error() != "发现任务队列已满" || time.Since(start) > time.Second {
		t.Fatalf("显式请求队满未立即返回明确错误: err=%v elapsed=%v", err, time.Since(start))
	}
	s.close()
}

func TestDiscoveryShortTTLBoundsSuccessCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, testHost(t, false), Config{NetworkKey: "short-ttl", MemberTTL: 2 * time.Minute, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.memberTTL != 2*time.Minute || d.scheduler.cooldown <= 0 || d.scheduler.cooldown > d.memberTTL/3 {
		t.Fatalf("短TTL冷却不符: TTL=%v cooldown=%v", d.memberTTL, d.scheduler.cooldown)
	}
}

func TestDiscoveryHintsDoNotRefreshTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := testHost(t, false)
	remote := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "ttl", Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := time.Now().Add(-11 * time.Minute)
	d.members[remote.ID().String()] = &Member{PeerID: remote.ID().String(), LastSeen: old}
	d.scheduler.cancel()
	d.addMember(remote.ID(), remote.Addrs(), "dht-private")
	if !d.members[remote.ID().String()].LastSeen.Equal(old) {
		t.Fatal("发现提示虚假续命")
	}
	d.reapExpired()
	if len(d.members) != 0 {
		t.Fatal("超期成员未回收")
	}
}

func TestDiscoverySchedulerCooldownAndExplicit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var count atomic.Int32
	fail := atomic.Bool{}
	s := newDiscoveryScheduler(ctx, 120*time.Second, func(context.Context, peer.ID) error {
		count.Add(1)
		if fail.Load() {
			return errors.New("失败")
		}
		return nil
	})
	s.jitter = func(d time.Duration) time.Duration { return d }
	if err := s.request(ctx, "peer"); err != nil {
		t.Fatal(err)
	}
	if c, e := s.submit("peer", false); c != nil || e != nil {
		t.Fatal("成功冷却未生效")
	}
	if err := s.request(ctx, "peer"); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("显式未绕过冷却")
	}
	fail.Store(true)
	for i, want := range []time.Duration{30, 60, 120, 240, 300} {
		if err := s.request(ctx, "peer"); err == nil {
			t.Fatal("失败未返回")
		}
		s.mu.Lock()
		r := s.retry["peer"]
		left := time.Until(r.next)
		s.mu.Unlock()
		if r.failures != i+1 || left > want*time.Second || left < (want-1)*time.Second {
			t.Fatalf("退避不符 %+v left=%v", r, left)
		}
	}
}

func TestDiscoverySchedulerSingleFlightCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int32
	s := newDiscoveryScheduler(ctx, time.Minute, func(ctx context.Context, _ peer.ID) error {
		count.Add(1)
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	call, err := s.submit("peer", false)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < 1000; i++ {
		other, _ := s.submit("peer", false)
		if other != call {
			t.Fatal("重复目标未单飞")
		}
	}
	short, stop := context.WithCancel(ctx)
	stop()
	if err := s.request(short, "peer"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-call.done:
	case <-time.After(time.Second):
		t.Fatal("任务未结束")
	}
	if count.Load() != 1 {
		t.Fatal("重复执行")
	}
}
