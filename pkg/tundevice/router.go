// Package router 把 TUN 网卡的 IP 包桥接到 libp2p 群组隧道：
// TUN 出向包 → 按目的虚拟 IP 路由 → 对端隧道流；
// 隧道流入包 → 写回 TUN 交给本机协议栈。
package tundevice

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"runtime"
	"sync"

	"github.com/ayflying/pvn/pkg/firewall"
	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
)

// virtioNetHdrLen Linux 端 wireguard/tun 以 IFF_VNET_HDR 打开 TUN 时的
// virtio 网络头长度（库源码 offload_linux.go: unsafe.Sizeof(virtioNetHdr{})）。
const (
	maxPacketSize   = 65535
	virtioNetHdrLen = 10
)

// packetWriteOffset 返回向 TUN 写包时数据在缓冲区中的起始偏移。
// Linux 端 vnetHdr 模式下库要求 offset >= virtioNetHdrLen（否则 Write 返回
// "invalid offset"，包永远写不进网卡——表现为 Linux 节点 TUN 只发不收，
// 双向 ping 全丢）；调用方须把包放在 [offset:] 并把切片长度截为 offset+包长，
// 库会在 [offset-10:offset] 编码 virtio 头后整体写入。Windows/macOS 无此要求。
func packetWriteOffset() int {
	if runtime.GOOS == "linux" {
		return virtioNetHdrLen
	}
	return 0
}

// Router 负责双向转发。每个对端并发流数量受限，避免单一对端占满。
type Router struct {
	device Device
	tunnel *tunnel.Service
	// fw 统一入向防火墙（nil = 不启用）：对隧道流入向的每个 IP 包
	// 按「源虚拟 IP + 协议 + 目标端口」判定，拒绝的包直接丢弃。
	fw *firewall.Firewall

	mu      sync.Mutex
	streams map[string]network.Stream // virtualIP -> 当前到对端的活跃流
}

func New(device Device, tunnelSvc *tunnel.Service) *Router {
	return &Router{
		device:  device,
		tunnel:  tunnelSvc,
		streams: make(map[string]network.Stream),
	}
}

// SetFirewall 启用统一入向防火墙（Run 之前调用）。
func (r *Router) SetFirewall(fw *firewall.Firewall) { r.fw = fw }

// Run 启动 TUN 读取循环，直到 ctx 取消或设备关闭。
func (r *Router) Run(ctx context.Context) {
	bufs := make([][]byte, 1)
	sizes := make([]int, 1)
	for {
		if ctx.Err() != nil {
			return
		}
		bufs[0] = make([]byte, maxPacketSize)
		n, err := r.device.Read(bufs, sizes, 0)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[router] tun read error: %v", err)
			return
		}
		if n == 0 || sizes[0] == 0 {
			continue
		}
		packet := bufs[0][:sizes[0]]
		if err = r.forwardPacket(ctx, packet); err != nil {
			log.Printf("[router] forward: %v", err)
		}
	}
}

// forwardPacket 解析目的 IP，找到对端路由并经隧道发送。
func (r *Router) forwardPacket(ctx context.Context, packet []byte) error {
	if len(packet) < 20 {
		return fmt.Errorf("packet too short: %d", len(packet))
	}
	version := packet[0] >> 4
	if version != 4 {
		// 非 IPv4 包（Windows/macOS 会向 TUN 发 IPv6 多播与邻居发现等）
		// 属正常噪音，静默丢弃，避免日志刷屏。
		return nil
	}
	destination := fmt.Sprintf("%d.%d.%d.%d", packet[16], packet[17], packet[18], packet[19])

	stream, err := r.streamTo(ctx, destination)
	if err != nil {
		return err
	}
	if _, err = stream.Write(packet); err != nil {
		r.dropStream(destination)
		return fmt.Errorf("write to %s: %w", destination, err)
	}
	return nil
}

// streamTo 返回到目的虚拟 IP 的活跃流，没有则建立。
func (r *Router) streamTo(ctx context.Context, virtualIP string) (network.Stream, error) {
	r.mu.Lock()
	if stream, ok := r.streams[virtualIP]; ok {
		r.mu.Unlock()
		return stream, nil
	}
	r.mu.Unlock()

	stream, _, err := r.tunnel.OpenStreamToVirtualIP(ctx, virtualIP)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.streams[virtualIP] = stream
	r.mu.Unlock()

	// 入向：把对端发来的包写回 TUN。
	go r.pumpFromStream(virtualIP, stream)
	return stream, nil
}

