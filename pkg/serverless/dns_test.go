package serverless

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseDNSName(t *testing.T) {
	// "xa.lanet" -> 标签长度前缀格式
	msg := []byte{2, 'x', 'a', 5, 'l', 'a', 'n', 'e', 't', 0, 0, 1, 0, 1}
	name, off, ok := parseDNSName(msg, 0)
	if !ok || name != "xa.lanet" || off != 10 {
		t.Fatalf("parseDNSName = %q, off=%d, ok=%v", name, off, ok)
	}
	// 压缩指针应被拒绝
	if _, _, ok := parseDNSName([]byte{0xC0, 0x0C}, 0); ok {
		t.Fatal("压缩指针应被拒绝")
	}
}

func buildQuery(name string, qtype uint16) []byte {
	var q []byte
	q = append(q, 0x12, 0x34) // ID
	q = append(q, 0x01, 0x00) // RD=1
	// QDCOUNT=1, ANCOUNT=0, NSCOUNT=0, ARCOUNT=0（头共 12 字节）
	q = append(q, 0x00, 0x01, 0, 0, 0, 0, 0, 0)
	for _, label := range strings.Split(name, ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0)
	q = append(q, byte(qtype>>8), byte(qtype), 0, 1) // qtype, IN
	return q
}

func TestHandleQueryARecord(t *testing.T) {
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	resp := d.handleQuery(buildQuery("xa.lanet", 1))
	if resp == nil {
		t.Fatal("应答不应为空")
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x8000 == 0 {
		t.Fatal("QR 位应置 1")
	}
	if flags&0xF != 0 {
		t.Fatalf("应答 RCODE 应为 0, got %d", flags&0xF)
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatal("ANCOUNT 应为 1")
	}
	// A 记录的 IP 在报文末 4 字节
	ip := resp[len(resp)-4:]
	if ip[0] != 10 || ip[1] != 7 || ip[2] != 89 || ip[3] != 31 {
		t.Fatalf("解析 IP 错误: %v", ip)
	}
	// TTL 应为 0（禁止缓存）
	ttl := binary.BigEndian.Uint32(resp[len(resp)-10 : len(resp)-6])
	if ttl != 0 {
		t.Fatalf("TTL 应为 0, got %d", ttl)
	}
}

func TestHandleQueryCases(t *testing.T) {
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	// 未知成员 NXDOMAIN
	if r := d.handleQuery(buildQuery("nobody.lanet", 1)); r == nil || uint16(r[3]&0xF) != dnsRCODENXDomain {
		t.Fatal("未知成员应 NXDOMAIN")
	}
	// 非成员域名的裸域名拒绝（NRPT 不会路由到这，防御面）
	if r := d.handleQuery(buildQuery("example.com", 1)); r == nil || uint16(r[3]&0xF) != dnsRCODENXDomain {
		t.Fatal("裸域名应 NXDOMAIN")
	}
	// AAAA 查询 NOTIMP
	if r := d.handleQuery(buildQuery("xa.lanet", 28)); r == nil || uint16(r[3]&0xF) != dnsRCODENotImp {
		t.Fatal("AAAA 应 NOTIMP")
	}
	// 畸形包丢弃
	if r := d.handleQuery([]byte{1, 2, 3}); r != nil {
		t.Fatal("畸形包应返回 nil")
	}
	// 短名（无后缀）也可解析
	if r := d.handleQuery(buildQuery("xa", 1)); r == nil || r[3]&0xF != 0 {
		t.Fatal("短名应可解析")
	}
}

func TestDNSServerUDPIntegration(t *testing.T) {
	// 用高端口验证真实 UDP 收发（53 需特权，集成环境起不了）
	d := NewDNSServer(func() []MemberRef {
		return []MemberRef{{PeerID: "p1", Name: "xa", VirtualIP: "10.7.89.31"}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.listenAndServe(ctx, "127.0.0.1:15353") }()
	time.Sleep(200 * time.Millisecond)

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15353})
	if err != nil {
		t.Skipf("UDP socket 不可用: %v", err)
	}
	defer conn.Close()
	q := buildQuery("xa.lanet", 1)
	if _, err := conn.Write(q); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	buf := make([]byte, 1500)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读应答失败: %v", err)
	}
	resp := buf[:n]
	if binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatalf("ANCOUNT 应为 1, resp=%v", resp)
	}
	ip := resp[len(resp)-4:]
	if ip[0] != 10 || ip[1] != 7 || ip[2] != 89 || ip[3] != 31 {
		t.Fatalf("IP 错误: %v", ip)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("listenAndServe 退出错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listenAndServe 未随 ctx 退出")
	}
}
