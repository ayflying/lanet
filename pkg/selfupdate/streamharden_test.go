package selfupdate

// 入向 handler 与出向请求的「健壮性」测试：聚焦审计里尚未闭环的两类风险——
//   - 入向 handler 此前无 JSON 尺寸限额，恶意对端可借超大请求把协程/内存撑爆；
//   - 出向 newStream 在 ctx 取消后未绑定 Reset，传输中途取消会留下半关流、对端
//     阻塞在 io.Copy 上（审计 item：出向建流后未绑定 ctx 取消）。
//
// 覆盖：大包（入向请求体/出向 head 长度伪造）、慢读（入向读写截止）、取消（出向
// ctx 取消即时 Reset）。限流本身由 protoharden_test.go 守。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

type updatePipeStream struct {
	network.Stream
	conn net.Conn
}

func (s *updatePipeStream) Read(p []byte) (int, error)         { return s.conn.Read(p) }
func (s *updatePipeStream) Write(p []byte) (int, error)        { return s.conn.Write(p) }
func (s *updatePipeStream) Reset() error                       { return s.conn.Close() }
func (s *updatePipeStream) SetReadDeadline(d time.Time) error  { return s.conn.SetReadDeadline(d) }
func (s *updatePipeStream) SetWriteDeadline(d time.Time) error { return s.conn.SetWriteDeadline(d) }

func TestUpdateStreamTimeoutAndCancel(t *testing.T) {
	for _, mode := range []string{"读超时", "写超时", "取消", "正常清理"} {
		t.Run(mode, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &Coordinator{cfg: Config{StreamIOTimeout: 100 * time.Millisecond}}
			if mode == "取消" || mode == "正常清理" {
				c.cfg.StreamIOTimeout = time.Hour
			}
			s := &updatePipeStream{conn: a}
			cleanup := c.bindUpdateStream(ctx, s)
			defer cleanup()
			done := make(chan error, 1)
			go func() {
				var err error
				if mode == "写超时" {
					_, err = s.Write([]byte("x"))
				} else {
					_, err = s.Read(make([]byte, 1))
				}
				done <- err
			}()
			if mode == "取消" {
				cancel()
			}
			if mode == "正常清理" {
				cleanup()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("阻塞 I/O 应返回错误")
				}
			case <-time.After(time.Second):
				t.Fatal("流未及时释放")
			}
		})
	}
}

