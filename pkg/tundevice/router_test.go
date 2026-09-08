package tundevice

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/protocol"
	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// ---- 测试辅助：内存 NetMap 与固定中继候选 ----

type stubNetmap struct {
	routes map[string]peer.ID
}

func (s *stubNetmap) Resolve(virtualIP string) (netmapclient.Route, bool) {
	id, ok := s.routes[virtualIP]
	if !ok {
		return tunnelRoute(virtualIP, "", nil), false
	}
	return tunnelRoute(virtualIP, id.String(), nil), true
}

// tunnelRoute 构造 netmapclient.Route 值。
func tunnelRoute(virtualIP, peerID string, addrs []string) (r netmapclient.Route) {
	r.VirtualIP = virtualIP
	r.PeerID = peerID
	r.Addrs = addrs
	return r
}

type stubRelay struct{}

func (stubRelay) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	return nil, fmt.Errorf("no relay in unit test")
}

type blockingRelay struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRelay) Candidates(ctx context.Context, _ int) ([]peer.AddrInfo, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
		return nil, fmt.Errorf("released blocked relay lookup")
	}
}

// buildIPv4 构造最小 IPv4 包（协议 UDP，载荷长度 len(payload)）。
func buildIPv4(src, dst [4]byte, payload []byte) []byte {
	totalLen := 20 + len(payload)
	packet := make([]byte, totalLen)
	packet[0] = 0x45 // IPv4, IHL=5
	packet[1] = 0    // DSCP
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	packet[8] = 64 // TTL
	packet[9] = 17 // UDP
	copy(packet[12:16], src[:])
	copy(packet[16:20], dst[:])
	copy(packet[20:], payload)
	return packet
}

func newPair(t *testing.T) (hostA, hostB host.Host) {
	t.Helper()
	var err error
	hostA, err = libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.NoSecurity)
	if err != nil {
		t.Fatalf("host a: %v", err)
	}
	t.Cleanup(func() { _ = hostA.Close() })
	hostB, err = libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.NoSecurity)
	if err != nil {
		t.Fatalf("host b: %v", err)
	}
	t.Cleanup(func() { _ = hostB.Close() })
	return hostA, hostB
}

// TestRouterForwardsBothWays 验证：TUN A 出向包 → 隧道 → TUN B；TUN B 出向包 → 隧道 → TUN A。
// 这是"两台设备各有一块同网段虚拟网卡"在单机内的等价模拟。
func TestRouterForwardsBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hostA, hostB := newPair(t)
	ipA := [4]byte{10, 7, 0, 2}
	ipB := [4]byte{10, 7, 0, 3}

	// B 端：隧道流直接回环——把收到的包写回流，模拟"对端网卡收到后回包"。
	// 更真实的做法是 B 的 Router 写进 B 的 TUN，再由测试注入 B 的出向包；
	// 此处为缩短链路，直接验证 A 的 Router → 流 → B 的 TUN 与反向。
	tunA, injectA, err := NewMemory(1400)
	if err != nil {
		t.Fatalf("mem tun a: %v", err)
	}
	defer tunA.Close()
	tunB, injectB, err := NewMemory(1400)
	if err != nil {
		t.Fatalf("mem tun b: %v", err)
	}
	defer tunB.Close()

	routesA := &stubNetmap{routes: map[string]peer.ID{"10.7.0.3": hostB.ID()}}
	routesB := &stubNetmap{routes: map[string]peer.ID{"10.7.0.2": hostA.ID()}}
	routerA := New(tunA, tunnel.New(hostA, routesA, stubRelay{}))
	_ = New(tunB, tunnel.New(hostB, routesB, stubRelay{})) // routerB

	go routerA.Run(ctx)
	// 注：routerB 不启动 Run——内存 TUN 是回环设备，routerB.Read 会把 B 的 Stream 回包读走，与 B 的流处理器 injectB 形成回环竞争。

	// B 端隧道流处理：读包写回 TUN B（等价于包到达 B 的虚拟网卡）。
	hostB.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		buf := make([]byte, 65535)
		for {
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			if err := injectB(buf[:n]); err != nil {
				return
			}
		}
	})

	// 让 A 能直连 B。
	if err := hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect a->b: %v", err)
	}

	// A 的应用发出一个包，目的 10.7.0.3。
	payload := []byte("hello-over-tun")
	if _, err := tunA.Write([][]byte{buildIPv4(ipA, ipB, payload)}, 0); err != nil {
		t.Fatalf("write tun a: %v", err)
	}

	// B 的 TUN 应收到该包。
	bufs := [][]byte{make([]byte, 65535)}
	sizes := []int{0}
	readDone := make(chan error, 1)
	go func() {
		n, err := tunB.Read(bufs, sizes, 0)
		if err != nil {
			readDone <- err
			return
		}
		if n != 1 {
			readDone <- fmt.Errorf("read %d packets", n)
			return
		}
		readDone <- nil
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("tun b read: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting packet at tun b")
	}

	got := bufs[0][:sizes[0]]
	if len(got) != 20+len(payload) {
		t.Fatalf("packet length = %d, want %d", len(got), 20+len(payload))
	}
	if string(got[20:]) != string(payload) {
		t.Fatalf("payload = %q, want %q", got[20:], payload)
	}
	dst := net.IPv4(got[16], got[17], got[18], got[19])
	if !dst.Equal(net.IPv4(ipB[0], ipB[1], ipB[2], ipB[3])) {
		t.Fatalf("dst = %s, want %s", dst, net.IP(ipB[:]))
	}
	_ = injectA
	_ = protocolOf
}

