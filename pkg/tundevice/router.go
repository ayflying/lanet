// Package router 把 TUN 网卡的 IP 包桥接到 libp2p 群组隧道：
// TUN 出向包 → 按目的虚拟 IP 路由 → 对端隧道流；
// 隧道流入包 → 写回 TUN 交给本机协议栈。
package tundevice

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	tunnel "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
)

// virtioNetHdrLen Linux 端 wireguard/tun 以 IFF_VNET_HDR 打开 TUN 时的
// virtio 网络头长度（库源码 offload_linux.go: unsafe.Sizeof(virtioNetHdr{})）。
const (
	maxPacketSize      = 65535
	virtioNetHdrLen    = 10
	outboundQueueSize  = 256
	maxOutboundWorkers = 1024
	outboundWorkerIdle = time.Minute
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
	// onIP 返回本机虚拟 IP（ARP 应答需要；nil = 不应答 ARP）。
	onIP func() netip.Addr

	mu      sync.Mutex
	streams map[string]*streamState // virtualIP -> 当前到对端的活跃双工流
	// outbound 按目标 IP 隔离拨号和发送。离线成员的慢拨号不能阻塞唯一的
	// TUN 读取循环，否则其他成员的回程包也会滞留在网卡里并表现为随机丢包。
	outbound map[string]*outboundWorker
	// 限制并回收按目标创建的 worker，避免无效地址扫描持续占用 goroutine
	// 和队列内存。字段保留在实例上，便于对边界行为做快速单元测试。
	outboundLimit int
	outboundIdle  time.Duration
	limitDrops    uint64
	// Wintun 的 NativeTun.Read/Write 要求每个方向由单一调用方串行访问。
	// 多条入向隧道流可能同时写 TUN，不加锁会在环形缓冲上产生间歇性丢包。
	writeMu sync.Mutex
	// dialing single-flight：同一虚拟 IP 只允许一个 goroutine 拨号，
	// 其余调用等待复用结果。没有它，TUN 读循环里每个丢包的 ping 都会
	// 各自触发一次拨号，几十个并发拨号会打爆 libp2p 资源限制
	// （观测到 resource limit exceeded / NO_RESERVATION），拖垮重连。
	dialing map[string]*dialCall
}

// dialCall 一次拨号的结果广播。done 关闭后 err 可安全读取。
type dialCall struct {
	done chan struct{}
	err  error
}

type outboundWorker struct {
	packets chan []byte
	dropped uint64
}

// streamState 串行化同一字节流上的包写入。network.Stream 是字节流，若多个
// goroutine 并发 Write，IP 包字节可能交错，接收端将无法按 IPv4 total_length 分帧。
type streamState struct {
	stream  network.Stream
	writeMu sync.Mutex
}

func New(device Device, tunnelSvc *tunnel.Service) *Router {
	return &Router{
		device:        device,
		tunnel:        tunnelSvc,
		streams:       make(map[string]*streamState),
		outbound:      make(map[string]*outboundWorker),
		outboundLimit: maxOutboundWorkers,
		outboundIdle:  outboundWorkerIdle,
		dialing:       make(map[string]*dialCall),
	}
}

// SetFirewall 启用统一入向防火墙（Run 之前调用）。
func (r *Router) SetFirewall(fw *firewall.Firewall) { r.fw = fw }

// SetLocalIP 设置本机虚拟 IP 提供函数（ARP 应答用，Run 之前调用）。
func (r *Router) SetLocalIP(fn func() netip.Addr) { r.onIP = fn }

// Run 启动 TUN 读取循环，直到 ctx 取消或设备关闭。
func (r *Router) Run(ctx context.Context) {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	bufs := [][]byte{make([]byte, maxPacketSize)}
	sizes := make([]int, 1)
	for {
		if ctx.Err() != nil {
			return
		}
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
		// ARP 请求应答：Windows L3 TUN 下内核对 on-link 目的强制邻居解析，
		// 无人应答则条目 Unreachable、单播全丢。必须在此处回应答。
		// Wintun Raw IP 模式：IPv4/IPv6 无以太网头；ARP 可能以两种形态出现——
		//   a) 带 14 字节以太网头：dstMAC(6)+srcMAC(6)+etype=0x0806
		//   b) 裸 ARP 帧：htype=0x0001 开头
		arpOffset := -1
		if len(packet) >= 16 && packet[12] == 0x08 && packet[13] == 0x06 {
			arpOffset = 14 // 以太网帧封装
		} else if len(packet) >= 2 && packet[0] == 0x00 && packet[1] == 0x01 {
			arpOffset = 0 // 裸 ARP 帧
		}
		if arpOffset >= 0 {
			frame := packet[arpOffset:]
			if _, targetIP, err := parseARPRequest(frame); err == nil {
				var myIP netip.Addr
				if r.onIP != nil {
					myIP = r.onIP()
				}
				ip4 := myIP.As4()
				if targetIP == ip4 {
					reply := make([]byte, arpOffset+arpFrameLen)
					// 以太网形态须回填应答的以太网头：dst=提问者MAC src=本机假MAC
					if arpOffset == 14 {
						copy(reply[0:6], packet[6:12]) // dst = 原帧 src
						copy(reply[6:12], packet[0:6]) // src = 原帧 dst（广播）
						binary.BigEndian.PutUint16(reply[12:14], 0x0806)
					}
					buildARPReply(frame, reply[arpOffset:])
					wOff := packetWriteOffset()
					out := make([]byte, wOff+len(reply))
					copy(out[wOff:], reply)
					if err := r.writeDevice([][]byte{out}, wOff); err != nil {
						log.Printf("[router] arp reply write: %v", err)
					}
				}
			}
			continue
		}
		if err = r.enqueuePacket(workerCtx, packet); err != nil {
			log.Printf("[router] forward: %v", err)
		}
	}
}

