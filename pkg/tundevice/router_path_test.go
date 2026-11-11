package tundevice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

type pathTestConn struct {
	network.Conn
	id string
}

func (c *pathTestConn) ID() string          { return c.id }
func (c *pathTestConn) RemotePeer() peer.ID { return "remote" }
func (c *pathTestConn) RemoteMultiaddr() ma.Multiaddr {
	return ma.StringCast("/ip4/127.0.0.1/tcp/1234")
}

type pathTestStream struct {
	*fakeFlowStream
	conn network.Conn
}

func (s *pathTestStream) Conn() network.Conn { return s.conn }
func pathStream(id string) *pathTestStream {
	return &pathTestStream{newFakeFlowStream(), &pathTestConn{id: id}}
}

func TestFlowFailureFeedbackFiltersLocalLifecycle(t *testing.T) {
	h, _ := newPair(t)
	svc := tunnel.New(h, &stubNetmap{}, stubRelay{})
	r := New(nil, svc)
	r.retireGrace = 0
	defer r.Close()
	f := newPeerFlow(pathStream("active"), "remote", "self", []string{testPeerV4})
	r.installFlow(f)
	for _, err := range []error{nil, io.EOF, context.Canceled} {
		if r.reportFlowFailure(f, err) {
			t.Fatalf("local/normal error reported: %v", err)
		}
	}
	failure := errors.New("transport lost")
	f.localReset.Store(true)
	if r.reportFlowFailure(f, failure) {
		t.Fatal("local reset reported")
	}
	f.localReset.Store(false)
	stale := newPeerFlow(pathStream("old"), "remote", "self", nil)
	if r.reportFlowFailure(stale, failure) {
		t.Fatal("noncurrent flow reported")
	}
	if !r.reportFlowFailure(f, failure) {
		t.Fatal("current flow I/O failure not reported")
	}
	if len(svc.PathHealth("remote")) != 1 {
		t.Fatal("missing actual path health feedback")
	}
	if !r.reportFlowFailure(f, context.DeadlineExceeded) {
		t.Fatal("real stream I/O deadline must report blackhole evidence")
	}
	f.retired.Store(true)
	if r.reportFlowFailure(f, failure) {
		t.Fatal("retired flow reported")
	}
}

type failedPathStream struct {
	*pathTestStream
	cause error
}

func (s *failedPathStream) Read([]byte) (int, error)  { return 0, s.cause }
func (s *failedPathStream) Write([]byte) (int, error) { return 0, s.cause }

func TestRouterIOFailureReportsActualStreamPath(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		t.Run(direction, func(t *testing.T) {
			h, _ := newPair(t)
			svc := tunnel.New(h, &stubNetmap{}, stubRelay{})
			r := New(nil, svc)
			r.retireGrace = 0
			defer r.Close()
			s := &failedPathStream{pathStream("io-path"), context.DeadlineExceeded}
			f := newPeerFlow(s, "remote", "self", []string{testPeerV4})
			r.installFlow(f)
			if direction == "read" {
				r.pumpFromStream(f)
			} else {
				pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, nil)
				if err := r.forwardPacket(context.Background(), pkt); err == nil {
					t.Fatal("expected I/O failure")
				}
			}
			health := svc.PathHealth("remote")
			if len(health) != 1 || health[0].Failures == 0 {
				t.Fatal("I/O deadline did not feed back actual stream path")
			}
			if !f.retired.Load() || r.flows["remote"] != nil {
				t.Fatal("failed flow was not retired")
			}
		})
	}
}

func TestInvalidatePathMatchesCurrentConnectionOnly(t *testing.T) {
	r := New(nil, nil)
	r.retireGrace = 10 * time.Millisecond
	oldStream := pathStream("old")
	old := newPeerFlow(oldStream, "remote", "remote", []string{testPeerV4, testPeerV6})
	r.installFlow(old)
	other := newPeerFlow(pathStream("other"), "other", "other", []string{"10.7.0.4"})
	r.installFlow(other)
	defer r.Close()
	for _, tc := range []struct {
		p peer.ID
		c string
	}{{"remote", ""}, {"", "old"}, {"absent", "old"}, {"remote", "wrong"}} {
		if r.InvalidatePath(tc.p, tc.c) {
			t.Fatalf("invalidated nonmatching path: %+v", tc)
		}
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.InvalidatePath("remote", "old") {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners=%d", winners.Load())
	}
	if !old.retired.Load() || !oldStream.closed.Load() {
		t.Fatal("matching flow not gracefully retired")
	}
	if len(r.flows) != 1 || r.flows["other"] != other || len(r.flowByAddr) != 1 {
		t.Fatal("retirement affected another peer or left dual-stack aliases")
	}
	next := newPeerFlow(pathStream("new"), "remote", "remote", []string{testPeerV4, testPeerV6})
	r.installFlow(next)
	if r.InvalidatePath("remote", "old") {
		t.Fatal("late probe invalidated replacement")
	}
	r.retireFlow(old, "late cleanup")
	if r.flows["remote"] != next || next.retired.Load() {
		t.Fatal("old cleanup damaged replacement")
	}
	for _, a := range next.addrs {
		if r.flowByAddr[a] != "remote" {
			t.Fatal("replacement alias removed")
		}
	}
	deadline := time.Now().Add(time.Second)
	for !oldStream.reset.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !oldStream.reset.Load() {
		t.Fatal("retireGrace did not reclaim old stream")
	}
	if next.stream.(*pathTestStream).closed.Load() || other.stream.(*pathTestStream).closed.Load() {
		t.Fatal("unrelated stream was closed")
	}
}

func TestInvalidatePathNilConnectionAndClosedRouter(t *testing.T) {
	r := New(nil, nil)
	f := newPeerFlow(newFakeFlowStream(), "remote", "remote", []string{testPeerV4})
	r.installFlow(f)
	if r.InvalidatePath("remote", "unknown") {
		t.Fatal("nil connection matched")
	}
	r.Close()
	if r.InvalidatePath("remote", "unknown") {
		t.Fatal("closed router invalidated flow")
	}
}

func TestInvalidatePathConcurrentReplacementKeepsAliases(t *testing.T) {
	for i := 0; i < 100; i++ {
		r := New(nil, nil)
		r.retireGrace = 0
		old := newPeerFlow(pathStream("old"), "remote", "remote", []string{testPeerV4, testPeerV6})
		next := newPeerFlow(pathStream("new"), "remote", "remote", []string{testPeerV4, testPeerV6})
		r.installFlow(old)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); r.InvalidatePath("remote", "old") }()
		go func() { defer wg.Done(); r.installFlow(next) }()
		wg.Wait()
		r.retireFlow(old, "late cleanup")
		assertCurrentDualStackFlow(t, r, next)
		r.Close()
	}
}

