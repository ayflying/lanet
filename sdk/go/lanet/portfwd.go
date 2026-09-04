package lanet

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/network"
)

// PortFWDTarget 端口转发目标。
type PortFWDTarget struct {
	// VirtualIP 目标节点虚拟 IP。
	VirtualIP string
	// Port 目标节点侧的 TCP 端口。
	Port int
}

// DialPortFWD 按虚拟 IP + 端口连接对端的 TCP 服务：
// 先与对端建立 PortFWD 协议流，由对端 net.Dial 本地/内网目标，
// 之后本连接等价于一条到目标端口的双向字节管道。
//
// 返回的 net.Conn 用完必须 Close；发送完毕可调用 CloseWrite
// 半关闭（ConnImpl 同时实现了 CloseWriter 接口）。
func (c *Client) DialPortFWD(ctx context.Context, target PortFWDTarget) (net.Conn, error) {
	if target.Port <= 0 || target.Port > 65535 {
		return nil, fmt.Errorf("lanet: 无效端口 %d", target.Port)
	}
	raw, viaRelay, err := c.tunnelSvc.OpenStreamToVirtualIPProtocol(ctx, target.VirtualIP, protocol.PortFWD)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", target.VirtualIP, target.Port)
	if err = writePortFWDHeader(raw, addr); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("lanet: 发送转发目标: %w", err)
	}
	return &PortFWDConn{
		streamAdapter: streamAdapter{Stream: raw, viaRelay: viaRelay},
		remote:        addr,
	}, nil
}

// enablePortFWD 注册端口转发入向处理器（默认开启）：
// 对端发来 PortFWD 流时，向 header 中的地址发起 TCP 连接并双向搬运。
func (c *Client) enablePortFWD() {
	c.node.SetStreamHandler(protocol.PortFWD, func(stream network.Stream) {
		addr, err := readPortFWDHeader(stream)
		if err != nil {
			c.logf("portfwd 读取目标地址失败: %v", err)
			_ = stream.Reset()
			return
		}
		// 目标为本节点虚拟 IP 时映射到 loopback：
		// SDK 节点无 TUN，虚拟 IP 在本机 OS 上不可路由。
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil && host == c.myIP {
			if _, port, p2 := net.SplitHostPort(addr); p2 == nil {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
		}
		var dialer net.Dialer
		dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		target, err := dialer.DialContext(dialCtx, "tcp", addr)
		if err != nil {
			c.logf("portfwd 连接 %s 失败: %v", addr, err)
			_ = stream.Reset()
			return
		}
		c.logf("portfwd: %s 经本节点转发", addr)
		pipeBoth(streamAdapter{Stream: stream}, target)
	})
}

// writePortFWDHeader 写入 2 字节大端长度 + 目标地址。
func writePortFWDHeader(w io.Writer, addr string) error {
	head := make([]byte, 2+len(addr))
	binary.BigEndian.PutUint16(head, uint16(len(addr)))
	copy(head[2:], addr)
	_, err := w.Write(head)
	return err
}

// readPortFWDHeader 读取并返回目标地址。
func readPortFWDHeader(r io.Reader) (string, error) {
	var lens [2]byte
	if _, err := io.ReadFull(r, lens[:]); err != nil {
		return "", err
	}
	buf := make([]byte, binary.BigEndian.Uint16(lens[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	addr := string(buf)
	if !strings.Contains(addr, ":") {
		return "", fmt.Errorf("非法转发目标 %q", addr)
	}
	return addr, nil
}

// PortFWDConn 把 libp2p 流适配为标准 net.Conn。
type PortFWDConn struct {
	streamAdapter
	remote string
}

func (c *PortFWDConn) LocalAddr() net.Addr  { return addrString("lanet-local") }
func (c *PortFWDConn) RemoteAddr() net.Addr { return addrString(c.remote) }
func (c *PortFWDConn) SetDeadline(t time.Time) error {
	return c.Stream.SetDeadline(t)
}
func (c *PortFWDConn) SetReadDeadline(t time.Time) error {
	return c.Stream.SetReadDeadline(t)
}
func (c *PortFWDConn) SetWriteDeadline(t time.Time) error {
	return c.Stream.SetWriteDeadline(t)
}

// addrString 最小 net.Addr 实现。
type addrString string

func (a addrString) Network() string { return "lanet" }
func (a addrString) String() string  { return string(a) }

// halfCloseWriter 支持半关闭的连接（*net.TCPConn、libp2p 流等）。
type halfCloseWriter interface{ CloseWrite() error }

// pipeBoth 双向搬运。半关闭语义：一侧读完后仅关闭对侧写端，
// 双向都结束后才彻底关闭（否则会掐掉尚未读完的回程数据）。
func pipeBoth(a Stream, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		if hc, ok := b.(halfCloseWriter); ok {
			_ = hc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.CloseWrite()
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}
