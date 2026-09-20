package gateway

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ayflying/pvn/pkg/gatewayproto"
)

type trackedStream struct{ closed atomic.Int32 }

func (s *trackedStream) Read(p []byte) (int, error)  { return 0, io.EOF }
func (s *trackedStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *trackedStream) Close() error                { s.closed.Add(1); return nil }

func TestQueueFullClosesSession(t *testing.T) {
	s := &trackedStream{}
	c := &wsConn{out: make(chan []byte, 1), streams: map[uint32]*meshStream{1: {rw: s}}}
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

func TestConcurrentSendAndClose(t *testing.T) {
	c := &wsConn{out: make(chan []byte, 128), streams: make(map[uint32]*meshStream)}
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