// ServeInboundStream 处理对端主动拨入的隧道流（内含 IP 包）：
// 逐包过防火墙后写回 TUN 交给本机协议栈。这是 TUN 数据面的入向半边——
// 出向由 Run/forwardPacket 主动拨号并 pumpFromStream 读回程；
// 入向（对端拨入本节点）必须由此接线，否则本机永远收不到对端发的包。
// 每条入向流一个 goroutine，流结束或设备关闭即退出。
func (r *Router) ServeInboundStream(stream network.Stream) {
	if p := stream.Conn().RemotePeer(); len(p) > 12 {
		log.Printf("[router] inbound tunnel stream peer=%s…", p[:12])
	}
	writeOffset := packetWriteOffset()
	bufs := make([][]byte, 1)
	sizes := make([]int, 1)
	for {
		bufs[0] = make([]byte, maxPacketSize)
		n, err := stream.Read(bufs[0][writeOffset:])
		if err != nil {
			_ = stream.Close()
			return
		}
		if n == 0 {
			continue
		}
		// 包数据位于 [writeOffset:]，切片长度截为 writeOffset+n，
		// 供库在 [writeOffset-10:writeOffset] 编码 virtio 头（Linux）。
		buf := bufs[0][:writeOffset+n]
		// 统一入向防火墙：源虚拟 IP + 协议 + 目标端口，拒绝即丢包。
		if !CheckPacket(r.fw, buf[writeOffset:]) {
			if r.fw != nil && n >= 20 {
				src := fmt.Sprintf("%d.%d.%d.%d", buf[writeOffset+12], buf[writeOffset+13], buf[writeOffset+14], buf[writeOffset+15])
				dropLog(src, protoName(buf[writeOffset+9]), 0)
			}
			continue
		}
		bufs[0] = buf
		sizes[0] = n
		if _, err = r.device.Write(bufs, writeOffset); err != nil {
			log.Printf("[router] tun write: %v", err)
			_ = stream.Close()
			return
		}
	}
}

func (r *Router) pumpFromStream(virtualIP string, stream network.Stream) {
	defer r.dropStream(virtualIP)
	writeOffset := packetWriteOffset()
	bufs := make([][]byte, 1)
	sizes := make([]int, 1)
	for {
		bufs[0] = make([]byte, maxPacketSize)
		n, err := stream.Read(bufs[0][writeOffset:])
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		// 包数据位于 [writeOffset:]，切片长度截为 writeOffset+n，
		// 供库在 [writeOffset-10:writeOffset] 编码 virtio 头（Linux）。
		buf := bufs[0][:writeOffset+n]
		// 统一入向防火墙：源虚拟 IP + 协议 + 目标端口，拒绝即丢包。
		if !CheckPacket(r.fw, buf[writeOffset:]) {
			if r.fw != nil && n >= 20 {
				src := fmt.Sprintf("%d.%d.%d.%d", buf[writeOffset+12], buf[writeOffset+13], buf[writeOffset+14], buf[writeOffset+15])
				dropLog(src, protoName(buf[writeOffset+9]), 0)
			}
			continue
		}
		bufs[0] = buf
		sizes[0] = n
		if _, err = r.device.Write(bufs, writeOffset); err != nil {
			log.Printf("[router] tun write: %v", err)
			return
		}
		_ = sizes
	}
}

// protoName IP 协议号转名称（日志用）。
func protoName(n byte) string {
	switch n {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	default:
		return fmt.Sprintf("ip:%d", n)
	}
}

func (r *Router) dropStream(virtualIP string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stream, ok := r.streams[virtualIP]; ok {
		_ = stream.Close()
		delete(r.streams, virtualIP)
	}
}

// 有用的小工具：把 IPv4 头中的协议字段取出来（TCP=6 UDP=17），调试用。
func protocolOf(packet []byte) byte {
	if len(packet) < 20 {
		return 0
	}
	return packet[9]
}

var _ = binary.BigEndian // 保留引用，后续分片/校验和扩展用