func TestUpdateJSONBounds(t *testing.T) {
	const limit = 64
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"边界", `{}` + strings.Repeat(" ", limit-2), true},
		{"超限空白尾部", `{}` + strings.Repeat(" ", limit-1), false},
		{"第二个对象", `{} {}`, false},
		{"截断", `{"current":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value map[string]any
			err := readUpdateJSON(strings.NewReader(tc.body), limit, &value)
			if (err == nil) != tc.ok {
				t.Fatalf("解析结果 %v，预期成功=%v", err, tc.ok)
			}
		})
	}
}

func TestUpdateJSONRequiresRealEOF(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go func() {
		var value map[string]any
		done <- readUpdateJSON(r, 2, &value)
	}()
	if _, err := w.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("真实 EOF 前不应成功: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF 后读取未结束")
	}
}

// TestInboundManifestRejectsOversizedJSON 入向清单 handler：超过 maxRequestJSON
// 的请求体必须被丢弃（不返回清单、不解完整 JSON 入内存）。
func TestInboundManifestRejectsOversizedJSON(t *testing.T) {
	dir := t.TempDir()
	cProv, _, _ := makeProvider(t, dir, nil, 0) // 固定 ID 提供方，持有有效 head
	hReq := newTestHost(t)
	if err := hReq.Connect(context.Background(), peer.AddrInfo{ID: cProv.host.ID(), Addrs: cProv.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := hReq.NewStream(ctx, cProv.host.ID(), ProtocolManifest)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 远超 64KiB 上限的 JSON 请求体。
	body := fmt.Sprintf(`{"current":"0.1.0","pad":"%s"}`, strings.Repeat("x", 256*1024))
	// 对端可在写入尚未结束时拒绝超限请求，发送错误本身也是合法拒绝。
	_ = s.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = s.Write([]byte(body))
	_ = s.CloseWrite()
	var m Manifest
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	err = json.NewDecoder(s).Decode(&m)
	if err == nil {
		t.Fatal("超大请求不应返回清单")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("超大请求未及时关闭，不能将读超时视为拒绝成功")
	}
}

// TestInboundFileRejectsOversizedJSON 入向文件 handler：超大请求体同样必须被丢弃。
func TestInboundFileRejectsOversizedJSON(t *testing.T) {
	dir := t.TempDir()
	cProv, _, _ := makeProvider(t, dir, nil, 0)
	hReq := newTestHost(t)
	if err := hReq.Connect(context.Background(), peer.AddrInfo{ID: cProv.host.ID(), Addrs: cProv.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := hReq.NewStream(ctx, cProv.host.ID(), ProtocolFile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body := fmt.Sprintf(`{"sha256":"%s","pad":"%s"}`, strings.Repeat("a", 40), strings.Repeat("x", 256*1024))
	// 对端可在写入尚未结束时拒绝超限请求，发送错误本身也是合法拒绝。
	_ = s.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = s.Write([]byte(body))
	_ = s.CloseWrite()
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	if n, err := s.Read(buf); n != 0 || err == nil {
		t.Fatal("超大文件请求不应收到数据")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("超大文件请求未及时关闭")
	}
}

// TestInboundHandlerDropsSlowReader 入向读写截止：对端建流后只发半个请求、永不
// CloseWrite，handler 的读截止（StreamIOTimeout）必须把它踢掉——否则慢连接会
// 一直占着协程（审计 item 8：handler 此前无 deadline，慢连接挂死）。
func TestInboundHandlerDropsSlowReader(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	bin := make([]byte, 64*1024)
	_, _ = rand.Read(bin)
	sum := sha256.Sum256(bin)
	shaHex := hex.EncodeToString(sum[:])
	srcExe := filepath.Join(dir, "lanet-new.exe")
	if err := os.WriteFile(srcExe, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "update-manifest.json")
	mm := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: int64(len(bin)), SHA256: shaHex}
	if err := SignManifest(priv, &mm); err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(mp, mm); err != nil {
		t.Fatal(err)
	}
	hProv := newTestHost(t)
	cProv := New(hProv, staticPeers{}, Config{
		CurrentVersion: "9.9.9", Platform: "windows/amd64", ExePath: srcExe,
		ManifestPath: mp, PublicKey: pubB64, CheckInterval: time.Hour, Quiet: true,
		StreamIOTimeout: 200 * time.Millisecond, // 把截止压到极小，凸显慢读被踢
	}, nil)
	if _, ok := cProv.SelfManifest(); !ok {
		t.Fatal("提供方 head 加载失败")
	}

	hReq := newTestHost(t)
	if err := hReq.Connect(context.Background(), peer.AddrInfo{ID: hProv.ID(), Addrs: hProv.Addrs()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := hReq.NewStream(ctx, hProv.ID(), ProtocolManifest)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 只写半个请求，且绝不 CloseWrite：对端应在读截止前一直等，触发截止后丢弃。
	if _, err = s.Write([]byte(`{"cur`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // 远超 200ms 截止
	var m Manifest
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err = json.NewDecoder(s).Decode(&m); err == nil {
		t.Fatal("半个请求不应返回清单")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("提供方未主动关闭超时流，客户端读超时不是通过证据")
	}
}

// TestOutboundRejectsOversizedHead 出向文件下载：head 长度字段可被伪造，必须被
// maxHeadJSON 这道硬上限拦下——否则 make([]byte, 4GB) 直接 OOM。
func TestOutboundRejectsOversizedHead(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	m := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: 1024, SHA256: strings.Repeat("a", 64)}
	if err := SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}
	hSrv := newTestHost(t)
	hSrv.SetStreamHandler(ProtocolFile, func(s network.Stream) {
		defer s.Close()
		// 伪造超大 head 长度（0xFFFFFFFF），请求方必须拒绝而非分配巨内存。
		_, _ = s.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	})
	dir := t.TempDir()
	cReq := newRequester(t, dir, nil, pubB64)
	if err := cReq.host.Connect(context.Background(), peer.AddrInfo{ID: hSrv.ID(), Addrs: hSrv.Addrs()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dest := filepath.Join(dir, "dl-head.tmp")
	start := time.Now()
	err := cReq.requestFile(ctx, hSrv.ID().String(), m, dest)
	if err == nil {
		t.Fatal("伪造超大 head 长度应被拒绝")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("拒绝超大 head 过慢（疑似真分配了巨内存）: %v", time.Since(start))
	}
}

// TestOutboundStreamResetOnCancel 出向 ctx 取消必须绑定 Reset：文件分发进行到一半
// 被取消时，请求方应立即 Reset 半关流、io.Copy 立即报错返回，而不是卡到对端睡醒
// 才结束（审计 item：出向建流后未绑定 ctx 取消 → 半关流泄漏/卡死）。
func TestOutboundStreamResetOnCancel(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	// 目标清单：请求方据此校验 head；head 必须匹配且验签通过才会进入文件体拷贝。
	m := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: 1024, SHA256: strings.Repeat("b", 64)}
	if err := SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}

	// 保持写方向打开，确保请求方确实阻塞在文件体而不是提前收到 EOF。
	headSent := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	hSrv := newTestHost(t)
	hSrv.SetStreamHandler(ProtocolFile, func(s network.Stream) {
		defer s.Close()
		headJSON, _ := json.Marshal(m)
		head := make([]byte, 4+len(headJSON))
		binary.BigEndian.PutUint32(head, uint32(len(headJSON)))
		copy(head[4:], headJSON)
		if _, err := s.Write(head); err != nil {
			return
		}
		close(headSent)
		<-release
	})

	dir := t.TempDir()
	cReq := newRequester(t, dir, nil, pubB64)
	if err := cReq.host.Connect(context.Background(), peer.AddrInfo{ID: hSrv.ID(), Addrs: hSrv.Addrs()}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dest := filepath.Join(dir, "dl-cancel.tmp")
	done := make(chan error, 1)
	go func() { done <- cReq.requestFile(ctx, hSrv.ID().String(), m, dest) }()
	select {
	case <-headSent:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到文件头")
	}
	select {
	case err := <-done:
		t.Fatalf("取消前请求提前退出: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消未打断阻塞读取")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("失败下载残留临时文件: %v", err)
	}
}
