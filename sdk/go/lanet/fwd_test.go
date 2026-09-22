package lanet

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func TestClientCloseStopsRunWithoutCancelingParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	webrtc := false
	c, err := New(parent, Config{
		Standalone: true, Name: "生命周期测试", Quiet: true,
		NetworkKey: "sdk-lifecycle-test", DBPath: "-", ConsoleAddr: "-",
		LanetDNSAddr: "127.0.0.1:0", ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		WebRTC: &webrtc,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close 后 Run 未及时退出")
	}
	if parent.Err() != nil {
		t.Fatal("Close 取消了调用方父上下文")
	}
	// 已关闭节点再次 Run，不应执行任何网络任务。
	done = make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("已关闭节点 Run 未立即返回")
	}
}

func TestMergeCtxCancellation(t *testing.T) {
	for _, source := range []string{"root", "caller", "stop", "closed"} {
		t.Run(source, func(t *testing.T) {
			root, cancelRoot := context.WithCancel(context.Background())
			defer cancelRoot()
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			if source == "closed" {
				cancelRoot()
			}
			c := &Client{rootCtx: root}
			merged, stop := c.mergeCtx(caller)
			defer stop()
			switch source {
			case "root":
				cancelRoot()
			case "caller":
				cancelCaller()
			case "stop":
				stop()
			case "closed":
				if merged.Err() == nil {
					t.Fatal("已取消 root 必须同步反映到 merged")
				}
			}
			select {
			case <-merged.Done():
			case <-time.After(time.Second):
				t.Fatal("合并上下文未取消")
			}
			if source != "caller" && caller.Err() != nil {
				t.Fatal("反向取消了调用方")
			}
		})
	}
}

func TestForwardParentCancellationClosesListener(t *testing.T) {
	c := newBareClient()
	defer c.Close()
	port := freePort(t)
	c.startListenForward(c.rootCtx, LANForward{Listen: port, Target: "127.0.0.1:1"})
	c.lfMu.Lock()
	fl := c.lfListeners[port]
	c.lfMu.Unlock()
	if fl == nil {
		t.Fatal("监听器未创建")
	}
	c.cancel()
	deadline := time.Now().Add(time.Second)
	for {
		fl.mu.Lock()
		closed := fl.closed
		fl.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("父上下文取消后监听器未关闭")
		}
		time.Sleep(time.Millisecond)
	}
	ln, err := net.Listen("tcp", ":"+itoa(port))
	if err != nil {
		t.Fatalf("端口未释放: %v", err)
	}
	ln.Close()
	c.Close()
	c.startListenForward(context.Background(), LANForward{Listen: port, Target: "127.0.0.1:1"})
	if len(c.lfListeners) != 0 {
		t.Fatal("Close 后重新创建了监听器")
	}
}

func TestConsoleBodyCompleteAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"正常", `{}`, http.StatusOK},
		{"恰好上限", `{}` + strings.Repeat(" ", maxConsoleBody-2), http.StatusOK},
		{"尾部超限", `{}` + strings.Repeat(" ", maxConsoleBody-1), http.StatusRequestEntityTooLarge},
		{"重复JSON", `{} {}`, http.StatusBadRequest},
		{"尾部垃圾", `{}x`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			var value map[string]any
			c := &Client{}
			c.decodeBody(w, r, &value)
			if w.Code != tc.status {
				t.Fatalf("状态码 %d，期望 %d", w.Code, tc.status)
			}
		})
	}
}

// ---- 测试辅助：最小可运行的 Client（只加载转发所需字段，避免完整 New） ----

func newBareClient() *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:         Config{Quiet: true},
		rootCtx:     ctx,
		cancel:      cancel,
		lfListeners: make(map[int]*fwdListener),
	}
	c.fwMu.Lock()
	c.forwards = nil
	c.fwMu.Unlock()
	return c
}

// tcpMarkerServer 每条连接返回固定字符串后关闭（验证「转发到哪个目标」）。
func tcpMarkerServer(marker string) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte(marker))
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// tcpLargeServer 每条连接发送 size 字节后关闭写端（半关闭语义验证不截断）。
func tcpLargeServer(size int) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				payload := make([]byte, size)
				for i := range payload {
					payload[i] = 'x'
				}
				_, _ = c.Write(payload)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// freePort 借一个空闲 TCP 端口用于转发监听（避免与系统端口冲突）。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// ---- 配额口径统一（fwdQuota） ----

