package tundevice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/network"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// ----------------------------------------------------------------------------
// 验收矩阵要求覆盖的「恢复」行为（fake 不需要真实 TUN）：
//   1. 预算饱和：已排队 + 在途包的预算真实释放（见 router_budget_test.go）
//   2. 失败冷却不得让正常长期流停滞
//   3. 取消必须解除 stream.Write 阻塞并回收预算
//   4. 慢写恢复：受背压的写最终要恢复吞吐，预算不泄漏、不超预算
//
// 下面三组测试补齐 2/3/4。生产路径 (forwardPacket 的 ctx.AfterFunc→Reset 解除
// 阻塞写、runOutbound 在转发返回后 releaseBudget) 通过可注入的 forwardImpl 与
// 可控 mock 流确定性地覆盖，无需真实网络。
// ----------------------------------------------------------------------------

// TestFailureCooldownRecoversHealthyStream 验证「失败冷却」只作用在失效对端上，
// 不会让一条本来健康的长期流永久停滞：前几次转发失败后进入指数退避，但一旦对端
// 恢复（转发成功），failures 归零、冷却消失，后续包必须持续被转发。
func TestFailureCooldownRecoversHealthyStream(t *testing.T) {
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: 16 * 1024 * 1024,
	}
	var (
		mu        sync.Mutex
		attempts  int
		processed int
		forwards  int
	)
	router.forwardImpl = func(ctx context.Context, p []byte) error {
		mu.Lock()
		attempts++
		n := attempts
		processed++
		mu.Unlock()
		// 前 5 次模拟对端瞬时失效（触发冷却退避，包按设计被丢弃）。
		if n <= 5 {
			return fmt.Errorf("transient failure %d", n)
		}
		mu.Lock()
		forwards++
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := &outboundWorker{packets: make(chan []byte, 256)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	const total = 200
	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, 100))
	for i := 0; i < total; i++ {
		_ = router.enqueuePacket(ctx, pkt)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		done := processed
		mu.Unlock()
		if done >= total {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			d, a := processed, attempts
			mu.Unlock()
			t.Fatalf("健康流被冷却拖死：processed=%d/%d (attempts=%d)", d, total, a)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 前 5 次失败属预期丢弃；其余必须全部成功转发（冷却后无永久停滞）。
	if forwards != total-5 {
		t.Fatalf("冷却恢复后转发数=%d，期望 %d（正常长期流被冷却拖慢）", forwards, total-5)
	}

	// 全部转发出去后，在途预算必须归零。
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("writeBudget=%d after drain, want 0", got)
	}
}

// TestCancelReleaseBudgetViaForwardImpl 验证「取消回收预算」的 worker 级契约：
// 当转发被 ctx 取消打断（模拟 stream.Write 被 AfterFunc 的 Reset 解除阻塞后返回），
// runOutbound 在 forwardImpl 返回后必须归还该在途包的字节预算，不能泄漏。
func TestCancelReleaseBudgetViaForwardImpl(t *testing.T) {
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: 16 * 1024 * 1024,
	}
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

	worker := &outboundWorker{packets: make(chan []byte, 4)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, 1000))
	if err := router.enqueuePacket(ctx, pkt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 等待包进入在途（被 forwardImpl 卡住），预算必须大于 0。
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt64(&router.writeBudget) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got == 0 {
		t.Fatal("expected writeBudget > 0 while packet is in-flight")
	}

	// 取消：forwardImpl 应立即从 ctx.Done() 返回，runOutbound 归还预算。
	cancel()
	deadline = time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("writeBudget after cancel = %d, want 0 (取消未回收预算)", got)
	}
}

// mockBlockingStream 实现 network.Stream：Write 阻塞直到 Reset（ctx 取消时的
// AfterFunc 会调用）或写超时；其余方法为无操作桩。用于确定性地验证「取消解除
// stream.Write 阻塞并回收预算」这条真实生产路径（forwardPacket 的 AfterFunc→Reset）。
type mockBlockingStream struct {
	resetOnce     sync.Once
	resetCh       chan struct{}
	blockedOnce   sync.Once
	blockedCh     chan struct{}
	mu            sync.Mutex
	writeDeadline time.Time
}

func newMockBlockingStream() *mockBlockingStream {
	return &mockBlockingStream{
		resetCh:   make(chan struct{}),
		blockedCh: make(chan struct{}),
	}
}

// Blocked 在 Write 真正进入阻塞时关闭，供测试做 happens-before 同步。
func (m *mockBlockingStream) Blocked() <-chan struct{} { return m.blockedCh }

// ResetCalled 在 Reset（即生产里的 AfterFunc）被调用时关闭。
func (m *mockBlockingStream) ResetCalled() <-chan struct{} { return m.resetCh }

func (m *mockBlockingStream) Write(p []byte) (int, error) {
	m.blockedOnce.Do(func() { close(m.blockedCh) })
	select {
	case <-m.resetCh:
		return 0, errors.New("stream reset")
	case <-time.After(time.Until(m.writeDeadline)):
		return 0, os.ErrDeadlineExceeded
	}
}

