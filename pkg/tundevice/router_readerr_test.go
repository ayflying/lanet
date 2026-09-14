package tundevice

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/peer"
	"golang.zx2c4.com/wireguard/tun"
)

// flakyDevice 模拟"前 transient 次读取返回单包级瞬时错误，之后恢复正常"的 TUN。
//
// 回归背景（2026-09-14 生产事故）：wireguard-go 的 tun 在解析 virtio 头 /
// GSO 分段时会对超大包返回 tun.ErrTooManySegments。router.Run 老实现在
// 任何读取错误上直接 return，导致整个虚拟网数据面永久停摆——而控制面
// （libp2p / probe / 控制台状态）照常，对外表现为「控制台显示在线、直连、
// rtt 正常，但 ping 与所有 TCP 端口全部超时」。本测试确保读循环能挺过
// 这类瞬时错误并继续工作。
type flakyDevice struct {
	mu        sync.Mutex
	transient int
	reads     int
	packet    []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func (d *flakyDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-d.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	d.mu.Lock()
	d.reads++
	n := d.reads
	d.mu.Unlock()

	if n <= d.transient {
		return 0, tun.ErrTooManySegments
	}
	// 后续返回一个正常包；目的地址不在 netmap 内，Router 会走
	// "forward failed" 分支，但这恰恰证明读循环仍在工作。
	copy(bufs[0][offset:], d.packet)
	sizes[0] = len(d.packet)
	// 模拟真实设备的阻塞读取，避免测试中忙转。
	time.Sleep(10 * time.Millisecond)
	return 1, nil
}

func (d *flakyDevice) Write(bufs [][]byte, offset int) (int, error) { return 1, nil }
func (d *flakyDevice) MTU() (int, error)                            { return 1400, nil }
func (d *flakyDevice) Name() (string, error)                        { return "flaky0", nil }

func (d *flakyDevice) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

func (d *flakyDevice) readsCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// TestRouterRunSurvivesTransientReadError 验证瞬时读错误（ErrTooManySegments）
// 不会终止 TUN 读循环——读循环必须跳过该包并继续读取。
func TestRouterRunSurvivesTransientReadError(t *testing.T) {
	const transient = 3

	dev := &flakyDevice{
		transient: transient,
		packet:    buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 99, 99}, []byte("after-error")),
		closed:    make(chan struct{}),
	}

	hostA, _ := newPair(t)
	router := New(dev, tunnel.New(hostA, &stubNetmap{routes: map[string]peer.ID{}}, stubRelay{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		router.Run(ctx)
		close(done)
	}()

	// 等到读次数明显超过瞬时错误次数：说明读循环挺过了错误、继续读了。
	deadline := time.Now().Add(5 * time.Second)
	for dev.readsCount() <= transient {
		if time.Now().After(deadline) {
			t.Fatalf("读循环在 %d 次瞬时错误后停止工作（reads=%d），期望继续读取",
				transient, dev.readsCount())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Run 此刻必须仍在运行（老实现会在这里已经 return）。
	select {
	case <-done:
		t.Fatal("Run 在瞬时读错误后退出：数据面会永久停摆")
	default:
	}

	// ctx 取消后应能正常退出。
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Run 未退出")
	}
}

// TestRouterRunExitsOnDeviceClosed 验证设备关闭时读循环正常退出（不被
// 容错逻辑误判为"可重试"，导致空转）。
func TestRouterRunExitsOnDeviceClosed(t *testing.T) {
	dev := &flakyDevice{closed: make(chan struct{})}

	hostA, _ := newPair(t)
	router := New(dev, tunnel.New(hostA, &stubNetmap{routes: map[string]peer.ID{}}, stubRelay{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		router.Run(ctx)
		close(done)
	}()

	// 关闭设备后，读循环应尽快退出（设备关闭错误不计入容错重试）。
	if err := dev.Close(); err != nil {
		t.Fatalf("close device: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("设备关闭后 Run 未退出")
	}
}
