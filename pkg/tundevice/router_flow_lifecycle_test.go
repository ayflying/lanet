package tundevice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestInstallFlowReplacesRetiredBeforeOldCleanup(t *testing.T) {
	for _, initiator := range []peer.ID{"a", "z"} {
		t.Run(string(initiator), func(t *testing.T) {
			r := New(nil, nil)
			r.retireGrace = 0
			old := newPeerFlow(newFakeFlowStream(), "remote", "a", []string{testPeerV4})
			r.installFlow(old)
			// 精确复现 retireFlow 已 CAS 退役、尚未取得 r.mu 摘表的窗口。
			old.retired.Store(true)
			next := newPeerFlow(newFakeFlowStream(), "remote", initiator, []string{testPeerV6})
			if kept, ok := r.installFlow(next); !ok || kept != next {
				t.Fatal("retired existing must never defeat a fresh candidate")
			}
			r.retireFlow(old, "delayed old cleanup")
			assertCurrentDualStackFlow(t, r, next)
		})
	}
}

func TestOldFlowCleanupDoesNotDeleteReplacement(t *testing.T) {
	r := New(nil, nil)
	r.retireGrace = 0
	old := newPeerFlow(newFakeFlowStream(), "remote", "z", []string{testPeerV4})
	next := newPeerFlow(newFakeFlowStream(), "remote", "a", []string{testPeerV6})
	r.installFlow(next)
	// 未退役的旧 flow 真正执行 retireFlow 的摘表分支，也不能删掉新 flow。
	r.retireFlow(old, "late pump exit")
	if r.flows[next.peer] != next || r.flowByAddr[testPeerV6] != next.peer {
		t.Fatal("old cleanup deleted replacement or its alias")
	}
	if next.stream.(*fakeFlowStream).closed.Load() {
		t.Fatal("old cleanup closed replacement")
	}
}

func assertCurrentDualStackFlow(t *testing.T, r *Router, flow *peerFlow) {
	t.Helper()
	if len(r.flows) != 1 || r.flows[flow.peer] != flow || flow.retired.Load() {
		t.Fatal("replacement is not the sole live flow")
	}
	for _, addr := range []string{testPeerV4, testPeerV6} {
		if r.flowByAddr[addr] != flow.peer {
			t.Fatalf("alias %s no longer points to replacement", addr)
		}
	}
}

