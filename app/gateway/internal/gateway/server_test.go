package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/gatewayproto"
	"github.com/gorilla/websocket"
)

type trackedStream struct{ closed atomic.Int32 }

func (s *trackedStream) Read(p []byte) (int, error)  { return 0, io.EOF }
func (s *trackedStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *trackedStream) Close() error                { s.closed.Add(1); return nil }

// 满队列：出站队列满时 send 必须终止会话（关闭底层流），而非静默丢帧。
func TestQueueFullClosesSession(t *testing.T) {
	s := &trackedStream{}
	c := &wsConn{out: make(chan []byte, 1), streams: map[uint32]*meshStream{1: newMeshStream(s)}}
	if !c.send(gatewayproto.Frame{Type: gatewayproto.TypeData}) {
		t.Fatal("首帧应成功")
	}
	if c.send(gatewayproto.Frame{Type: gatewayproto.TypeData}) {
		t.Fatal("队满应终止会话")
	}
	if !c.closed || len(c.streams) != 0 || s.closed.Load() != 1 {
		t.Fatal("会话资源未完整回收")
	}
	c.closeAllStreams()
	if s.closed.Load() != 1 {
		t.Fatal("重复关闭不幂等")
	}
}

// 并发 send 与 close：不允许向已关闭/正在关闭的通道写，且不得死锁。
func TestConcurrentSendAndClose(t *testing.T) {
	c := &wsConn{out: make(chan []byte, outBufSize), streams: make(map[uint32]*meshStream)}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.send(gatewayproto.Frame{Type: gatewayproto.TypePing})
			}
		}()
	}
	c.closeAllStreams()
	wg.Wait()
	if c.send(gatewayproto.Frame{}) {
		t.Fatal("关闭后不能发送")
	}
	for range c.out {
	}
}

// 超限：单连接应用层流数硬上限。达到上限后再次 track 必须拒绝并回收
// 底层流；重复 streamID 同样拒绝并关闭。
func TestStreamCountLimit(t *testing.T) {
	c := &wsConn{streams: make(map[uint32]*meshStream)}
	for i := uint32(1); i <= maxStreams; i++ {
		st := newMeshStream(&trackedStream{})
		if !c.track(i, st) {
			t.Fatalf("前 %d 条流应全部登记成功，第 %d 条失败", maxStreams, i)
		}
	}
	if len(c.streams) != maxStreams {
		t.Fatalf("期望 %d 条流，实际 %d", maxStreams, len(c.streams))
	}

	over := &trackedStream{}
	if c.track(maxStreams+1, newMeshStream(over)) {
		t.Fatal("超过流数上限仍被登记，应用层上限失效")
	}
	if over.closed.Load() != 1 {
		t.Fatal("超限的流未被关闭（应明确拒绝并回收底层资源）")
	}

	dup := &trackedStream{}
	if c.track(1, newMeshStream(dup)) {
		t.Fatal("重复 streamID 不应被登记")
	}
	if dup.closed.Load() != 1 {
		t.Fatal("重复 id 的流未被关闭")
	}
}

// 关闭清理：closeAllStreams 关闭所有底层流、清空流表、标记 closed、幂等，
// 且关闭后 send 必须失败（不向已关通道写入）。
func TestCloseAllStreamsCleansUp(t *testing.T) {
	var streams []*trackedStream
	c := &wsConn{out: make(chan []byte, outBufSize), streams: make(map[uint32]*meshStream)}
	for i := uint32(1); i <= 8; i++ {
		ts := &trackedStream{}
		streams = append(streams, ts)
		c.streams[i] = newMeshStream(ts)
	}

	c.closeAllStreams()
	if !c.closed {
		t.Fatal("closeAllStreams 后会话应标记关闭")
	}
	if len(c.streams) != 0 {
		t.Fatal("closeAllStreams 后应清空流表")
	}
	for i, ts := range streams {
		if ts.closed.Load() != 1 {
			t.Fatalf("第 %d 条底层流未关闭", i)
		}
	}

	// 幂等：重复调用不 panic、不重复关闭。
	c.closeAllStreams()
	for i, ts := range streams {
		if ts.closed.Load() != 1 {
			t.Fatalf("幂等关闭后第 %d 条流关闭次数异常: %d", i, ts.closed.Load())
		}
	}

	// 关闭后 send 必须失败（不丢帧、不写入已关通道）。
	if c.send(gatewayproto.Frame{Type: gatewayproto.TypePing}) {
		t.Fatal("关闭后 send 不应成功")
	}
}

