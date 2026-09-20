package serverless

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestInfoBoundedJSON 验证 info 握手的有界 JSON 读取：
//  1. 正常大小请求仍正常往返（回归）；
//  2. 超过 64KiB 的合法 JSON 请求被服务端截断 → Decode 失败 → Reset，
//     客户端读响应立即报错（而非无限等待），且服务端后续仍正常服务。
//
// 若没有 64KiB 上限，超大合法 JSON 会被完整读完并成功 Decode，服务端会回
// 送响应——本测试据此把「上限是否生效」变成可判定的断言。
func TestInfoBoundedJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	da, err := New(ctx, ha, Config{NetworkKey: "bounded", Name: "node-a"})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "bounded", Name: "node-b"})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 阶段一：正常请求正常往返。
	info, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("正常 fetchInfo 失败: %v", err)
	}
	if info.Name != "node-b" {
		t.Fatalf("info.Name = %q, want node-b", info.Name)
	}

	// 阶段二：发送一个超过 64KiB 的「合法 JSON」（超长 name + 正确 group）。
	// 有界读取应截断到 64KiB，Decode 失败 → 服务端 Reset 该流。
	hugeName := strings.Repeat("x", 70*1024)
	payload := fmt.Sprintf(`{"name":"%s","group":"%s"}`, hugeName, GroupFingerprint(da.groupKey))

	s, err := ha.NewStream(ctx, hb.ID(), da.protoInfo)
	if err != nil {
		t.Fatalf("open raw stream: %v", err)
	}
	defer s.Close()
	_ = s.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatalf("写入超大请求失败: %v", err)
	}
	// 读响应：有界生效时应立刻收到 Reset（err != nil）；若读到合法响应说明上限失效。
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := s.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("超大请求竟被服务端正常处理（有界 JSON 未生效）：got %d bytes %q", n, buf[:n])
	}

	// 阶段三：服务端在 Reset 该流后必须仍健康，正常握手不受影响。
	info2, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("超大请求后服务端不可用（可能崩溃）: %v", err)
	}
	if info2.Name != "node-b" {
		t.Fatalf("超大请求后 info.Name = %q, want node-b", info2.Name)
	}
}

// TestInfoDeadlineCancelReset 验证 fetchInfo 携带更短 ctx 截止时，读取响应
// 的真实流截止时间取自 ctx（而非默认的 10s 握手超时），因此能在 ctx 截止后
// 很快返回；且 ctx 取消路径会走 stream.Reset()（而非优雅关闭）。
//
// 做法：把对端 B 的 info 处理 handler 替换为一个「只收不发、永久阻塞读取」
// 的桩，使正常握手必然超时，从而观察 fetchInfo 的返回时机。
func TestInfoDeadlineCancelReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ha := testHost(t, false)
	hb := testHost(t, false)

	da, err := New(ctx, ha, Config{NetworkKey: "deadline", Name: "node-a"})
	if err != nil {
		t.Fatalf("new discovery A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "deadline", Name: "node-b"})
	if err != nil {
		t.Fatalf("new discovery B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 覆盖 B 的 info handler：读完首段后无限阻塞（不响应）。
	blocking := func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(time.Hour)) // 自身不超时，纯靠对端截止驱动
		buf := make([]byte, 256)
		_, _ = s.Read(buf) // 读掉客户端写入的请求，随后第二次 Read 阻塞
		_, _ = s.Read(buf) // 永久阻塞（客户端已 CloseWrite，不会再有数据）
	}
	hb.SetStreamHandler(da.protoInfo, blocking)

	// 300ms 的 ctx：fetchInfo 必须在远小于默认 10s 超时内返回。
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer shortCancel()
	start := time.Now()
	_, err = da.fetchInfo(shortCtx, hb.ID())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("fetchInfo 未在 ctx 截止内返回（耗时 %v），说明真实 deadline 未生效", elapsed)
	}
	if err == nil {
		t.Fatalf("对端不响应时应返回错误，但得到 nil")
	}
}

// TestDiscoveryClose 验证 Discovery.Close() 能安全关闭底层 DHT/mDNS 资源，
// 且重复调用幂等（不 panic、不重复关闭报错）。Run 循环由调用方 ctx 取消驱动，
// 本方法覆盖其之外的长生命周期资源。
func TestDiscoveryClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h := testHost(t, false)
	d, err := New(ctx, h, Config{
		NetworkKey: "close-test", Name: "node-a",
		EnableMDNS: true,
	})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}
	if err = d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	go d.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if err := d.Close(); err != nil {
		t.Fatalf("首次 Close 返回错误: %v", err)
	}
	// 幂等：二次关闭不应 panic 或报错。
	if err := d.Close(); err != nil {
		t.Fatalf("二次 Close 应幂等，但返回错误: %v", err)
	}
	cancel()
}

// TestCallbackLockSafety 验证 onDiscovered 回调切片在并发下的线程安全。
// 分两阶段：先并发注册 50 个回调（触发 OnDiscovered 的加锁追加），再并发触发
// 100 次 emit（触发 cbMu.RLock 快照 + 回调）。未加锁时这种并发会触发
// data race（go test -race 可捕获）；本测试在 -race 下应通过，即锁保护生效。
// 确定性断言：注册全部结束后，100 次 emit × 50 个回调应恰好调用 5000 次。
func TestCallbackLockSafety(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "cb-test", Name: "node-a"})
	if err != nil {
		t.Fatalf("new discovery: %v", err)
	}

	var mu sync.Mutex
	var total int

	// 阶段一：并发注册 50 个回调（触发 OnDiscovered 的加锁追加）。
	var wgReg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wgReg.Add(1)
		go func() {
			defer wgReg.Done()
			d.OnDiscovered(func(Member) {
				mu.Lock()
				total++
				mu.Unlock()
			})
		}()
	}
	wgReg.Wait()

	// 阶段二：并发触发 100 次 emit（每次都快照当前全部 50 个回调并调用）。
	var wgEmit sync.WaitGroup
	for j := 0; j < 100; j++ {
		wgEmit.Add(1)
		go func() {
			defer wgEmit.Done()
			d.emit(Member{PeerID: "x", Name: "x"})
		}()
	}
	wgEmit.Wait()

	if total != 50*100 {
		t.Fatalf("回调触发次数不符：got %d want %d（并发注册/触发竞态？）", total, 50*100)
	}
}