// enqueuePacket 把包交给目标 IP 专属的串行 worker。不同目标可并行拨号和发送，
// 同一目标仍保持内核交付顺序，避免 TCP 包乱序。队列满时丢弃新包，由上层协议重传。
func (r *Router) enqueuePacket(ctx context.Context, packet []byte) error {
	if len(packet) < 20 {
		return fmt.Errorf("packet too short: %d", len(packet))
	}
	if packet[0]>>4 != 4 {
		return nil
	}
	destinationAddr := netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	if !destinationAddr.IsGlobalUnicast() {
		return nil
	}
	destination := destinationAddr.String()

	r.mu.Lock()
	worker, ok := r.outbound[destination]
	if !ok {
		if len(r.outbound) >= r.outboundLimit {
			r.limitDrops++
			dropped := r.limitDrops
			r.mu.Unlock()
			if dropped == 1 || dropped%100 == 0 {
				return fmt.Errorf("outbound worker limit %d reached (dropped=%d)", r.outboundLimit, dropped)
			}
			return nil
		}
		worker = &outboundWorker{packets: make(chan []byte, outboundQueueSize)}
		r.outbound[destination] = worker
		go r.runOutbound(ctx, destination, worker)
	}
	// Run 复用 TUN 读取缓冲。队列和 worker 异步消费，因此入队前必须复制，
	// 与 Nebula 握手缓存、WireGuard staged queue 的数据所有权语义一致。
	ownedPacket := append([]byte(nil), packet...)
	select {
	case worker.packets <- ownedPacket:
		r.mu.Unlock()
		return nil
	default:
		worker.dropped++
		dropped := worker.dropped
		r.mu.Unlock()
		if dropped == 1 || dropped%100 == 0 {
			return fmt.Errorf("outbound queue to %s is full (dropped=%d)", destination, dropped)
		}
		return nil
	}
}

