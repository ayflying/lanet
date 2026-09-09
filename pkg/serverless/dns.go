// 内置 DNS 应答器：把 .lanet 虚拟域名暴露给操作系统 DNS。
//
// 组网后用户期望 `ping yunloli.lanet` 直接可用，但 SDK 内部的
// ResolveTarget 只服务编程接口，操作系统不认识 .lanet 后缀。本文件在
// 本机回环地址（127.0.0.1:53）提供最小 DNS 服务，把 <成员名>.lanet 的
// A 查询按成员表实时应答——成员重启换 IP 时解析自动跟随，无需 hosts。
//
// 操作系统侧由宿主程序负责把 .lanet 后缀路由到本服务：
//   - Windows：NRPT 规则（Add-DnsClientNrptRule，落 HKLM 策略注册表）；
//   - macOS：/etc/resolver/lanet；
//   - Linux：glibc/nss 不识别 /etc/resolver，需要改 /etc/resolv.conf
//     （侵入性强，官方程序不自动改，SDK 只启动服务供手动接入）。
//
// 协议范围：只应答 IN 类 A 查询（虚拟 IPv4）；其余类型/类别一律 NOTIMP；
// 不认识的域名 NXDOMAIN。TTL=0：成员表实时变化，禁止中间层缓存。
package serverless

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
)

// dnsListenAddr 内置 DNS 服务默认监听地址（本机回环，仅本机解析器使用）。
const dnsListenAddr = "127.0.0.1:53"

// MembersFunc 返回当前成员表快照（每次查询实时调用，解析天然跟随成员变化）。
type MembersFunc func() []MemberRef

// DNSServer .lanet 域名 DNS 应答器。
type DNSServer struct {
	mu      sync.Mutex
	members MembersFunc
	udp     *net.UDPConn
	tcp     net.Listener
	closed  bool
}

// NewDNSServer 创建应答器；members 在每次查询时实时调用。
func NewDNSServer(members MembersFunc) *DNSServer {
	return &DNSServer{members: members}
}

// ListenAndServe 绑定 UDP/53（TCP/53 尽力而为）并阻塞服务，直到 ctx 取消。
// 绑定 53 端口需要特权（Windows 提权运行 / Linux root）；失败返回错误，
// 由调用方决定降级策略——本服务不可用不影响组网与虚拟 IP 直连。
func (d *DNSServer) ListenAndServe(ctx context.Context) error {
	return d.listenAndServe(ctx, dnsListenAddr)
}

// ListenAndServeAddr 同 ListenAndServe，但监听指定地址（SDK 覆盖默认端口用）。
func (d *DNSServer) ListenAndServeAddr(ctx context.Context, addr string) error {
	return d.listenAndServe(ctx, addr)
}

