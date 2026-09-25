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
// 协议范围：应答 IN 类 A（虚拟 IPv4）和 AAAA（虚拟 IPv6）；其余类型/类别
// 一律 NOTIMP；未解析或对应地址无效时 NXDOMAIN。TTL=0：成员表实时变化，禁止中间层缓存。
package serverless

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// dnsListenAddr 内置 DNS 服务默认监听地址（本机回环，仅本机解析器使用）。
const dnsListenAddr = "127.0.0.1:53"

// DNS 服务资源边界：默认并发连接上限与单连接读写超时。
//   - maxDNSConns：单实例可同时服务的 TCP 连接数上限（UDP 无连接不计入）。
//     超出时新连接被直接拒绝，防止恶意/故障解析器把连接数打满拖垮宿主进程。
//   - dnsConnTimeout：每条 TCP 连接的读写截止时间，每处理完一个报文就刷新。
//     对端只连不发包（或发包不收应答）时连接会在超时后自动回收，避免永久挂着。
const (
	dnsMaxConns    = 256
	dnsConnTimeout = 5 * time.Second
	// dnsMaxMsgSize TCP DNS 报文长度上限（2 字节大端长度前缀可达 65535）。
	dnsMaxMsgSize = 65535
)

// MembersFunc 返回当前成员表快照（每次查询实时调用，解析天然跟随成员变化）。
type MembersFunc func() []MemberRef

// DNSServer .lanet 域名 DNS 应答器。
type DNSServer struct {
	mu          sync.Mutex
	members     MembersFunc
	udp         *net.UDPConn
	tcp         net.Listener
	conns       map[net.Conn]struct{} // 活动 TCP 连接（Close 时全量回收）
	closed      bool
	maxConns    int           // 并发连接上限（默认 dnsMaxConns，测试可改小）
	connTimeout time.Duration // 单连接读写超时（默认 dnsConnTimeout）
}

// NewDNSServer 创建应答器；members 在每次查询时实时调用。
func NewDNSServer(members MembersFunc) *DNSServer {
	return &DNSServer{
		members:     members,
		conns:       make(map[net.Conn]struct{}),
		maxConns:    dnsMaxConns,
		connTimeout: dnsConnTimeout,
	}
}

// trackConn 在 accept 后登记一条新连接；若已关闭或已达并发上限则拒绝
// （返回 false，由调用方直接关闭连接）。必须在 d.mu 下调用。
func (d *DNSServer) trackConn(conn net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || len(d.conns) >= d.maxConns {
		return false
	}
	d.conns[conn] = struct{}{}
	return true
}

// untrackConn 连接结束时从活动集合移除。必须在 d.mu 下调用。
func (d *DNSServer) untrackConn(conn net.Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.conns, conn)
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
		// 两种常见成因合并提示：低端口（53）需要特权，以及已有实例占着端口。
		// 早期只写「需要管理员权限」，让「端口被另一个 lanet 实例占用」这种
		// 真实原因被误导成权限问题（双实例共存时实测踩到）。
		return fmt.Errorf("lanet: DNS 监听 %s 失败（原因可能是需要管理员/root 权限，或该端口已被占用——例如已有一个 lanet 实例在运行）: %w", addr, err)
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
		if !d.publishTCP(tcp) {
			return nil
		}
		go func() {
			for {
				conn, err := tcp.Accept()
				if err != nil {
					return
				}
				// 并发上限/已关闭：直接拒连接（不等处理），由调用方关闭。
				if !d.trackConn(conn) {
					_ = conn.Close()
					continue
				}
				go d.serveTCP(conn)
			}
		}()
	}

	// 服务主动关闭或读取失败时也解除取消回调，不等待父 context 结束。
	stop := context.AfterFunc(ctx, d.Close)
	defer stop()
	defer d.Close()

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

// publishTCP 与 Close 在同一把锁内决定监听器归属，关闭期间创建的监听立即释放。
func (d *DNSServer) publishTCP(tcp net.Listener) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		_ = tcp.Close()
		return false
	}
	d.tcp = tcp
	return true
}

