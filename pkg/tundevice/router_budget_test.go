package tundevice

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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
