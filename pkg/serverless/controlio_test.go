package serverless

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

type controlTestConn struct{ network.Conn }

func (controlTestConn) RemotePeer() peer.ID { return peer.ID("test-peer") }

type controlTestStream struct {
	network.Stream
	reader io.Reader
	read   int
	resets atomic.Int32
}

func (s *controlTestStream) Read(p []byte) (int, error) {
	n, e := s.reader.Read(p)
	s.read += n
	return n, e
}
func (s *controlTestStream) Conn() network.Conn          { return controlTestConn{} }
func (s *controlTestStream) Close() error                { return nil }
func (s *controlTestStream) Reset() error                { s.resets.Add(1); return nil }
func (s *controlTestStream) SetDeadline(time.Time) error { return nil }

type controlCaptureHost struct {
	host.Host
	handlers map[protocol.ID]network.StreamHandler
}

func (h *controlCaptureHost) SetStreamHandler(p protocol.ID, f network.StreamHandler) {
	h.handlers[p] = f
}

func TestAllProtocolEntriesRegisterRateLimit(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "派生与兼容", true: "历史协议"}[legacy], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &controlCaptureHost{Host: testHost(t, false), handlers: map[protocol.ID]network.StreamHandler{}}
			d, err := New(ctx, h, Config{NetworkKey: "rate-test", LegacyProtocols: legacy, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.Start(ctx); err != nil {
				t.Fatal(err)
			}
			d.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, GlobalEnabled: true})
			d.controlGate.active = controlMaxConcurrent
			d.seedGate.active = seedsMaxConcurrent
			expected := map[protocol.ID]bool{d.protoInfo: true, d.protoUnfriend: true, d.protoSeedsGroup: true, ProtocolSeedsGlobal: true}
			if d.protoInfoAlt != "" {
				expected[d.protoInfoAlt] = true
			}
			if d.protoUnfriendAlt != "" {
				expected[d.protoUnfriendAlt] = true
			}
			delete(expected, "")
			for p := range expected {
				t.Run(string(p), func(t *testing.T) {
					f, ok := h.handlers[p]
					if !ok {
						t.Fatalf("协议未注册: %s", p)
					}
					s := &controlTestStream{reader: strings.NewReader(`{}`)}
					f(s)
					if s.resets.Load() == 0 || s.read != 0 {
						t.Fatalf("超限入口未在读取前 Reset: resets=%d read=%d", s.resets.Load(), s.read)
					}
				})
			}
		})
	}
}

func TestHandlerDecodingBounded(t *testing.T) {
	for _, name := range []string{"info", "unfriend"} {
		t.Run(name, func(t *testing.T) {
			d := &Discovery{}
			f := d.handleInfo
			if name == "unfriend" {
				f = d.handleUnfriend
			}
			for _, body := range []string{`{"name":"` + strings.Repeat("x", maxInfoJSONSize) + `"}`, `{}` + strings.Repeat(" ", maxInfoJSONSize), `{} {}`} {
				s := &controlTestStream{reader: strings.NewReader(body)}
				f(s)
				if s.resets.Load() == 0 {
					t.Fatal("超限或尾随 JSON 未被 Reset")
				}
				if s.read > maxInfoJSONSize+1 {
					t.Fatalf("读取超界: %d", s.read)
				}
			}
		})
	}
}

func TestSeedHandlerRejectsOversizeSuffix(t *testing.T) {
	for _, scope := range []string{SeedScopeGroup, SeedScopeGlobal} {
		t.Run(scope, func(t *testing.T) {
			d := &Discovery{}
			d.initSeedRuntime(Config{GroupSeedsEnabled: true, GlobalSeedsEnabled: true})
			s := &controlTestStream{reader: strings.NewReader(`{}` + strings.Repeat(" ", seedsMaxPayload))}
			d.handleSeeds(s, scope)
			if s.resets.Load() == 0 || s.read != seedsMaxPayload+1 {
				t.Fatalf("种子超限未在边界拒绝: reset=%d read=%d", s.resets.Load(), s.read)
			}
			if d.seedGate.active != 0 {
				t.Fatal("拒绝后并发额度未释放")
			}
		})
	}
}

