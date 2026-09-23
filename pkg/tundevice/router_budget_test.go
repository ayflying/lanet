package tundevice

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestReaperHonorsBothDirectionsAndInFlightWrite(t *testing.T) {
	r := New(nil, nil)
	st := &streamState{}
	past := time.Now().Add(-time.Minute)
	st.lastActivity.Store(past.UnixNano())
	st.writing.Add(1)
	r.streams["busy"] = st
	r.reapIdle(time.Now(), 5*time.Second)
	if r.streams["busy"] != st {
		t.Fatal("在途写期间误回收")
	}
	st.lastActivity.Store(time.Now().UnixNano()) // 单向出站写也应刷新活动
	st.writing.Add(-1)
	r.reapIdle(time.Now(), 5*time.Second)
	if r.streams["busy"] != st {
		t.Fatal("单向写活动后误回收")
	}
	st.lastActivity.Store(time.Now().UnixNano()) // 单向入站Read同样刷新该字段
	r.reapIdle(time.Now(), 5*time.Second)
	if r.streams["busy"] != st {
		t.Fatal("单向读活动后误回收")
	}
}

func TestFailedDialPreservesConcurrentInboundStream(t *testing.T) {
	a, b := newPair(t)
	offline, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatal(err)
	}
	offlineID := offline.ID()
	_ = offline.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inbound := make(chan struct{})
	a.SetStreamHandler(protocol.Tunnel, func(st network.Stream) {
		defer st.Close()
		<-inbound
	})
	routes := &stubNetmap{routes: map[string]peer.ID{"10.7.0.2": offlineID}}
	relay := &blockingRelay{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(nil, tunnel.New(a, routes, relay))
	defer r.Close()
	failed := make(chan error, 1)
	go func() { _, err := r.dialStream(ctx, "10.7.0.2"); failed <- err }()
	select {
	case <-relay.entered:
	case <-ctx.Done():
		t.Fatal("未进入失败拨号阶段")
	}
	if err := b.Connect(ctx, peer.AddrInfo{ID: a.ID(), Addrs: a.Addrs()}); err != nil {
		t.Fatal(err)
	}
	st, err := b.NewStream(ctx, a.ID(), protocol.Tunnel)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r.mu.Lock()
	// 使用真实入向流作为竞争注册结果，避免阻塞的入向读取干扰断言。
	healthy := &streamState{stream: st}
	r.streams["10.7.0.2"] = healthy
	r.mu.Unlock()
	close(relay.release)
	if err := <-failed; err == nil {
		t.Fatal("出向拨号本应失败")
	}
	r.mu.Lock()
	kept := r.streams["10.7.0.2"] == healthy
	r.mu.Unlock()
	if !kept {
		t.Fatal("失败出向拨号误删并发健康入向流")
	}
	close(inbound)
}

func TestRouterCloseWhileRunBlocked(t *testing.T) {
	device, _, err := NewMemory(1400)
	if err != nil {
		t.Fatal(err)
	}
	r := New(device, nil)
	runDone := make(chan struct{})
	go func() { r.Run(context.Background()); close(runDone) }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		started := r.runDone != nil
		r.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run 未启动")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	for _, ch := range []<-chan struct{}{closed, runDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("外部Close与Run退出互等")
		}
	}
}

func TestRouterConcurrentClose(t *testing.T) {
	r := New(nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Close() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("并发关闭死锁")
	}
}

func TestThousandOfflineTargetsCloseReleasesBudget(t *testing.T) {
	r := New(nil, nil)
	var active, peak atomic.Int32
	r.forwardImpl = func(ctx context.Context, p []byte) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	for i := 0; i < 1000; i++ {
		p := buildIPv4([4]byte{10, 7, 0, 1}, [4]byte{10, 8, byte(i >> 8), byte(i)}, make([]byte, 1480))
		_ = r.enqueuePacket(context.Background(), p)
	}
	r.mu.Lock()
	workers := len(r.outbound)
	queued := 0
	for _, w := range r.outbound {
		queued += len(w.packets)
	}
	r.mu.Unlock()
	if workers > 256 || peak.Load() > 256 || atomic.LoadInt64(&r.writeBudget) > outboundQueueBudget {
		t.Fatal("目标输入突破资源预算")
	}
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 未取消 worker")
	}
	if active.Load() != 0 || atomic.LoadInt64(&r.writeBudget) != 0 {
		t.Fatal("Close 后资源未归零")
	}
	t.Logf("输入1000目标，worker峰值不超过%d，转发峰值%d，采样排队%d，Close后预算0", workers, peak.Load(), queued)
}

// TestOutboundBudgetCapsQueuedBytes 验证全局出向队列字节预算（默认 16MiB）的
// 三项核心语义：
//  1. 先判满再拷贝：预算耗尽时 enqueue 直接丢弃新包，不会再为它分配/拷贝内存；
//  2. 计在途：所有目标 worker 在途（已入队未转发）字节之和始终受预算约束，
//     writeBudget 永不超过 writeBudgetMax；
//  3. 释放：worker 取走并「转发」后预算归零。
//
// 用可控的 forwardImpl 把包卡在「在途」状态，使在途字节能堆积到逼近预算上限，
// 从而确定性地触发预算满丢弃，无需真实网络。
func TestOutboundBudgetCapsQueuedBytes(t *testing.T) {
	const budget = 16 * 1024 * 1024
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: budget,
	}
	// 卡住转发：包进入 forwardImpl 后阻塞，预算一直计入在途，直到测试放行。
	block := make(chan struct{})
	router.forwardImpl = func(ctx context.Context, p []byte) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 大包 + 较大队列，使（在途 + 队列）能触及 16MiB 预算，从而触发预算满丢弃。
	const pktSize = 60000
	worker := &outboundWorker{packets: make(chan []byte, 400)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, pktSize-20))
	dropped := false
	for i := 0; i < 10000; i++ {
		if err := router.enqueuePacket(ctx, pkt); err != nil {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Fatal("expected enqueue to drop once global byte budget is exhausted")
	}
	// 预算不变量：在途字节之和永远不超过上限。
	if got := atomic.LoadInt64(&router.writeBudget); got > int64(budget) {
		t.Fatalf("writeBudget = %d exceeds budget %d", got, budget)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got == 0 {
		t.Fatal("expected writeBudget > 0 while packets are in-flight")
	}

	// 放行转发：消费掉所有在途包后预算应归零（计在途 → 释放）。
	close(block)
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("writeBudget after drain = %d, want 0 (in-flight bytes not released)", got)
	}
}

// TestOutboundBudgetReleasedOnStop 验证「停止后预算释放」：worker 因 ctx 取消
// 退出时，其队列里尚未转发的残留包所占用的在途字节必须归还预算，不能泄漏。
func TestOutboundBudgetReleasedOnStop(t *testing.T) {
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: 16 * 1024 * 1024,
	}
	// 永远不放行：模拟对端卡死，包一直占着在途预算。
	block := make(chan struct{})
	router.forwardImpl = func(ctx context.Context, p []byte) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())

	worker := &outboundWorker{packets: make(chan []byte, 16)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, 1000))
	for i := 0; i < 10; i++ {
		_ = router.enqueuePacket(ctx, pkt)
	}
	// 等待 worker 取走若干包（进入在途），预算应大于 0。
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt64(&router.writeBudget) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got == 0 {
		t.Fatal("expected writeBudget > 0 before worker stopped")
	}

	// 停止 worker：ctx 取消触发 runOutbound 退出，removeOutboundWorker 应释放
	// 残留的在途字节预算。
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("budget not released after worker stopped: %d (停止后预算释放失败)", got)
	}
}
