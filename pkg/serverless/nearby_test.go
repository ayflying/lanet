package serverless

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestNearbyProbeUntrustedNameAndIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := testHost(t, false), testHost(t, false)
	var pending atomic.Int32
	da, err := New(ctx, a, Config{NetworkKey: "nearby-group", Name: "请求方", IsTrusted: func(string) bool { return false }, OnPending: func(string, []string, string) { pending.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	db, err := New(ctx, b, Config{NetworkKey: "nearby-group", Name: "设备-B", IsTrusted: func(string) bool { return false }, OnPending: func(string, []string, string) { pending.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := da.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := da.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()})
	if err != nil || !got.Alive || got.Name != "设备-B" || got.PeerID != b.ID().String() {
		t.Fatalf("探测结果 %+v, %v", got, err)
	}
	if pending.Load() != 0 || len(da.Peers()) != 0 || len(db.Peers()) != 0 {
		t.Fatalf("探测不得触发待审批或成员关系: pending=%d A=%v B=%v", pending.Load(), da.Peers(), db.Peers())
	}
	if _, err := da.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID()}); !errors.Is(err, ErrNearbyCooldown) {
		t.Fatalf("重复探测应冷却: %v", err)
	}
}

func TestNearbyProbeDifferentGroupAndUnsupported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := testHost(t, false), testHost(t, false)
	da, err := New(ctx, a, Config{NetworkKey: "nearby-A"})
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	db, err := New(ctx, b, Config{NetworkKey: "nearby-B", Name: "秘密名称"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := da.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = da.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()})
	if !errors.Is(err, ErrNearbyUnsupported) {
		t.Fatalf("异群不应协商成功: %v", err)
	}
	_, err = da.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID()})
	if !errors.Is(err, ErrNearbyCooldown) {
		t.Fatalf("旧版或异群不应反复协商: %v", err)
	}
}

func TestNearbyProbeRejectsInvalidProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := testHost(t, false), testHost(t, false)
	d, err := New(ctx, b, Config{NetworkKey: "nearby-proof", Name: "不可泄漏"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatal(err)
	}
	s, err := a.NewStream(ctx, b.ID(), d.protoNearby)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _ = s.Write(make([]byte, nearbyNonceBytes+sha256.Size))
	_ = s.CloseWrite()
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	body, _ := io.ReadAll(s)
	if strings.Contains(string(body), "不可泄漏") {
		t.Fatalf("没有网络密钥证明不得读取设备名: %q", body)
	}
}

func TestNearbyProbeRejectsForgedResponseAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := testHost(t, false), testHost(t, false)
	d, err := New(ctx, a, Config{NetworkKey: "nearby-replay"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	firstResponse := make(chan []byte, 1)
	b.SetStreamHandler(d.protoNearby, func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(3 * time.Second))
		var req [nearbyNonceBytes + sha256.Size]byte
		if _, err := io.ReadFull(s, req[:]); err != nil {
			return
		}
		name := []byte("伪造的在线设备")
		response := make([]byte, 3+len(name)+sha256.Size)
		response[0] = 1
		binary.BigEndian.PutUint16(response[1:3], uint16(len(name)))
		copy(response[3:], name)
		// 交给主 goroutine 的必须是独立副本：主 goroutine 收到后会在原地改写签名段
		// （见下面 copy(first[...])），若与 handler 自己的 response 共享底层数组，
		// 就会和本 goroutine 随后的读并发——CI Linux -race 实证的 DATA RACE。
		firstResponse <- append([]byte(nil), response...)
		// 写网络同样用独立副本：libp2p 的流写路径可能异步持有传入切片。
		_, _ = s.Write(append([]byte(nil), response...))
	})
	_, err = d.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()})
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("缺少响应证明不得报在线: %v", err)
	}
	// 即使某次合法响应被重放，随机挑战不同，签名也不匹配。
	var oldNonce [nearbyNonceBytes]byte
	if _, err := rand.Read(oldNonce[:]); err != nil {
		t.Fatal(err)
	}
	first := <-firstResponse
	binding := append(oldNonce[:], first[:len(first)-sha256.Size]...)
	oldProof := d.nearbyMAC("response-v1", a.ID(), b.ID(), binding)
	copy(first[len(first)-sha256.Size:], oldProof[:])
	b.SetStreamHandler(d.protoNearby, func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(3 * time.Second))
		var req [nearbyNonceBytes + sha256.Size]byte
		if _, err := io.ReadFull(s, req[:]); err == nil {
			_, _ = s.Write(first)
		}
	})
	// 冷却只影响重试频率，不影响认证校验；这里调整本测试节点的冷却时刻。
	d.nearbyGate.mu.Lock()
	d.nearbyGate.lastOut[b.ID()] = time.Time{}
	d.nearbyGate.mu.Unlock()
	_, err = d.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID()})
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("重放响应不得报在线: %v", err)
	}
}

func TestNearbyProbeTimeoutAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, b := testHost(t, false), testHost(t, false)
	da, err := New(ctx, a, Config{NetworkKey: "nearby-timeout"})
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	if err := da.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b.SetStreamHandler(da.protoNearby, func(s network.Stream) { defer s.Close(); <-ctx.Done() })
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	begin := time.Now()
	_, err = da.ProbeNearby(short, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()})
	if err == nil || time.Since(begin) > 2*time.Second {
		t.Fatalf("取消应快速停止: %v, duration=%v", err, time.Since(begin))
	}
	if err := da.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = da.ProbeNearby(ctx, peer.AddrInfo{ID: b.ID()})
	if err == nil || !strings.Contains(err.Error(), "已关闭") {
		t.Fatalf("关闭后应拒绝探测: %v", err)
	}
}