func TestFwdQuotaUnified(t *testing.T) {
	c := &Client{cfg: Config{}}
	if q := c.fwdQuota(); q != fwdDefaultQuota {
		t.Errorf("LANForwardMaxConns=0 → %d, want 默认 %d", q, fwdDefaultQuota)
	}
	c = &Client{cfg: Config{LANForwardMaxConns: -5}}
	if q := c.fwdQuota(); q != fwdDefaultQuota {
		t.Errorf("LANForwardMaxConns=-5 → %d, want 默认 %d（负数等同 0=默认上限）", q, fwdDefaultQuota)
	}
	c = &Client{cfg: Config{LANForwardMaxConns: 3}}
	if q := c.fwdQuota(); q != 3 {
		t.Errorf("LANForwardMaxConns=3 → %d, want 3", q)
	}
}

// ---- 修复：proxyToTarget 在 ctx 取消后必须释放 target 连接（防泄漏） ----

func TestProxyToTargetClosesOnCtxCancel(t *testing.T) {
	// 目标保持 socket 不关（模拟「收 FIN 仍保持」的对端），验证 ctx 取消
	// 后 proxy goroutine 退出、target 连接被回收（验收矩阵 listenfwd 项）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		accepted <- conn // 保持连接，不关闭
	}()

	c := newBareClient()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()
	defer serverConn.Close()
	fl := &fwdListener{target: ln.Addr().String(), quota: 1}

	proxyDone := make(chan struct{})
	go func() {
		c.proxyToTarget(ctx, fl, serverConn)
		close(proxyDone)
	}()

	select {
	case tc := <-accepted:
		defer tc.Close()
		// 目标已连上，准备取消。
		cancel()
		// 1 秒内 target 应被 proxy 的 AfterFunc 关闭。
		done := make(chan struct{})
		go func() { var b [1]byte; _, _ = tc.Read(b[:]); close(done) }()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			_ = tc.Close()
			t.Fatal("ctx 取消后 target 连接未在 1 秒内释放（proxy goroutine 泄漏）")
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("目标未建立连接")
	}
	select {
	case <-proxyDone:
	case <-time.After(time.Second):
		t.Fatal("取消后代理 goroutine 未退出")
	}
}

// ---- 转发目标切换 / 删除 / 同配置不重建 / 大响应不截断 ----

func TestForwardTargetSwitchAndDelete(t *testing.T) {
	addrA, stopA := tcpMarkerServer("A")
	defer stopA()
	addrB, stopB := tcpMarkerServer("B")
	defer stopB()

	port := freePort(t)
	c := newBareClient()
	defer c.Close()

	dialAndRead := func() string {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
		if err != nil {
			return "DIAL_FAIL:" + err.Error()
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 8)
		n, _ := io.ReadFull(conn, buf)
		return string(buf[:n])
	}

	// 1. 新增 → 连到 A。
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addrA}}
	c.fwMu.Unlock()
	c.startListenForwards(c.rootCtx)
	if got := dialAndRead(); got != "A" {
		t.Fatalf("初始应连到 A，got %q", got)
	}

	// 2. 同配置再启动一次：监听器不应重建（指针不变）。
	c.lfMu.Lock()
	fl1 := c.lfListeners[port]
	c.lfMu.Unlock()
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addrA}}
	c.fwMu.Unlock()
	c.startListenForwards(c.rootCtx)
	c.lfMu.Lock()
	fl2 := c.lfListeners[port]
	c.lfMu.Unlock()
	if fl1 != fl2 {
		t.Fatal("同配置重复启动应复用同一监听器，却重建了")
	}
	if got := dialAndRead(); got != "A" {
		t.Fatalf("同配置重建后仍应连到 A，got %q", got)
	}

	// 3. 同端口换 target → 后续只能到 B。
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addrB}}
	c.fwMu.Unlock()
	c.syncListenForwards(c.rootCtx)
	if got := dialAndRead(); got != "B" {
		t.Fatalf("换 target 后应连到 B，got %q", got)
	}

	// 4. 删除 → 端口不再可拨（连接被拒）。
	c.fwMu.Lock()
	c.forwards = nil
	c.fwMu.Unlock()
	c.syncListenForwards(c.rootCtx)
	if got := dialAndRead(); !strings.HasPrefix(got, "DIAL_FAIL:") {
		t.Fatalf("删除后端口应不可达，got %q", got)
	}
}

