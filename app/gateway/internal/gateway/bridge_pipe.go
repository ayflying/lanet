// bridge_pipe 网关内部的内存双工管道：把两个客户端连接（如两个 Godot
// 游戏客户端）桥接成一条流，语义与帧协议保持一致：
//
//   - Write → 数据进入对端读队列（帧级传递，Read 侧按缓冲拆分）；
//   - CloseWrite（半关闭写端）→ 对端读到 EOF，本端仍可继续读；
//   - Close / Reset（全关）→ 双向终止，对端读写均报错。
//
// 两端各自独立加锁，读侧用 pending 保留单帧未被 Read 缓冲消费完的剩余
// 字节，因此对 WS 帧大小（读限 1MB）与 pump 缓冲（16KB）无耦合。
package gateway

import (
	"io"
	"sync"
	"sync/atomic"
)

// bridgePipe 桥接管道的一端。两端经 newBridgePipePair 配对。
type bridgePipe struct {
	mu      sync.Mutex
	ch      chan []byte   // 对端 Write 推入的数据帧
	pending []byte        // Read 未消费完的剩余字节
	peer    *bridgePipe   // 配对端
	wDone   chan struct{} // 本端 CloseWrite 时关闭，对端 Read 据此返回 EOF
	done    chan struct{} // 本端 Close 时关闭，本端读写据此报错
	wClosed bool          // 本端写端是否已半关
	closed  bool          // 本端是否已全关
	halfs   atomic.Int32  // 半关计数（两端各计一次，=2 即双方都已半关）
	onceD   sync.Once     // 保护 done 只关一次
}

// newBridgePipePair 创建一对互相连通的管道端。
func newBridgePipePair() (*bridgePipe, *bridgePipe) {
	a := &bridgePipe{ch: make(chan []byte, 256), wDone: make(chan struct{}), done: make(chan struct{})}
	b := &bridgePipe{ch: make(chan []byte, 256), wDone: make(chan struct{}), done: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

// Read 从管道读取对端写来的数据。
//   - 对端 CloseWrite（半关）且数据耗尽后返回 io.EOF；
//   - 任一端 Close/Reset（全关）且缓冲排空后返回 io.ErrClosedPipe
//     （网关据此向客户端发 Reset 帧，与 EOF/Close 帧区分开）。
func (p *bridgePipe) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if len(p.pending) > 0 {
		n := copy(b, p.pending)
		p.pending = p.pending[n:]
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()

	select {
	case data, ok := <-p.ch:
		return p.take(b, data, ok)
	case <-p.peer.wDone:
		// 对端已半关写端。channel 里可能还有未读完的数据：先非阻塞
		// 尝试取一条，取不到才报 EOF。
		select {
		case data, ok := <-p.ch:
			return p.take(b, data, ok)
		default:
			return 0, io.EOF
		}
	case <-p.peer.done:
		select { // 全关也先排空残留数据
		case data, ok := <-p.ch:
			return p.take(b, data, ok)
		default:
			return 0, io.ErrClosedPipe
		}
	case <-p.done:
		select { // 本端被全关也先排空残留数据
		case data, ok := <-p.ch:
			return p.take(b, data, ok)
		default:
			return 0, io.ErrClosedPipe
		}
	}
}

// take 消费一条入队数据：拷入 b，剩余存 pending。ch 已关闭时报 EOF。
func (p *bridgePipe) take(b []byte, data []byte, ok bool) (int, error) {
	if !ok {
		return 0, io.EOF
	}
	n := copy(b, data)
	if n < len(data) {
		p.mu.Lock()
		p.pending = data[n:]
		p.mu.Unlock()
	}
	return n, nil
}

// Write 把数据推给对端。本端已半关写端或已全关时报错。
func (p *bridgePipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	if p.wClosed || p.closed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	p.mu.Unlock()

	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case p.peer.ch <- cp:
		return len(b), nil
	case <-p.peer.done:
		return 0, io.ErrClosedPipe
	}
}

// CloseWrite 半关闭写端：对端后续 Read 在数据耗尽后返回 EOF，本端仍可读。
func (p *bridgePipe) CloseWrite() error {
	p.mu.Lock()
	if p.wClosed {
		p.mu.Unlock()
		return nil
	}
	p.wClosed = true
	p.mu.Unlock()
	close(p.wDone)
	p.halfs.Add(1)
	return nil
}

// FullyHalfClosed 双方是否都已半关闭（桥接流终结条件之一）。
func (p *bridgePipe) FullyHalfClosed() bool { return p.halfs.Load() >= 2 }

// Reset 强制中止流（全关），对端读写均报错。
func (p *bridgePipe) Reset() error { return p.Close() }

// Close 全关本端：对端读写报 ErrClosedPipe（区分于半关的 EOF）；
// 本端读写报错。幂等。注意全关不复用 wDone——对端必须收到「中止」
// 而不是「EOF」，Reset 帧才能沿链路正确传播。
func (p *bridgePipe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.halfs.Add(1)
	p.onceD.Do(func() { close(p.done) })
	return nil
}