// A blocked packet write must be released by the existing retireGrace Reset;
// both the in-flight packet and queued packets must return their byte budget.
func TestInvalidatePathReleasesBlockedWriteBudget(t *testing.T) {
	r := New(nil, nil)
	r.retireGrace = 10 * time.Millisecond
	blocked := newMockBlockingStream()
	s := &blockingPathStream{mockBlockingStream: blocked, conn: &pathTestConn{id: "bad"}}
	flow := newPeerFlow(s, "remote", "self", []string{testPeerV4, testPeerV6})
	r.installFlow(flow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pkt := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("blocked"))
	if err := r.enqueuePacket(ctx, pkt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.Blocked():
	case <-time.After(time.Second):
		t.Fatal("write not blocked")
	}
	if !r.InvalidatePath("remote", "bad") {
		t.Fatal("path not invalidated")
	}
	select {
	case <-blocked.ResetCalled():
	case <-time.After(time.Second):
		t.Fatal("retireGrace did not unblock write")
	}
	r.Close()
	if got := atomic.LoadInt64(&r.writeBudget); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

type blockingPathStream struct {
	*mockBlockingStream
	conn network.Conn
}

func (s *blockingPathStream) Conn() network.Conn { return s.conn }

type packetPathStream struct {
	*fakeFlowStream
	reader *bytes.Reader
}

func (s *packetPathStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func TestInboundSourceBindingStillAppliesFirewall(t *testing.T) {
	pkt := buildIPv4([4]byte{10, 7, 0, 3}, [4]byte{10, 7, 0, 2}, make([]byte, 8))
	d := &recordingDevice{}
	r := New(d, nil)
	r.retireGrace = 0
	fw := firewall.New()
	fw.Set(firewall.ModeAllowList, nil)
	r.SetFirewall(fw)
	s := &packetPathStream{newFakeFlowStream(), bytes.NewReader(pkt)}
	f := newPeerFlow(s, "remote", "remote", []string{testPeerV4, testPeerV6})
	r.installFlow(f)
	r.pumpFromStream(f)
	if len(d.snapshot()) != 0 {
		t.Fatal("source binding bypassed firewall")
	}
}

func TestInboundFlowRejectsSpoofedSourcesDualStack(t *testing.T) {
	good4 := buildIPv4([4]byte{10, 7, 0, 3}, [4]byte{192, 168, 1, 9}, []byte("forwarded destination allowed"))
	bad4 := buildIPv4([4]byte{10, 7, 0, 4}, [4]byte{10, 7, 0, 2}, []byte("spoof"))
	good6 := buildIPv6Packet(testPeerV6, "fd00:6c61:6e65::2", []byte("v6"))
	bad6 := buildIPv6Packet("fd00:6c61:6e65::4", "fd00:6c61:6e65::2", []byte("spoof"))
	data := bytes.Join([][]byte{bad4, good4, bad6, good6}, nil)
	d := &recordingDevice{}
	r := New(d, nil)
	r.retireGrace = 0
	s := &packetPathStream{newFakeFlowStream(), bytes.NewReader(data)}
	f := newPeerFlow(s, "remote", "remote", []string{testPeerV4, testPeerV6})
	r.installFlow(f)
	r.pumpFromStream(f)
	got := d.snapshot()
	if len(got) != 2 || !bytes.Equal(got[0], good4) || !bytes.Equal(got[1], good6) {
		t.Fatalf("source binding failed: got %d packets", len(got))
	}
	for _, pkt := range [][]byte{nil, {0x45}, {0x60}, make([]byte, 40)} {
		if r.acceptFlowSource(f, pkt) {
			t.Fatal("malformed/retired source accepted")
		}
	}
}