func (d *DNSServer) listenAndServe(ctx context.Context, addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("lanet: DNS 地址解析失败: %w", err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("lanet: DNS 监听 %s 失败（需要管理员/root 权限）: %w", addr, err)
	}
	d.mu.Lock()
	d.udp = udp
	closed := d.closed
	d.mu.Unlock()
	if closed {
		_ = udp.Close()
		return nil
	}

	// TCP 可选：失败不影响 UDP 主通道（解析器 UDP 优先，响应超 512 字节
	// 才需要 TCP，本服务应答极小，TCP 只是协议完备性兜底）。
	if tcp, err := net.Listen("tcp", addr); err == nil {
		d.mu.Lock()
		d.tcp = tcp
		d.mu.Unlock()
		go func() {
			for {
				conn, err := tcp.Accept()
				if err != nil {
					return
				}
				go d.serveTCP(conn)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		d.Close()
	}()

	buf := make([]byte, 1500)
	for {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			if d.isClosed() {
				return nil
			}
			return fmt.Errorf("lanet: DNS 读取失败: %w", err)
		}
		resp := d.handleQuery(buf[:n])
		if resp == nil {
			continue // 畸形包：静默丢弃
		}
		if _, err := udp.WriteToUDP(resp, from); err != nil && d.isClosed() {
			return nil
		}
	}
}

// Close 停止应答并释放端口（幂等）。
func (d *DNSServer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	if d.udp != nil {
		_ = d.udp.Close()
	}
	if d.tcp != nil {
		_ = d.tcp.Close()
	}
}

func (d *DNSServer) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// serveTCP 处理单条 DNS-over-TCP 连接（2 字节大端长度前缀的报文流）。
func (d *DNSServer) serveTCP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var lenBuf [2]byte
	for {
		if _, err := ioReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint16(lenBuf[:])
		if n == 0 {
			return
		}
		msg := make([]byte, n)
		if _, err := ioReadFull(conn, msg); err != nil {
			return
		}
		resp := d.handleQuery(msg)
		if resp == nil {
			return
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

// ioReadFull 读满 buf 或返回错误。
func ioReadFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// DNS RCODE 常量（低 4 位）。
const (
	dnsRCODEFormatError uint16 = 1
	dnsRCODENXDomain    uint16 = 3
	dnsRCODENotImp      uint16 = 4
)

// handleQuery 解析查询并产出应答报文；畸形包返回 nil（静默丢弃）。
func (d *DNSServer) handleQuery(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	if binary.BigEndian.Uint16(query[4:6]) != 1 { // 只支持单问题段
		return d.errorResponse(query, dnsRCODEFormatError)
	}

	name, off, ok := parseDNSName(query, 12)
	if !ok {
		return nil
	}
	if off+4 > len(query) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(query[off : off+2])
	qclass := binary.BigEndian.Uint16(query[off+2 : off+4])
	questionEnd := off + 4

	// 应答头：复制事务 ID；QR=1（应答）AA=1（权威）。
	resp := make([]byte, 12)
	copy(resp, query[:2])
	binary.BigEndian.PutUint16(resp[2:4], 0x8400)
	binary.BigEndian.PutUint16(resp[4:6], 1) // QDCOUNT=1
	// ANCOUNT/NSCOUNT/ARCOUNT 留 0，命中时再填。

	// 问题段原样回显。
	resp = append(resp, query[12:questionEnd]...)

	// 非 IN 类或非 A 查询：NOTIMP（本服务只做 .lanet 的 A 应答）。
	if qclass != 1 || qtype != 1 {
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENotImp)
		return resp
	}

	ip := d.resolve(name)
	if ip == "" {
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENXDomain)
		return resp
	}

	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENXDomain)
		return resp
	}

	// A 应答：NAME 用压缩指针 0xC00C 指向问题段、Type=1、Class=IN、
	// TTL=0（成员表实时变化，禁止缓存）、RDLENGTH=4。
	ans := make([]byte, 16)
	binary.BigEndian.PutUint16(ans[0:2], 0xC00C)
	binary.BigEndian.PutUint16(ans[2:4], 1)   // Type A
	binary.BigEndian.PutUint16(ans[4:6], 1)   // Class IN
	binary.BigEndian.PutUint32(ans[6:10], 0)  // TTL=0
	binary.BigEndian.PutUint16(ans[10:12], 4) // RDLENGTH=4
	copy(ans[12:16], parsed)
	resp = append(resp, ans...)
	binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT=1
	return resp
}

// resolve 把查询名解析为虚拟 IP；不属于本网络的名字返回空串。
// 只认 <label>.lanet / 短名（label）/ 虚拟 IP 三种形态，裸域名
// （如 example.com）一律拒绝——NRPT 只路由 .lanet，但 macOS
// /etc/resolver 也可能只挂了子域，防御面保持一致。
func (d *DNSServer) resolve(name string) string {
	if d.members == nil {
		return ""
	}
	members := d.members()
	if len(members) == 0 {
		return ""
	}
	trimmed := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if trimmed == "" {
		return ""
	}
	// 明确排除：既非 .lanet 后缀、也非纯短名/虚拟 IP 的查询
	// （短名与虚拟 IP 是 ResolveTarget 的合法输入，保留支持）。
	if !strings.HasSuffix(trimmed, "."+VirtualDomain) && strings.Contains(trimmed, ".") {
		return ""
	}
	m, err := ResolveTarget(members, name)
	if err != nil {
		return ""
	}
	return m.VirtualIP
}

// errorResponse 构造仅头部的错误应答（无问题段/应答段）。
func (d *DNSServer) errorResponse(query []byte, rcode uint16) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, 12)
	copy(resp, query[:2])
	binary.BigEndian.PutUint16(resp[2:4], 0x8400|rcode)
	return resp
}

// parseDNSName 解析 DNS 报文域名（标签长度前缀格式），返回小写名字与下一偏移。
// 查询段不应出现压缩指针，遇到即拒绝（防回环/畸形包）。
func parseDNSName(msg []byte, off int) (string, int, bool) {
	var sb strings.Builder
	labels := 0
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		if l == 0 {
			off++
			return sb.String(), off, true
		}
		if l&0xC0 == 0xC0 || l > 63 || off+1+l > len(msg) {
			return "", 0, false
		}
		label := string(msg[off+1 : off+1+l])
		if !validDNSLabelBytes(label) {
			return "", 0, false
		}
		if sb.Len() > 0 {
			sb.WriteByte('.')
		}
		sb.WriteString(label)
		off += 1 + l
		labels++
		if labels > 16 {
			return "", 0, false
		}
	}
}

// validDNSLabelBytes 校验标签字符（宽松：字母数字连字符下划线）。
func validDNSLabelBytes(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