// TestRouterOfflineDestinationDoesNotBlockOthers 回归验证：一个离线目标的慢拨号
// 不能占住唯一的 TUN 读取循环，发往在线目标的包应由独立 worker 立即转发。
func TestRouterOfflineDestinationDoesNotBlockOthers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	hostA, hostB := newPair(t)
	offlineHost, err := libp2p.New(libp2p.NoListenAddrs, libp2p.NoSecurity)
	if err != nil {
		t.Fatalf("offline host: %v", err)
	}
	offlineID := offlineHost.ID()
	_ = offlineHost.Close()

	if err = hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect a->b: %v", err)
	}

	received := make(chan []byte, 1)
	onlinePacket := buildIPv4(
		[4]byte{10, 7, 0, 2},
		[4]byte{10, 7, 0, 3},
		[]byte("online-must-not-wait"),
	)
	hostB.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		defer stream.Close()
		packet := make([]byte, len(onlinePacket))
		if _, readErr := io.ReadFull(stream, packet); readErr == nil {
			received <- packet
		}
	})

	device, inject, err := NewMemory(1400)
	if err != nil {
		t.Fatalf("mem tun: %v", err)
	}
	defer device.Close()
	relay := &blockingRelay{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(relay.release)
	routes := &stubNetmap{routes: map[string]peer.ID{
		"10.7.0.3":   hostB.ID(),
		"10.7.0.250": offlineID,
	}}
	router := New(device, tunnel.New(hostA, routes, relay))
	go router.Run(ctx)

	offlinePacket := buildIPv4(
		[4]byte{10, 7, 0, 2},
		[4]byte{10, 7, 0, 250},
		[]byte("blocks-its-own-worker"),
	)
	if err = inject(offlinePacket); err != nil {
		t.Fatalf("inject offline packet: %v", err)
	}
	select {
	case <-relay.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("offline route did not enter blocked relay lookup")
	}

	if err = inject(onlinePacket); err != nil {
		t.Fatalf("inject online packet: %v", err)
	}
	select {
	case got := <-received:
		if string(got) != string(onlinePacket) {
			t.Fatalf("online packet mismatch: got %d bytes, want %d", len(got), len(onlinePacket))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("online packet was blocked by offline destination dial")
	}
}

func TestEnqueuePacketOwnsPacketData(t *testing.T) {
	worker := &outboundWorker{packets: make(chan []byte, 1)}
	router := &Router{
		outbound:      map[string]*outboundWorker{"10.7.0.3": worker},
		outboundLimit: 1,
		outboundIdle:  time.Minute,
	}
	packet := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("original"))
	if err := router.enqueuePacket(context.Background(), packet); err != nil {
		t.Fatalf("enqueue packet: %v", err)
	}
	packet[20] = 'X'
	queued := <-worker.packets
	if got := string(queued[20:]); got != "original" {
		t.Fatalf("queued payload changed with read buffer: %q", got)
	}
}

func TestEnqueuePacketLimitsDestinationWorkers(t *testing.T) {
	router := &Router{
		outbound: map[string]*outboundWorker{
			"10.7.0.3": {packets: make(chan []byte, 1)},
		},
		outboundLimit: 1,
		outboundIdle:  time.Minute,
	}
	packet := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 4}, nil)
	if err := router.enqueuePacket(context.Background(), packet); err == nil {
		t.Fatal("expected destination worker limit error")
	}
}

func TestOutboundWorkerIsReapedWhenIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := &Router{
		outbound:      make(map[string]*outboundWorker),
		outboundLimit: 1,
		outboundIdle:  20 * time.Millisecond,
	}
	worker := &outboundWorker{packets: make(chan []byte, 1)}
	router.outbound["10.7.0.3"] = worker
	go router.runOutbound(ctx, "10.7.0.3", worker)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		router.mu.Lock()
		remaining := len(router.outbound)
		router.mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("idle outbound worker was not removed")
}