// Close 停止应答并释放端口（幂等）。
// 除关闭 UDP/TCP 监听外，还会回收当前全部活动 TCP 连接——否则仅关监听会让
// 已建立的连接悬空：对端不停它们的连接，本端 goroutine 与 fd 长期残留。
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
	for c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

func (d *DNSServer) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// serveTCP 处理单条 DNS-over-TCP 连接（2 字节大端长度前缀的报文流）。
// 每条连接受 connTimeout 约束：读/写都带截止时间，每处理完一个报文刷新，
// 对端挂起不发包时连接会在超时后回收。连接结束自动从活动集合移除。
func (d *DNSServer) serveTCP(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		d.untrackConn(conn)
	}()
	_ = conn.SetDeadline(time.Now().Add(d.connTimeout))
	var lenBuf [2]byte
	for {
		if _, err := ioReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint16(lenBuf[:])
		if n == 0 {
			return
		}
		if n > dnsMaxMsgSize {
			return // 超出 DNS over TCP 报文上限，视为畸形，终止连接
		}
		msg := make([]byte, n)
		if _, err := ioReadFull(conn, msg); err != nil {
			return
		}
		resp := d.handleQuery(msg)
		if resp == nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(d.connTimeout))
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
	if len(query) < 12 || len(query) > dnsMaxMsgSize {
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

	// 非 IN 类或非 A/AAAA 查询：NOTIMP。
	if qclass != 1 || qtype != 1 && qtype != 28 {
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENotImp)
		return resp
	}

	member, ok := d.resolve(name)
	if !ok {
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENXDomain)
		return resp
	}

	var parsed net.IP
	if qtype == 1 {
		parsed = net.ParseIP(member.VirtualIP).To4()
	} else {
		parsed = net.ParseIP(member.VirtualIPv6).To16()
		if parsed != nil && (parsed.To4() != nil || parsed[0] != 0xfd || parsed[1] != 0x00 || parsed[2] != 0x6c || parsed[3] != 0x61 || parsed[4] != 0x6e || parsed[5] != 0x65) {
			parsed = nil
		}
	}
	if parsed == nil {
		if qtype == 28 {
			return resp // 名字存在但没有有效 AAAA 记录：NOERROR + ANCOUNT=0（NODATA）。
		}
		binary.BigEndian.PutUint16(resp[2:4], 0x8400|dnsRCODENXDomain)
		return resp
	}

	// A/AAAA 应答：压缩域名指针、IN 类、TTL=0；RDATA 长度分别为 4/16。
	ans := make([]byte, 12+len(parsed))
	binary.BigEndian.PutUint16(ans[0:2], 0xC00C)
	binary.BigEndian.PutUint16(ans[2:4], qtype)
	binary.BigEndian.PutUint16(ans[4:6], 1)
	binary.BigEndian.PutUint32(ans[6:10], 0)
	binary.BigEndian.PutUint16(ans[10:12], uint16(len(parsed)))
	copy(ans[12:], parsed)
	resp = append(resp, ans...)
	binary.BigEndian.PutUint16(resp[6:8], 1)
	return resp
}

// resolve 把查询名解析为成员；不属于本网络的名字返回 false。
// 只认 <label>.lanet / 短名（label）/ 虚拟 IP 三种形态，裸域名
// （如 example.com）一律拒绝——NRPT 只路由 .lanet，但 macOS
// /etc/resolver 也可能只挂了子域，防御面保持一致。
func (d *DNSServer) resolve(name string) (MemberRef, bool) {
	if d.members == nil {
		return MemberRef{}, false
	}
	members := d.members()
	if len(members) == 0 {
		return MemberRef{}, false
	}
	trimmed := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if trimmed == "" {
		return MemberRef{}, false
	}
	// 明确排除：既非 .lanet 后缀、也非纯短名/虚拟 IP 的查询
	// （短名与虚拟 IP 是 ResolveTarget 的合法输入，保留支持）。
	if !strings.HasSuffix(trimmed, "."+VirtualDomain) && strings.Contains(trimmed, ".") {
		return MemberRef{}, false
	}
	m, err := ResolveTarget(members, name)
	if err != nil {
		return MemberRef{}, false
	}
	return m, true
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