// 并发 track/untrack/close：在 -race 下不得出现数据竞争或死锁。
func TestConcurrentTrackUntrackClose(t *testing.T) {
	c := &wsConn{out: make(chan []byte, outBufSize), streams: make(map[uint32]*meshStream)}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := uint32(base*50 + j + 1)
				st := newMeshStream(&trackedStream{})
				if c.track(id, st) {
					if j%3 == 0 {
						if s := c.untrack(id); s != nil {
							_ = s.close()
						}
					}
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		c.closeAllStreams()
	}()
	wg.Wait()
	c.closeAllStreams()
}

// 验收项 1：HTTP 读头/空闲超时必须由 http.Server 显式配置，避免慢速或
// 空闲连接长期占用文件描述符（空闲超时仅作用于未升级的 HTTP 连接，
// 升级后的 WS 长连接不受影响）。
func TestHTTPTimeoutsOnSocket(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "读头", true: "空闲"}[idle], func(t *testing.T) {
			s := &Server{}
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
			ts.Config = s.httpServer(ts.Config.Handler)
			ts.Config.ReadHeaderTimeout = 60 * time.Millisecond
			ts.Config.IdleTimeout = 60 * time.Millisecond
			ts.Start()
			defer ts.Close()
			c, err := net.Dial("tcp", ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if idle {
				_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
				r, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
			} else {
				_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost:")
			}
			_, err = io.ReadAll(c)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("服务端未按时回收连接")
			}
		})
	}
}

func TestWebSocketContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{}
	done := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		s.handleWS(w, r)
	}))
	ts.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	ts.Start()
	defer ts.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后未鉴权 WebSocket 未释放")
	}
}

func TestWriteFailureClosesStreams(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			accepted <- ws
		}
	}))
	defer ts.Close()
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ws := <-accepted
	_ = ws.UnderlyingConn().Close()
	st := &trackedStream{}
	c := &wsConn{ws: ws, out: make(chan []byte, 1), writerDone: make(chan struct{}), streams: map[uint32]*meshStream{1: newMeshStream(st)}}
	c.out <- []byte("触发写失败")
	done := make(chan struct{})
	go func() { c.writeLoop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("写失败清理死锁")
	}
	if st.closed.Load() != 1 || !c.closed {
		t.Fatal("写失败未清理会话与流")
	}
}

func TestBridgeQueueFailureRollsBack(t *testing.T) {
	for _, targetFull := range []bool{false, true} {
		t.Run(map[bool]string{false: "发起方队满", true: "目标队满"}[targetFull], func(t *testing.T) {
			source := &wsConn{out: make(chan []byte, 1), streams: make(map[uint32]*meshStream)}
			target := &wsConn{out: make(chan []byte, 4), streams: make(map[uint32]*meshStream)}
			defer source.closeAllStreams()
			defer target.closeAllStreams()
			full := source
			if targetFull {
				full = target
			}
			for len(full.out) < cap(full.out) {
				full.out <- []byte("占满")
			}
			s := &Server{clients: map[string]*wsConn{"target": target}}
			s.bridgeDial(source, 1000, dialReq{Peer: "target"})
			if len(source.streams) != 0 || len(target.streams) != 0 {
				t.Fatal("通知失败后双端仍残留流")
			}
		})
	}
}

func TestBridgeTargetLimitRollsBack(t *testing.T) {
	source := &wsConn{out: make(chan []byte, 8), streams: make(map[uint32]*meshStream)}
	target := &wsConn{out: make(chan []byte, 8), streams: make(map[uint32]*meshStream)}
	for i := uint32(1); i <= maxStreams; i++ {
		target.streams[i] = newMeshStream(&trackedStream{})
	}
	defer source.closeAllStreams()
	defer target.closeAllStreams()
	s := &Server{clients: map[string]*wsConn{"target": target}}
	s.bridgeDial(source, 1000, dialReq{Peer: "target"})
	frame, err := gatewayproto.Unmarshal(<-source.out)
	if err != nil || frame.Type != gatewayproto.TypeDialErr {
		t.Fatalf("目标超限必须返回 DialErr，不能先发 DialOk: %+v %v", frame, err)
	}
	if source.get(1000) != nil {
		t.Fatal("目标超限后发起端流未摘除")
	}
}

func TestHTTPServerTimeouts(t *testing.T) {
	s := &Server{cfg: Config{ListenAddr: ":0", Path: "/gateway"}}
	mux := http.NewServeMux()
	srv := s.httpServer(mux)
	if srv.ReadHeaderTimeout != httpReadHeaderTimeout {
		t.Fatalf("读头超时未设置: %v", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != httpIdleTimeout {
		t.Fatalf("空闲超时未设置: %v", srv.IdleTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatal("HTTP 超时必须为正数")
	}
}