// 用真实 libp2p 流复现：发起端已经重拨，接收端旧 pump 尚未处理关闭。
// 新入向必须接管旧入向，但不能对本机同发起方健康流无条件以新换旧。
func TestSameInitiatorRedialSurvivesDelayedReceiverCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := newPair(t)
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatal(err)
	}
	incoming := make(chan network.Stream, 2)
	b.SetStreamHandler(protocol.Tunnel, func(s network.Stream) { incoming <- s })
	ra, rb := New(nil, nil), New(nil, nil)
	ra.selfID, rb.selfID = a.ID(), b.ID()
	ra.retireGrace, rb.retireGrace = 0, 0
	defer ra.Close()
	defer rb.Close()
	open := func() (network.Stream, network.Stream) {
		t.Helper()
		s, err := a.NewStream(ctx, b.ID(), protocol.Tunnel)
		if err != nil {
			t.Fatal(err)
		}
		// 已协商过的协议可能延迟发送协议头，首次写触发远端 handler。
		started := make(chan error, 1)
		go func() { _, err := s.Write([]byte{0}); started <- err }()
		select {
		case remote := <-incoming:
			var marker [1]byte
			if _, err := io.ReadFull(remote, marker[:]); err != nil {
				t.Fatal(err)
			}
			if err := <-started; err != nil {
				t.Fatal(err)
			}
			return s, remote
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return nil, nil
		}
	}
	s1a, s1b := open()
	oldA := newPeerFlow(s1a, b.ID(), a.ID(), []string{testPeerV4})
	oldB := newPeerFlow(s1b, a.ID(), a.ID(), []string{testPeerV4})
	ra.installFlow(oldA)
	rb.installFlow(oldB)
	ra.retireFlow(oldA, "write failed")
	s2a, s2b := open()
	nextA := newPeerFlow(s2a, b.ID(), a.ID(), []string{testPeerV6})
	nextB := newPeerFlow(s2b, a.ID(), a.ID(), []string{testPeerV6})
	if kept, _ := ra.installFlow(nextA); kept != nextA {
		t.Fatal("initiator rejected its new generation")
	}
	if kept, _ := rb.installFlow(nextB); kept != nextB {
		t.Fatal("receiver retained stale same-initiator stream")
	}
	// 旧流的关闭传播与延迟清理不能误伤新流或 v4/v6 别名。
	rb.pumpFromStream(oldB)
	assertCurrentDualStackFlow(t, rb, nextB)
	_ = s2a.SetDeadline(time.Now().Add(3 * time.Second))
	_ = s2b.SetDeadline(time.Now().Add(3 * time.Second))
	written := make(chan error, 1)
	go func() { _, err := s2a.Write([]byte("new generation")); written <- err }()
	buf := make([]byte, len("new generation"))
	if _, err := io.ReadFull(s2b, buf); err != nil || string(buf) != "new generation" {
		t.Fatalf("replacement broken by old closure: %q, %v", buf, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	// 即使流的本地 ID 看起来更大，本机健康出向也不因新 candidate 被顶替。
	extra := newFakeFlowStream()
	extra.tag = "999999"
	candidate := newPeerFlow(extra, b.ID(), a.ID(), []string{testPeerV4})
	if kept, _ := ra.installFlow(candidate); kept != nextA {
		t.Fatal("healthy same-initiator outbound must be reused")
	}
}

type readErrorFlowStream struct {
	*fakeFlowStream
	err error
}

func (s *readErrorFlowStream) Read([]byte) (int, error) { return 0, s.err }

// 两端真实同时拨出且启动 pump，再放行入向注册；败者关闭传播后仍须双向可用，
// 并能在主动断开赢家后通过生产 forwardPacket 路径再次同时重建。
func TestSimultaneousOutboundPumpsConvergeAndRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := newPair(t)
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatal(err)
	}
	memA, _, err := NewMemory(1400)
	if err != nil {
		t.Fatal(err)
	}
	memB, _, err := NewMemory(1400)
	if err != nil {
		_ = memA.Close()
		t.Fatal(err)
	}
	da, db := &recordingDevice{Device: memA}, &recordingDevice{Device: memB}
	ra := New(da, tunnel.New(a, &dualStackNetmap{v4: testPeerV4, v6: testPeerV6, id: b.ID()}, stubRelay{}))
	rb := New(db, tunnel.New(b, &dualStackNetmap{v4: "10.7.0.2", v6: "fd00:6c61:6e65::2", id: a.ID()}, stubRelay{}))
	defer ra.Close()
	defer rb.Close()
	ra.retireGrace, rb.retireGrace = time.Millisecond, time.Millisecond
	gate := make(chan struct{})
	registered := make(chan struct{}, 4)
	a.SetStreamHandler(protocol.Tunnel, func(s network.Stream) {
		select {
		case <-gate:
			ra.ServeInboundStreamAliases(testPeerV4, testPeerV6, s)
			registered <- struct{}{}
		case <-ctx.Done():
			_ = s.Reset()
		}
	})
	b.SetStreamHandler(protocol.Tunnel, func(s network.Stream) {
		select {
		case <-gate:
			rb.ServeInboundStreamAliases("10.7.0.2", "fd00:6c61:6e65::2", s)
			registered <- struct{}{}
		case <-ctx.Done():
			_ = s.Reset()
		}
	})
	packetA := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("a-to-b"))
	packetB := buildIPv6Packet(testPeerV6, "fd00:6c61:6e65::2", []byte("b-to-a"))
	errorsOut := make(chan error, 2)
	go func() {
		s, err := a.NewStream(ctx, b.ID(), protocol.Tunnel)
		if err == nil {
			flow := newPeerFlow(s, b.ID(), a.ID(), []string{testPeerV4, testPeerV6})
			ra.installFlow(flow)
			ra.ensurePump(flow)
			_, err = s.Write(packetA)
		}
		errorsOut <- err
	}()
	go func() {
		s, err := b.NewStream(ctx, a.ID(), protocol.Tunnel)
		if err == nil {
			flow := newPeerFlow(s, a.ID(), b.ID(), []string{"10.7.0.2", "fd00:6c61:6e65::2"})
			rb.installFlow(flow)
			rb.ensurePump(flow)
			_, err = s.Write(packetB)
		}
		errorsOut <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errorsOut:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(gate)
	for i := 0; i < 2; i++ {
		select {
		case <-registered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	current := func(r *Router, remote peer.ID) *peerFlow {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.flows[remote]
	}
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("routers failed to converge/recover: ", ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	winner := a.ID()
	if b.ID() < winner {
		winner = b.ID()
	}
	wait(func() bool {
		fa, fb := current(ra, b.ID()), current(rb, a.ID())
		return fa != nil && fb != nil && fa.initiator == winner && fb.initiator == winner && !fa.retired.Load() && !fb.retired.Load()
	})
	// 必须等待入向 handler 全部处理完成，不能仅以赢家 initiator 在位判断收敛。
	// 较小发起方的出向在入向败者尚未到达前就已经是赢家。
	wait(func() bool { return current(ra, b.ID()).pumped.Load() && current(rb, a.ID()).pumped.Load() })
	verifyTraffic := func() {
		t.Helper()
		beforeA, beforeB := len(da.snapshot()), len(db.snapshot())
		if err := ra.forwardPacket(ctx, packetA); err != nil {
			t.Fatal(err)
		}
		if err := rb.forwardPacket(ctx, packetB); err != nil {
			t.Fatal(err)
		}
		wait(func() bool { return len(da.snapshot()) > beforeA && len(db.snapshot()) > beforeB })
	}
	verifyTraffic()
	if winner == a.ID() {
		ra.retireFlow(current(ra, b.ID()), "test generation failure")
	} else {
		rb.retireFlow(current(rb, a.ID()), "test generation failure")
	}
	wait(func() bool { return current(ra, b.ID()) == nil && current(rb, a.ID()) == nil })
	go func() { errorsOut <- ra.forwardPacket(ctx, packetA) }()
	go func() { errorsOut <- rb.forwardPacket(ctx, packetB) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errorsOut:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	wait(func() bool {
		fa, fb := current(ra, b.ID()), current(rb, a.ID())
		return fa != nil && fb != nil && fa.initiator == fb.initiator && !fa.retired.Load() && !fb.retired.Load()
	})
	verifyTraffic()
}

func TestPumpFromStreamLogsActualReadExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"EOF", io.EOF, "closed by peer: EOF"},
		{"reset", errors.New("stream reset by remote, error code 7"), "read failed: stream reset by remote, error code 7"},
		{"deadline", os.ErrDeadlineExceeded, "read failed: i/o timeout"},
		{"transport", errors.New("transport connection lost"), "read failed: transport connection lost"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			r := New(nil, nil)
			r.retireGrace = 0
			s := &readErrorFlowStream{fakeFlowStream: newFakeFlowStream(), err: tt.err}
			s.tag = "local-stream-42"
			flow := newPeerFlow(s, "remote", "self", []string{testPeerV4})
			r.installFlow(flow)
			r.pumpFromStream(flow)
			text := output.String()
			if !strings.Contains(text, tt.want) || !strings.Contains(text, "conn=unknown stream=local-stream-42") {
				t.Fatalf("missing real exit reason/stream identity in log: %s", text)
			}
			if tt.name != "EOF" && strings.Contains(text, "closed by peer") {
				t.Fatalf("non-EOF error misreported as peer closure: %s", text)
			}
			if !flow.retired.Load() || len(r.flows) != 0 || len(r.flowByAddr) != 0 {
				t.Fatal("read exit did not retire flow and aliases")
			}
		})
	}
}