func (m *mockBlockingStream) Reset() error {
	m.resetOnce.Do(func() { close(m.resetCh) })
	return nil
}

func (m *mockBlockingStream) SetWriteDeadline(t time.Time) error {
	m.mu.Lock()
	m.writeDeadline = t
	m.mu.Unlock()
	return nil
}

// —— network.Stream 桩方法（仅满足接口；生产错误路径不触碰 Conn/Scope 等）——

func (m *mockBlockingStream) Read(p []byte) (int, error)      { return 0, io.EOF }
func (m *mockBlockingStream) Close() error                    { return nil }
func (m *mockBlockingStream) CloseWrite() error               { return nil }
func (m *mockBlockingStream) CloseRead() error                { return nil }
func (m *mockBlockingStream) SetDeadline(time.Time) error     { return nil }
func (m *mockBlockingStream) SetReadDeadline(time.Time) error { return nil }
func (m *mockBlockingStream) ResetWithError(network.StreamErrorCode) error {
	return m.Reset()
}
func (m *mockBlockingStream) ID() string                       { return "mock" }
func (m *mockBlockingStream) Protocol() libprotocol.ID         { return protocol.Tunnel }
func (m *mockBlockingStream) SetProtocol(libprotocol.ID) error { return nil }
func (m *mockBlockingStream) Stat() network.Stats              { return network.Stats{} }
func (m *mockBlockingStream) Conn() network.Conn               { return nil }
func (m *mockBlockingStream) Scope() network.StreamScope       { return nil }

// TestForwardPacketCancelUnblocksWriteAndReclaimsBudget 走真实生产路径：
// enqueue → runOutbound → forwardImpl(=forwardPacket) → 对 mock 流 Write 阻塞
// → ctx 取消触发 context.AfterFunc 调用 stream.Reset() 解除阻塞 → forwardPacket
// 返回错误 → runOutbound 在途预算归还。验证「取消解除 stream.Write 阻塞」与
// 「回收预算」二者同时成立。
func TestForwardPacketCancelUnblocksWriteAndReclaimsBudget(t *testing.T) {
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: 16 * 1024 * 1024,
		streams:        make(map[string]*streamState),
	}
	// 走真实 forwardPacket（含 AfterFunc→Reset 解除阻塞写）。
	router.forwardImpl = func(ctx context.Context, p []byte) error {
		return router.forwardPacket(ctx, p)
	}

	ms := newMockBlockingStream()
	router.streams["10.7.0.3"] = &streamState{stream: ms}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := &outboundWorker{packets: make(chan []byte, 4)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, 1000))
	if err := router.enqueuePacket(ctx, pkt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 等待 forwardPacket 卡在 stream.Write（在途预算被占用）。
	select {
	case <-ms.Blocked():
	case <-time.After(3 * time.Second):
		t.Fatal("forwardPacket 未阻塞在 stream.Write（测试前提不成立）")
	}
	if got := atomic.LoadInt64(&router.writeBudget); got == 0 {
		t.Fatal("expected writeBudget > 0 while Write is blocked (in-flight)")
	}

	// 取消：AfterFunc 必须 Reset 流以打断阻塞写。
	cancel()
	select {
	case <-ms.ResetCalled():
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 stream.Reset() 未被调用（AfterFunc 解除阻塞写失效）")
	}

	// Write 解除阻塞 → forwardPacket 返回 → 预算回收。
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("writeBudget after cancel = %d, want 0 (取消未回收预算)", got)
	}
}

// TestSlowWriteRecovers 验证「慢写恢复」：forwardImpl 模拟受背压的慢写（每次
// 阻塞一小段时间但终会成功）。worker 是串行的，慢写不能让队列/预算死锁——
// 每个包转发完成后预算必须归还，且全程在途字节不得超过预算上限。
func TestSlowWriteRecovers(t *testing.T) {
	router := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  8,
		outboundIdle:   time.Minute,
		writeBudgetMax: 16 * 1024 * 1024,
	}
	var (
		mu        sync.Mutex
		forwards  int
		maxBudget int64
	)
	router.forwardImpl = func(ctx context.Context, p []byte) error {
		// 模拟背压下的慢写：阻塞 20ms 后成功。
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		mu.Lock()
		forwards++
		if b := atomic.LoadInt64(&router.writeBudget); b > maxBudget {
			maxBudget = b
		}
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := &outboundWorker{packets: make(chan []byte, 256)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	const total = 100
	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, make([]byte, 500))
	for i := 0; i < total; i++ {
		_ = router.enqueuePacket(ctx, pkt)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		done := forwards
		mu.Unlock()
		if done >= total {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			d := forwards
			mu.Unlock()
			t.Fatalf("慢写导致 worker 停滞：forwarded=%d/%d", d, total)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 预算不变量：慢写期间在途字节峰值不得超过上限。
	if maxBudget > router.writeBudgetMax {
		t.Fatalf("writeBudget peaked at %d > max %d (预算不变量被破坏)", maxBudget, router.writeBudgetMax)
	}
	// 全部转发后预算归零。
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&router.writeBudget) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&router.writeBudget); got != 0 {
		t.Fatalf("writeBudget=%d after drain, want 0", got)
	}
}