func (r *Router) runOutbound(ctx context.Context, destination string, worker *outboundWorker) {
	timer := time.NewTimer(r.outboundIdle)
	defer timer.Stop()
	defer r.removeOutboundWorker(destination, worker)
	var failures uint64
	for {
		select {
		case <-ctx.Done():
			return
		case packet := <-worker.packets:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if err := r.forwardPacket(ctx, packet); err != nil {
				failures++
				if failures == 1 || failures%100 == 0 {
					log.Printf("[router] forward to %s failed (count=%d): %v", destination, failures, err)
				}
			} else {
				failures = 0
			}
			timer.Reset(r.outboundIdle)
		case <-timer.C:
			r.mu.Lock()
			current := r.outbound[destination]
			if current == worker && len(worker.packets) == 0 {
				delete(r.outbound, destination)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			timer.Reset(r.outboundIdle)
		}
	}
}

func (r *Router) removeOutboundWorker(destination string, worker *outboundWorker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outbound[destination] == worker {
		delete(r.outbound, destination)
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

	state, err := r.streamTo(ctx, destination)
	if err != nil {
		return err
	}
	state.writeMu.Lock()
	_, err = state.stream.Write(packet)
	state.writeMu.Unlock()
	if err != nil {
		r.dropStream(destination, state)
		return fmt.Errorf("write to %s: %w", destination, err)
	}
	return nil
}

// streamTo 返回到目的虚拟 IP 的活跃流，没有则建立。
// 拨号按虚拟 IP single-flight 合并：并发调用只发起一次真实拨号，
// 其余等待结果并复用同一条流，避免拨号风暴。
func (r *Router) streamTo(ctx context.Context, virtualIP string) (*streamState, error) {
	for {
		r.mu.Lock()
		if state, ok := r.streams[virtualIP]; ok {
			r.mu.Unlock()
			return state, nil
		}
		if call, ok := r.dialing[virtualIP]; ok {
			// 已有拨号进行中：等待其结果后重查缓存。
			r.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		// 由当前调用者发起拨号。
		call := &dialCall{done: make(chan struct{})}
		r.dialing[virtualIP] = call
		r.mu.Unlock()

		state, err := r.dialStream(ctx, virtualIP)
		r.mu.Lock()
		delete(r.dialing, virtualIP)
		r.mu.Unlock()
		call.err = err
		close(call.done)
		return state, err
	}
}

// dialStream 真实拨号并注册到流缓存。
func (r *Router) dialStream(ctx context.Context, virtualIP string) (*streamState, error) {
	stream, _, err := r.tunnel.OpenStreamToVirtualIP(ctx, virtualIP)
	if err != nil {
		// 失败时摘除可能残留的死流，让下一次拨号真正重建，
		// 而不是继续向已断开的流写包（静默丢包）。
		r.dropStream(virtualIP, nil)
		return nil, err
	}
	state := &streamState{stream: stream}

	r.mu.Lock()
	// 拨号期间对端可能已经建立了反向流。保留先注册的健康流，关闭重复流。
	if existing, ok := r.streams[virtualIP]; ok {
		r.mu.Unlock()
		_ = stream.Close()
		return existing, nil
	}
	r.streams[virtualIP] = state
	r.mu.Unlock()

	log.Printf("[router] tunnel established to %s via peer=%s remote=%s",
		virtualIP, stream.Conn().RemotePeer().ShortString(), stream.Conn().RemoteMultiaddr())

	// 入向：把对端发来的包写回 TUN。
	go r.pumpFromStream(virtualIP, state)
	return state, nil
}

// ServeInboundStream 处理对端主动拨入的隧道流（内含 IP 包）。virtualIP 必须是
// 该 PeerID 对应的虚拟 IP；注册后，本机协议栈产生的 Reply 会沿同一条双工流返回，
// 避免 NAT/拨号 backoff 令回程包等待数秒甚至直接丢失。
func (r *Router) ServeInboundStream(virtualIP string, stream network.Stream) {
	if virtualIP == "" {
		log.Printf("[router] inbound tunnel stream ignored: unknown peer=%s", stream.Conn().RemotePeer().ShortString())
		_ = stream.Reset()
		return
	}
	state := &streamState{stream: stream}
	r.mu.Lock()
	if old, ok := r.streams[virtualIP]; ok && old.stream != stream {
		_ = old.stream.Close()
	}
	r.streams[virtualIP] = state
	r.mu.Unlock()
	log.Printf("[router] inbound tunnel established from %s peer=%s",
		virtualIP, stream.Conn().RemotePeer().ShortString())
	r.pumpFromStream(virtualIP, state)
}

// writeInbound 把一个完整的 IP 包过防火墙后写回 TUN。
func (r *Router) writeInbound(bufs [][]byte, sizes []int, pkt []byte, writeOffset int) {
	n := len(pkt)
	// 统一入向防火墙：源虚拟 IP + 协议 + 目标端口，拒绝即丢包。
	if !CheckPacket(r.fw, pkt) {
		if r.fw != nil && n >= 20 {
			src := fmt.Sprintf("%d.%d.%d.%d", pkt[12], pkt[13], pkt[14], pkt[15])
			dropLog(src, protoName(pkt[9]), 0)
		}
		return
	}
	// 包数据位于 [writeOffset:]，切片长度截为 writeOffset+n，
	// 供库在 [writeOffset-10:writeOffset] 编码 virtio 头（Linux）。
	buf := make([]byte, writeOffset+n)
	copy(buf[writeOffset:], pkt)
	bufs[0] = buf
	sizes[0] = n
	if err := r.writeDevice(bufs, writeOffset); err != nil {
		log.Printf("[router] tun write: %v", err)
		return
	}
}

// writeDevice 串行化所有 TUN 写入。wireguard/tun 的 NativeTun 实现明确要求
// 同一设备的 Write 不被多个 goroutine 并发调用；Router 的多个入向流则天然并发。
func (r *Router) writeDevice(bufs [][]byte, offset int) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err := r.device.Write(bufs, offset)
	return err
}

func (r *Router) pumpFromStream(virtualIP string, state *streamState) {
	defer r.dropStream(virtualIP, state)
	writeOffset := packetWriteOffset()
	bufs := make([][]byte, 1)
	sizes := make([]int, 1)
	readBuf := make([]byte, maxPacketSize)
	framer := &ipFramer{}
	for {
		n, err := state.stream.Read(readBuf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		// 隧道是字节流，必须按 IP 包边界切分后再逐个写 TUN。
		for _, pkt := range framer.feed(readBuf[:n]) {
			r.writeInbound(bufs, sizes, pkt, writeOffset)
		}
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

func (r *Router) dropStream(virtualIP string, state *streamState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// state == nil：无条件摘除（拨号失败后清理残留死流）。
	// state != nil：只删除触发清理的这一条流；若已有新流替换，
	// 旧 goroutine 退出不能误删新流。
	if state == nil {
		if current, ok := r.streams[virtualIP]; ok {
			_ = current.stream.Close()
			delete(r.streams, virtualIP)
		}
		return
	}
	if current, ok := r.streams[virtualIP]; ok && current == state {
		_ = current.stream.Close()
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