// 大响应经半关闭转发不截断（验收矩阵「正常半关闭后大响应不截断」）。
func TestForwardLargeResponseNoTruncation(t *testing.T) {
	const size = 4 * 1024 * 1024 // 4MB，恰穿 default 转发路径
	addr, stop := tcpLargeServer(size)
	defer stop()

	port := freePort(t)
	c := newBareClient()
	defer c.Close()
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addr}}
	c.fwMu.Unlock()
	c.startListenForwards(c.rootCtx)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("读响应出错: %v", err)
	}
	if len(got) != size {
		t.Fatalf("响应被截断: 收到 %d 字节, 期望 %d", len(got), size)
	}
}

// ---- 配额：N 条保留、第 N+1 条被拒、释放后可新增 ----

func TestForwardQuotaRejectsExtra(t *testing.T) {
	addr, stop := tcpMarkerServer("X")
	defer stop()

	port := freePort(t)
	c := &Client{
		cfg:         Config{Quiet: true, LANForwardMaxConns: 1},
		rootCtx:     context.Background(),
		cancel:      func() {},
		lfListeners: make(map[int]*fwdListener),
	}
	defer c.Close()
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addr}}
	c.fwMu.Unlock()
	c.startListenForwards(c.rootCtx)

	// 第 1 条长连保留。
	conn1, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	_ = conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn1.Read(buf); err != nil {
		t.Fatalf("第 1 条连接应正常: %v", err)
	}

	// 第 2 条（N+1）被拒：服务器 accept 后立即关，读取应 EOF。
	conn2, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("第 2 条 TCP 握手应成功（拒绝在应用层）: %v", err)
	}
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn2.Read(buf); err == nil {
		conn2.Close()
		t.Fatal("超出配额的第 2 条连接应被立即关闭（读 EOF），却收到数据")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		conn2.Close()
		t.Fatal("超出配额的连接仅超时，未被及时关闭")
	}
	conn2.Close()

	// 释放第 1 条后，可新增。
	conn1.Close()
	// 给 accept 循环一点时间注销。
	deadline := time.Now().Add(2 * time.Second)
	for {
		nc, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
		if e == nil {
			_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, re := nc.Read(buf); re == nil {
				nc.Close()
				break // 新增成功
			}
			nc.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("释放配额后新连接仍被拒")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// accept 与 shutdown 并发、更新与 Close 并发：不应 panic/死锁（合法性校验）。
func TestForwardConcurrentAcceptShutdown(t *testing.T) {
	addr, stop := tcpMarkerServer("X")
	defer stop()
	port := freePort(t)
	c := newBareClient()
	c.fwMu.Lock()
	c.forwards = []LANForward{{Listen: port, Target: addr}}
	c.fwMu.Unlock()
	c.startListenForwards(c.rootCtx)

	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	// 持续发起连接（触发 accept 并发）。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
				if err == nil {
					_ = conn.Close()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	// 让 Close 与后半段热更新真正并发，且 Close 后不得复活监听。
	closed := make(chan struct{})
	for i := 0; i < 20; i++ {
		if i == 10 {
			go func() { c.Close(); close(closed) }()
		}
		c.fwMu.Lock()
		if i%2 == 0 {
			c.forwards = []LANForward{{Listen: port, Target: addr}}
		} else {
			c.forwards = nil
		}
		c.fwMu.Unlock()
		c.syncListenForwards(c.rootCtx)
		time.Sleep(10 * time.Millisecond)
	}
	close(stopCh)
	wg.Wait()
	<-closed
	c.lfMu.Lock()
	defer c.lfMu.Unlock()
	if len(c.lfListeners) != 0 {
		t.Fatal("并发 Close/热更新后仍有监听器")
	}
}
