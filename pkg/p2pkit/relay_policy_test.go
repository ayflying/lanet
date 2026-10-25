package p2pkit

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

// 验证真实普通节点中继，不以专用中继或伪造流替代生产启动入口。
func TestNodeRelayLargeTransfer(t *testing.T) {
	testNodeRelayTransfer(t, 0)
}

// 常规测试检查预约返回的无连接限额及大流量；显式长测另外跨过默认120秒。
func TestNodeRelayBeyondDefaultDuration(t *testing.T) {
	if os.Getenv("LANET_LONG_RELAY_TEST") != "1" {
		t.Skip("设置 LANET_LONG_RELAY_TEST=1 执行超过两分钟的真实中继长测")
	}
	testNodeRelayTransfer(t, 125*time.Second)
}

func testNodeRelayTransfer(t *testing.T, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration+20*time.Second)
	defer cancel()
	hop, err := NewHost(ctx, HostSpec{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, RelayServiceAlways: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hop.Close() })
	newEndpoint := func() host.Host {
		h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.EnableRelay())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	target, source := newEndpoint(), newEndpoint()
	reservation, err := relayclient.Reserve(ctx, target, peer.AddrInfo{ID: hop.ID(), Addrs: hop.Addrs()})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.LimitDuration != 0 || reservation.LimitData != 0 {
		t.Fatalf("普通节点仍宣告临时连接限额: duration=%s data=%d", reservation.LimitDuration, reservation.LimitData)
	}
	const proto protocol.ID = "/lanet/relay-policy-regression/1"
	target.SetStreamHandler(proto, func(st network.Stream) {
		defer st.Close()
		_, _ = io.Copy(st, st)
	})
	circuit := hop.Addrs()[0].Encapsulate(ma.StringCast("/p2p/" + hop.ID().String() + "/p2p-circuit"))
	if err := source.Connect(ctx, peer.AddrInfo{ID: target.ID(), Addrs: []ma.Multiaddr{circuit}}); err != nil {
		t.Fatal(err)
	}
	st, err := source.NewStream(network.WithAllowLimitedConn(ctx, "真实中继回归"), target.ID(), proto)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Conn().RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT); err != nil {
		t.Fatal("测试意外走了直连")
	}
	connID := st.Conn().ID()
	payload := bytes.Repeat([]byte("lanet-stable-relay"), 4096)
	buf := make([]byte, len(payload))
	exchange := func() {
		t.Helper()
		_ = st.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.Write(payload); err != nil {
			t.Fatalf("中继写入失败: %v", err)
		}
		if _, err := io.ReadFull(st, buf); err != nil {
			t.Fatalf("中继读取失败: %v", err)
		}
		if !bytes.Equal(payload, buf) {
			t.Fatal("中继回显内容不一致")
		}
	}
	// 超过每方向128KiB，并避免整块大写与echo形成双向缓冲死锁。
	for i := 0; i < 16; i++ {
		exchange()
	}
	start := time.Now()
	if duration > 0 {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for time.Since(start) < duration {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-ticker.C:
				exchange()
			}
		}
	}
	if st.Conn().ID() != connID || st.Conn().IsClosed() {
		t.Fatal("长测没有保持同一条底层中继连接")
	}
	t.Logf("真实中继同连接双向传输通过，持续%s，初始每方向%d字节", time.Since(start).Round(time.Millisecond), 16*len(payload))
}