func TestControlJSONBoundary(t *testing.T) {
	for _, n := range []int{maxInfoJSONSize - 1, maxInfoJSONSize, maxInfoJSONSize + 1} {
		var v map[string]any
		err := decodeControlJSON(strings.NewReader(`{}`+strings.Repeat(" ", n-2)), &v)
		if (err == nil) != (n <= maxInfoJSONSize) {
			t.Fatalf("边界 %d: %v", n, err)
		}
	}
}

type controlPipeStream struct {
	network.Stream
	pipe   net.Conn
	resets atomic.Int32
}

func (s *controlPipeStream) Read(p []byte) (int, error)    { return s.pipe.Read(p) }
func (s *controlPipeStream) Write(p []byte) (int, error)   { return s.pipe.Write(p) }
func (s *controlPipeStream) Close() error                  { return s.pipe.Close() }
func (s *controlPipeStream) Reset() error                  { s.resets.Add(1); return s.pipe.Close() }
func (s *controlPipeStream) SetDeadline(t time.Time) error { return s.pipe.SetDeadline(t) }

func TestControlCancelInterruptsIO(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			s := &controlPipeStream{pipe: a}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cleanup := bindControlIO(ctx, s)
			defer cleanup()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				var err error
				if op == "read" {
					_, err = s.Read(make([]byte, 1))
				} else {
					_, err = s.Write([]byte("x"))
				}
				done <- err
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("取消前 I/O 未阻塞: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("取消未打断 I/O")
				}
			case <-time.After(time.Second):
				t.Fatal("取消后 I/O 仍阻塞")
			}
			if s.resets.Load() == 0 {
				t.Fatal("取消未 Reset")
			}
		})
	}
}

type controlDialHost struct {
	host.Host
	stream network.Stream
}

func (h controlDialHost) NewStream(context.Context, peer.ID, ...protocol.ID) (network.Stream, error) {
	return h.stream, nil
}

func (s *controlPipeStream) CloseWrite() error { return nil }

func TestFetchInfoCancelInterruptsRead(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := &controlPipeStream{pipe: a}
	h := testHost(t, false)
	d := &Discovery{host: controlDialHost{Host: h, stream: s}, groupKey: make([]byte, 32)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan struct{})
	go func() { buf := make([]byte, 4096); _, _ = b.Read(buf); close(received) }()
	done := make(chan error, 1)
	go func() { _, err := d.fetchInfo(ctx, h.ID()); done <- err }()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("未收到请求")
	}
	select {
	case err := <-done:
		t.Fatalf("取消前读取未阻塞: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消读取未返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("fetchInfo 取消未生效")
	}
	if s.resets.Load() == 0 {
		t.Fatal("未 Reset")
	}
}

func TestNotifyUnfriendCancelInterruptsWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := &controlPipeStream{pipe: a}
	h := testHost(t, false)
	d := &Discovery{host: controlDialHost{Host: h, stream: s}, groupKey: make([]byte, 32)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.NotifyUnfriend(ctx, h.ID().String()) }()
	select {
	case err := <-done:
		t.Fatalf("取消前写入未阻塞: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消写入未返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("NotifyUnfriend 取消未生效")
	}
	if s.resets.Load() == 0 {
		t.Fatal("未 Reset")
	}
}

func TestControlRateBudgetSharedAndBounded(t *testing.T) {
	var r controlRateLimiter
	now := time.Now()
	for i := 0; i < controlPeerBurst; i++ {
		if !r.allow("info:peer", now) {
			t.Fatalf("提前限流 %d", i)
		}
		r.release()
	}
	if r.allow("info:peer", now) {
		t.Fatal("同桶超额仍放行")
	}
	if !r.allow("unfriend:peer", now) {
		t.Fatal("独立操作被误限流")
	}
	r.release()
	if !r.allow("info:peer", now.Add(time.Minute)) {
		t.Fatal("窗口过期未恢复")
	}
	r.release()
	for i := 0; i < controlMaxPeers; i++ {
		key := string(rune(i)) + "peer"
		if r.allow(key, now) {
			r.release()
		}
	}
	if len(r.peers) > controlMaxPeers {
		t.Fatal("限流状态表无界")
	}
}
