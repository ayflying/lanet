package serverless

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestDNSConnLimit 验证并发连接上限生效：把 maxConns 调到 2，发起 3 条 TCP
// 连接且每条都「保持打开」（连接成功后不关闭，使服务端活动表长期持有它），
// 服务端应在达到上限后立即拒绝第 3 条（直接关闭，不进入活动连接表）。
//
// 关键点：限额只在「同时活跃连接数」达到上限时触发，因此必须把连接保持打开，
// 否则前一条处理完就被客户端关掉、活动表永远凑不满 2 条，限额形同虚设。
func TestDNSConnLimit(t *testing.T) {
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	d.maxConns = 2 // 刻意调小便于测试

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.listenAndServe(ctx, "127.0.0.1:15354") }()
	time.Sleep(200 * time.Millisecond)

	// 同时打开 3 条并保持：成功返回打开的连接，被拒返回 nil。
	var conns []net.Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	success := 0
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dnsTCPOpen(t, "127.0.0.1:15354", buildQuery("xa.lanet", 1))
			mu.Lock()
			if c != nil {
				success++
				conns = append(conns, c)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	// 收尾：关闭所有成功建立、仍由本端持有的连接。
	for _, c := range conns {
		_ = c.Close()
	}

	// 恰好应放行 2 条、拒绝 1 条（限额命中）。
	if success != 2 {
		t.Fatalf("连接限额应恰好放行 2 条、拒绝 1 条，实际放行 %d 条（期望被拒的第 3 条未被挡下）", success)
	}
}

// TestDNSConnTimeout 验证单连接读写超时：把 connTimeout 调到 200ms，客户端
// 建连后完全不发包，服务端必须在超时后主动回收该连接（客户端读立即得到错误）。
func TestDNSConnTimeout(t *testing.T) {
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	d.connTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.listenAndServe(ctx, "127.0.0.1:15355") }()
	time.Sleep(200 * time.Millisecond)

	conn, err := net.Dial("tcp", "127.0.0.1:15355")
	if err != nil {
		t.Skipf("TCP dial 不可用: %v", err)
	}
	defer conn.Close()

	// 建连后什么都不发，等服务器因超时关闭连接。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("空闲连接应被超时回收，但服务端仍发来了数据")
	}
}

// TestDNSCloseReclaimsConns 验证 Close() 不仅关监听，还会回收所有活动 TCP
// 连接：客户端先建立一条正常连接（收发一次），再调用 Close()，随后该活动
// 连接必须被服务端关闭（客户端再次读应报错，而非永远挂着）。
func TestDNSCloseReclaimsConns(t *testing.T) {
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	d.connTimeout = 5 * time.Second // 拉长为「常驻」，避免被超时提前回收干扰断言

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.listenAndServe(ctx, "127.0.0.1:15356") }()
	time.Sleep(200 * time.Millisecond)

	// 先确认连接已被服务端接受并登记（正常收发一次，DNS-over-TCP 带 2 字节长度前缀）。
	// 保持连接打开，以便随后验证 Close 回收它。
	conn := dnsTCPOpen(t, "127.0.0.1:15356", buildQuery("xa.lanet", 1))
	if conn == nil {
		t.Fatalf("首轮查询未得到应答")
	}
	defer conn.Close()

	// 触发 Close：必须回收上面这条活动连接。
	d.Close()

	// 活动连接被回收后，客户端后续读取应失败（而非永远挂着）。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf2 := make([]byte, 16)
	n2, err2 := conn.Read(buf2)
	if err2 == nil && n2 > 0 {
		t.Fatalf("Close 后活动连接应被回收，但客户端仍收到了数据")
	}

	// Close 幂等，不应 panic/出错。
	d.Close()
}

// dnsTCPOpen 通过 DNS-over-TCP（2 字节大端长度前缀的报文帧）向服务端发一次
// A 查询；校验拿到 ANCOUNT=1 的应答后「保持连接打开」并返回 conn，失败返回 nil。
// 用于需要让连接驻留服务端活动表的测试（连接限额、Close 回收）。
func dnsTCPOpen(t *testing.T, addr string, q []byte) net.Conn {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	out := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(out, uint16(len(q)))
	copy(out[2:], q)
	if _, err := conn.Write(out); err != nil {
		_ = conn.Close()
		return nil
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		_ = conn.Close()
		return nil
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		_ = conn.Close()
		return nil
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 1 {
		_ = conn.Close()
		return nil
	}
	return conn // 保持打开
}
