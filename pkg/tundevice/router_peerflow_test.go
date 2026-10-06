package tundevice

import (
	"context"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	libprotocol "github.com/libp2p/go-libp2p/core/protocol"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/protocol"
	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// 本文件锁定「同一 peer 只保留一条隧道流」这条不变量。
//
// 背景（线上故障）：同一 peer 在 NetMap 里同时拥有 IPv4 与 IPv6 两个虚拟地址。
// 旧实现出向流按目的 IP 建表、入向流却按 v4+v6 两个别名归一，同一 peer 因此
// 出现两条流；入向注册又把同 peer 另一 key 上的老流 Reset 掉，两端互拆，
// 表现为隧道每 1~3 秒重建一次 + "stream reset by remote, error code 0"，
// 对外被误读为节点持续掉线。

// dualStackNetmap 同时登记一个 peer 的 IPv4 与 IPv6 虚拟地址。
type dualStackNetmap struct {
	v4 string
	v6 string
	id peer.ID
}

func (d *dualStackNetmap) Resolve(virtualIP string) (netmapclient.Route, bool) {
	if virtualIP != d.v4 && virtualIP != d.v6 {
		return netmapclient.Route{}, false
	}
	return netmapclient.Route{VirtualIP: d.v4, VirtualIPv6: d.v6, PeerID: d.id.String()}, true
}

func (d *dualStackNetmap) Routes() []netmapclient.Route {
	return []netmapclient.Route{{VirtualIP: d.v4, VirtualIPv6: d.v6, PeerID: d.id.String()}}
}

const testPeerV4 = "10.7.0.3"
const testPeerV6 = "fd00:6c61:6e65::3"

// buildIPv6Packet 构造最小 IPv6 包（next header = UDP）。
func buildIPv6Packet(src, dst string, payload []byte) []byte {
	s := mustAddr(src)
	d := mustAddr(dst)
	payloadLen := len(payload)
	packet := make([]byte, 40+payloadLen)
	packet[0] = 0x60 // version 6
	packet[4] = byte(payloadLen >> 8)
	packet[5] = byte(payloadLen)
	packet[6] = 17 // UDP
	copy(packet[8:24], s.AsSlice())
	copy(packet[24:40], d.AsSlice())
	copy(packet[40:], payload)
	return packet
}

// TestIPv4AndIPv6OfSamePeerShareOneTunnelStream 是本次故障的核心回归用例：
// 先给同一 peer 的 IPv4 发包建流，再给它的 IPv6 发包 —— 后者必须复用同一条流，
// 而不是再建第二条（旧实现的拆流根源）。
func TestIPv4AndIPv6OfSamePeerShareOneTunnelStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hostA, hostB := newPair(t)
	if err := hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect a->b: %v", err)
	}

	var opened int32
	received := make(chan struct{}, 8)
	hostB.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		atomic.AddInt32(&opened, 1)
		buf := make([]byte, 65535)
		for {
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				select {
				case received <- struct{}{}:
				default:
				}
			}
		}
	})

	device, inject, err := NewMemory(1400)
	if err != nil {
		t.Fatalf("mem tun: %v", err)
	}
	defer device.Close()

	routes := &dualStackNetmap{v4: testPeerV4, v6: testPeerV6, id: hostB.ID()}
	router := New(device, tunnel.New(hostA, routes, stubRelay{}))
	defer router.Close()
	go router.Run(ctx)

	v4Packet := buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("v4-payload"))
	if err = inject(v4Packet); err != nil {
		t.Fatalf("inject v4: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("IPv4 包未送达对端")
	}

	v6Packet := buildIPv6Packet("fd00:6c61:6e65::2", testPeerV6, []byte("v6-payload"))
	if err = inject(v6Packet); err != nil {
		t.Fatalf("inject v6: %v", err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("IPv6 包未送达对端")
	}

	router.mu.Lock()
	flowCount := len(router.flows)
	byV4, v4Bound := router.flowByAddr[testPeerV4]
	byV6, v6Bound := router.flowByAddr[testPeerV6]
	sameFlow := router.flows[hostB.ID()]
	router.mu.Unlock()

	if flowCount != 1 {
		t.Fatalf("同一 peer 的流数 = %d, want 1（IPv4/IPv6 必须共用一条流）", flowCount)
	}
	if !v4Bound || !v6Bound || byV4 != hostB.ID() || byV6 != hostB.ID() {
		t.Fatalf("别名索引不一致: v4=%v(%v) v6=%v(%v)", byV4, v4Bound, byV6, v6Bound)
	}
	if sameFlow == nil {
		t.Fatal("流表缺少该 peer 的流")
	}
	if got := atomic.LoadInt32(&opened); got != 1 {
		t.Fatalf("对端被打开 %d 条隧道流, want 1（IPv6 包应复用 v4 建立的流）", got)
	}
}

// TestDualStackPeerIsNotDialedTwiceConcurrently 验证拨号 single-flight 按 PeerID
// 归并：IPv4 与 IPv6 包几乎同时到达也只发起一次拨号。
func TestDualStackPeerIsNotDialedTwiceConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hostA, hostB := newPair(t)
	if err := hostA.Connect(ctx, peer.AddrInfo{ID: hostB.ID(), Addrs: hostB.Addrs()}); err != nil {
		t.Fatalf("connect a->b: %v", err)
	}

	var opened int32
	hostB.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		atomic.AddInt32(&opened, 1)
		<-ctx.Done()
		_ = stream.Close()
	})

	device, inject, err := NewMemory(1400)
	if err != nil {
		t.Fatalf("mem tun: %v", err)
	}
	defer device.Close()

	routes := &dualStackNetmap{v4: testPeerV4, v6: testPeerV6, id: hostB.ID()}
	router := New(device, tunnel.New(hostA, routes, stubRelay{}))
	defer router.Close()
	go router.Run(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = inject(buildIPv4([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("x")))
			} else {
				_ = inject(buildIPv6Packet("fd00:6c61:6e65::2", testPeerV6, []byte("x")))
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		router.mu.Lock()
		n := len(router.flows)
		router.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	router.mu.Lock()
	n := len(router.flows)
	dials := len(router.dialing)
	router.mu.Unlock()
	if n != 1 {
		t.Fatalf("并发双栈包后流数 = %d, want 1", n)
	}
	if dials != 0 {
		t.Fatalf("拨号未收敛，残留 dialing = %d", dials)
	}
	if got := atomic.LoadInt32(&opened); got != 1 {
		t.Fatalf("对端被打开 %d 条隧道流, want 1", got)
	}
}

// TestInstallFlowArbitrationIsSymmetric 覆盖真正的对称竞争场景：**两端同时向对方拨号**。
// 于是 A 的流表里有两条竞争流（自己拨的 initiator=A，对端拨进来的 initiator=B），
// B 的流表里同样是这两条（只是视角相反）。两端各自算出 min(发起方) 作为赢家，
// 结果必然一致 —— 不会出现「A 留自己拨的、B 留自己拨的」互相对拆。
// 同时验证安装顺序无关。
func TestInstallFlowArbitrationIsSymmetric(t *testing.T) {
	idA := peer.ID("12D3KooWR4qSqJtc5LCzfWrNqBQ8rmr35YrwQR159d9H6fxhRigF")
	idB := peer.ID("12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p")

	// 同一对 peer（idA 与 idB）上出现的两条竞争流：
	//   dialedByA —— A 主动拨的，A 侧 initiator=A，B 侧 initiator=A
	//   dialedByB —— B 主动拨的，A 侧 initiator=B，B 侧 initiator=B
	// 差别只在 peerFlow.peer 指向谁（永远是「对端」）。
	dialedByAOnA := func() *peerFlow { return newPeerFlow(newFakeFlowStream(), idB, idA, []string{testPeerV4}) }
	dialedByBOnA := func() *peerFlow { return newPeerFlow(newFakeFlowStream(), idB, idB, []string{testPeerV4, testPeerV6}) }
	dialedByAOnB := func() *peerFlow { return newPeerFlow(newFakeFlowStream(), idA, idA, []string{testPeerV4, testPeerV6}) }
	dialedByBOnB := func() *peerFlow { return newPeerFlow(newFakeFlowStream(), idA, idB, []string{testPeerV4}) }

	newRouter := func(self peer.ID) *Router {
		r := New(nil, nil)
		r.selfID = self
		r.retireGrace = time.Hour // 淘汰只 Close，不立刻 Reset，便于断言
		return r
	}
	keptInitiator := func(r *Router) peer.ID {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, f := range r.flows {
			return f.initiator
		}
		return ""
	}

	// A 视角：自己拨的（initiator=A）对上对端拨进来的（initiator=B）。
	routerA := newRouter(idA)
	routerA.installFlow(dialedByAOnA())
	routerA.installFlow(dialedByBOnA())

	routerA2 := newRouter(idA)
	routerA2.installFlow(dialedByBOnA())
	routerA2.installFlow(dialedByAOnA())

	// B 视角：同一对竞争流，只是视角相反。
	routerB := newRouter(idB)
	routerB.installFlow(dialedByBOnB())
	routerB.installFlow(dialedByAOnB())

	routerB2 := newRouter(idB)
	routerB2.installFlow(dialedByAOnB())
	routerB2.installFlow(dialedByBOnB())

	winner := idA
	if idB < idA {
		winner = idB
	}
	for name, r := range map[string]*Router{"A": routerA, "A2": routerA2, "B": routerB, "B2": routerB2} {
		r.mu.Lock()
		n := len(r.flows)
		r.mu.Unlock()
		if n != 1 {
			t.Fatalf("%s: 流表条数 = %d, want 1（每个对端恰好一条流）", name, n)
		}
		if got := keptInitiator(r); got != winner {
			t.Fatalf("%s: 留存流发起方 = %s, want %s", name, got, winner)
		}
	}
}

// TestArbitrationLoserIsClosedNotReset 验证淘汰走 Close 而非 Reset：
// Reset 会让对端收到 STREAM_RESET(code 0) 并打出 "stream reset by remote,
// error code 0"，正是本次故障日志里的刷屏信号；Close 让对端读到 EOF 干净退出。
func TestArbitrationLoserIsClosedNotReset(t *testing.T) {
	// 取字典序更小的一方作为本机，使其主动拨出的流成为仲裁赢家。
	selfID := peer.ID("12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p")
	remoteID := peer.ID("12D3KooWR4qSqJtc5LCzfWrNqBQ8rmr35YrwQR159d9H6fxhRigF")
	if !(selfID < remoteID) {
		t.Skip("需要 selfID < remoteID 才能让出向流胜出")
	}

	router := New(nil, nil)
	router.selfID = selfID
	router.retireGrace = time.Hour

	outbound := newPeerFlow(newFakeFlowStream(), remoteID, selfID, []string{testPeerV4})
	inbound := newPeerFlow(newFakeFlowStream(), remoteID, remoteID, []string{testPeerV4, testPeerV6})

	if _, ok := router.installFlow(outbound); !ok {
		t.Fatal("安装出向流失败")
	}
	kept, ok := router.installFlow(inbound)
	if !ok {
		t.Fatal("安装入向流失败")
	}
	if kept != outbound {
		t.Fatal("发起方更小的出向流应当获胜")
	}

	loser := inbound.stream.(*fakeFlowStream)
	winner := outbound.stream.(*fakeFlowStream)
	if !loser.closed.Load() {
		t.Fatal("败者未被 Close")
	}
	if loser.reset.Load() {
		t.Fatal("败者被立刻 Reset，会让对端打出误导性的 stream reset by remote error code 0")
	}
	if winner.closed.Load() || winner.reset.Load() {
		t.Fatal("胜者不应被关闭")
	}

	router.mu.Lock()
	n := len(router.flows)
	aliasV6, hasV6 := router.flowByAddr[testPeerV6]
	router.mu.Unlock()
	if n != 1 {
		t.Fatalf("仲裁后流数 = %d, want 1", n)
	}
	if !hasV6 || aliasV6 != remoteID {
		t.Fatalf("别名索引未更新: %v", aliasV6)
	}
}

// TestRetiredFlowStopsDeliveringToTUN 验证被淘汰的流不再往 TUN 投递包：
// 老流与新流同时活着时，对端同一份数据会在本机协议栈出现两次。
func TestRetiredFlowStopsDeliveringToTUN(t *testing.T) {
	flow := newPeerFlow(newFakeFlowStream(), peer.ID("p"), peer.ID("p"), []string{testPeerV4})
	if flow.retired.Load() {
		t.Fatal("新流不应处于 retired 状态")
	}
	flow.retired.Store(true)
	if !flow.retired.Load() {
		t.Fatal("retired 标记未生效")
	}
}

// TestLookupPeerResolvesBothAliasesToSamePeer 验证别名解析：
// v4 与 v6 解析到同一 PeerID，并互相回填别名索引。
func TestLookupPeerResolvesBothAliasesToSamePeer(t *testing.T) {
	hostA, hostB := newPair(t)
	routes := &dualStackNetmap{v4: testPeerV4, v6: testPeerV6, id: hostB.ID()}
	router := New(nil, tunnel.New(hostA, routes, stubRelay{}))

	for _, addr := range []string{testPeerV4, testPeerV6} {
		got, err := router.lookupPeer(addr)
		if err != nil {
			t.Fatalf("lookupPeer(%s): %v", addr, err)
		}
		if got != hostB.ID() {
			t.Fatalf("lookupPeer(%s) = %s, want %s", addr, got, hostB.ID())
		}
	}

	router.mu.Lock()
	byV4 := router.flowByAddr[testPeerV4]
	byV6 := router.flowByAddr[testPeerV6]
	router.mu.Unlock()
	if byV4 != hostB.ID() || byV6 != hostB.ID() {
		t.Fatalf("别名未回填: v4=%s v6=%s", byV4, byV6)
	}

	// 再解析一次应命中别名索引，不再触碰 NetMap。
	routes.v4 = "" // 破坏 NetMap 后仍应命中缓存
	if got, err := router.lookupPeer(testPeerV6); err != nil || got != hostB.ID() {
		t.Fatalf("别名缓存未生效: got=%s err=%v", got, err)
	}
}

func mustAddr(s string) netip.Addr {
	parsed, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return parsed
}

// fakeFlowStream 是只记录动作、不产生 I/O 的 network.Stream，用于断言
// 「淘汰走 Close 而不是 Reset」。
type fakeFlowStream struct {
	tag    string
	closed atomic.Bool
	reset  atomic.Bool
}

func newFakeFlowStream() *fakeFlowStream { return &fakeFlowStream{} }

func (f *fakeFlowStream) Read([]byte) (int, error) {
	time.Sleep(time.Hour)
	return 0, io.EOF
}
func (f *fakeFlowStream) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeFlowStream) Close() error                 { f.closed.Store(true); return nil }
func (f *fakeFlowStream) CloseWrite() error            { return nil }
func (f *fakeFlowStream) CloseRead() error             { return nil }
func (f *fakeFlowStream) Reset() error {
	f.reset.Store(true)
	return nil
}
func (f *fakeFlowStream) ResetWithError(network.StreamErrorCode) error { return f.Reset() }
func (f *fakeFlowStream) SetDeadline(time.Time) error                 { return nil }
func (f *fakeFlowStream) SetReadDeadline(time.Time) error             { return nil }
func (f *fakeFlowStream) SetWriteDeadline(time.Time) error            { return nil }
func (f *fakeFlowStream) ID() string                                  { return f.tag }
func (f *fakeFlowStream) Protocol() libprotocol.ID                    { return protocol.Tunnel }
func (f *fakeFlowStream) SetProtocol(libprotocol.ID) error            { return nil }
func (f *fakeFlowStream) Stat() network.Stats                         { return network.Stats{} }
func (f *fakeFlowStream) Conn() network.Conn                          { return nil }
func (f *fakeFlowStream) Scope() network.StreamScope                  { return nil }