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
	"sync/atomic"
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
	maxOutboundWorkers = 256
	streamIdleTimeout  = 5 * time.Minute
	outboundWorkerIdle = time.Minute
	// outboundQueueBudget 全局出向队列字节预算：所有目标 worker 队列里尚未
	// 转发（在途）的包字节数之和上限。超过即丢弃新包（先判满再拷贝），由上层
	// TCP/UDP 重传兜底。默认 16MiB，足以在 ~1Gbps 下缓冲约 70ms，同时把最坏
	// 情况下的内存占用钉死，避免单一离线目标把 TUN 读取缓冲无限堆积。
	outboundQueueBudget = 16 * 1024 * 1024
	// outboundWriteDeadline 单包写向隧道流的最大阻塞时间。对端不读时 Write 会
	// 一直挂起并拖垮整个串行 worker；超时即判定对端失效、摘流重连。
	outboundWriteDeadline = 30 * time.Second
	// outboundCooldownBase/Max 连续转发失败时的退避区间：对失效对端空转会打满
	// 日志与资源，按失败次数指数退避、封顶 Max。
	outboundCooldownBase = 5 * time.Millisecond
	outboundCooldownMax  = 320 * time.Millisecond
	// maxConsecutiveReadErrors 连续「非瞬时」读错误上限：达到即判定设备
	// 已失效并退出读循环。单包级瞬时错误（IsRecoverableReadError）不计入。
	maxConsecutiveReadErrors = 100
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
	// writeBudget / writeBudgetMax 出向队列在途字节预算（原子访问）。
	// writeBudget 为所有目标 worker 当前在途（已入队、尚未转发）的字节总数；
	// 超过 writeBudgetMax（默认 16MiB）时 enqueue 直接丢弃新包。包被 worker
	// 取走转发后释放；worker 停止时队列里残留的包一并释放（停止后预算释放）。
	writeBudget    int64
	writeBudgetMax int64
	// forwardImpl 是 forwardPacket 的可替换实现，仅供单元测试注入；
	// 生产路径始终为 (*Router).forwardPacket。
	forwardImpl func(context.Context, []byte) error
	// Wintun 的 NativeTun.Read/Write 要求每个方向由单一调用方串行访问。
	// 多条入向隧道流可能同时写 TUN，不加锁会在环形缓冲上产生间歇性丢包。
	writeMu sync.Mutex
	// dialing single-flight：同一虚拟 IP 只允许一个 goroutine 拨号，
	// 其余调用等待复用结果。没有它，TUN 读循环里每个丢包的 ping 都会
	// 各自触发一次拨号，几十个并发拨号会打爆 libp2p 资源限制
	// （观测到 resource limit exceeded / NO_RESERVATION），拖垮重连。
	dialing    map[string]*dialCall
	closed     bool
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	pumps      sync.WaitGroup
	streamIdle time.Duration
	closeDone  chan struct{}
	runDone    chan struct{}
}

// dialCall 一次拨号的结果广播。done 关闭后 err 可安全读取。
type dialCall struct {
	done chan struct{}
	err  error
}

type outboundWorker struct {
	cancel  context.CancelFunc
	packets chan []byte
	dropped uint64
}

// streamState 串行化同一字节流上的包写入。network.Stream 是字节流，若多个
// goroutine 并发 Write，IP 包字节可能交错，接收端将无法按 IPv4 total_length 分帧。
type streamState struct {
	stream       network.Stream
	writeMu      sync.Mutex
	lastActivity atomic.Int64
	writing      atomic.Int32
}

func New(device Device, tunnelSvc *tunnel.Service) *Router {
	r := &Router{
		device:         device,
		tunnel:         tunnelSvc,
		streams:        make(map[string]*streamState),
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  maxOutboundWorkers,
		outboundIdle:   outboundWorkerIdle,
		dialing:        make(map[string]*dialCall),
		writeBudgetMax: outboundQueueBudget,
		streamIdle:     streamIdleTimeout,
	}
	r.forwardImpl = func(ctx context.Context, p []byte) error { return r.forwardPacket(ctx, p) }
	return r
}

// SetFirewall 启用统一入向防火墙（Run 之前调用）。
func (r *Router) SetFirewall(fw *firewall.Firewall) { r.fw = fw }

// SetLocalIP 设置本机虚拟 IP 提供函数（ARP 应答用，Run 之前调用）。
func (r *Router) SetLocalIP(fn func() netip.Addr) { r.onIP = fn }

// Run 启动 TUN 读取循环，直到 ctx 取消或设备关闭。
func (r *Router) Run(ctx context.Context) {
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	r.mu.Lock()
	if r.closed || r.runDone != nil {
		r.mu.Unlock()
		cancelWorkers()
		return
	}
	r.cancel = cancelWorkers
	r.runDone = make(chan struct{})
	r.workers.Add(1)
	r.mu.Unlock()
	stop := context.AfterFunc(workerCtx, func() { _ = r.device.Close() })
	defer stop()
	defer func() {
		close(r.runDone)
		r.mu.Lock()
		closing := r.closed
		r.mu.Unlock()
		if !closing {
			r.Close()
		}
	}()
	go func() { defer r.workers.Done(); r.reapStreams(workerCtx) }()
	bufs := [][]byte{make([]byte, maxPacketSize)}
	sizes := make([]int, 1)
	// consecutiveErrs / transientErrs 区分「偶发单包错误」与「设备失效」，
	// 避免任何一次读取失败都终止整个数据面（见下方错误分支注释）。
	consecutiveErrs := 0
	transientErrs := 0
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
			// 设备已被显式关闭：正常退出。
			if IsDeviceClosed(err) {
				log.Printf("[router] TUN 已关闭，读循环退出: %v", err)
				return
			}
			// 单包级瞬时错误（如 wireguard tun 的 ErrTooManySegments）：
			// 只丢弃这一个包，必须继续读。老实现此处直接 return，会让
			// 虚拟网数据面永久停摆，而控制面（libp2p/probe/控制台状态）
			// 完全正常——对外表现为「控制台显示成员在线、直连、rtt 正常，
			// 但 ping 与所有 TCP 端口全部超时」。Read 自身阻塞，且每个
			// 错误都对应一个真实到达的包，因此这里不存在空转风险。
			// 该错误可能频繁出现，只按次汇总打印，避免刷爆日志。
			if IsRecoverableReadError(err) {
				transientErrs++
				if transientErrs == 1 || transientErrs%1000 == 0 {
					log.Printf("[router] tun 读取跳过瞬时错误 %d 次（读循环继续）: %v", transientErrs, err)
				}
				if !waitRouter(ctx, time.Millisecond) {
					return
				}
				continue
			}
			// 其余错误：退避重试，连续超限才判定设备失效。
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutiveReadErrors {
				log.Printf("[router] TUN 连续读取失败 %d 次，退出读循环: %v", consecutiveErrs, err)
				return
			}
			log.Printf("[router] tun read error (%d/%d，重试): %v", consecutiveErrs, maxConsecutiveReadErrors, err)
			if !waitRouter(ctx, time.Duration(consecutiveErrs)*20*time.Millisecond) {
				return
			}
			continue
		}
		consecutiveErrs = 0
		if n == 0 || sizes[0] == 0 {
			if !waitRouter(ctx, time.Millisecond) {
				return
			}
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
// 同一目标仍保持内核交付顺序，避免 TCP 包乱序。队列满或全局字节预算满时丢弃新包，
// 由上层协议重传。
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
	if r.closed || ctx.Err() != nil {
		r.mu.Unlock()
		return context.Canceled
	}
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
		workerCtx, workerCancel := context.WithCancel(ctx)
		worker = &outboundWorker{cancel: workerCancel, packets: make(chan []byte, outboundQueueSize)}
		r.outbound[destination] = worker
		r.workers.Add(1)
		go func() { defer r.workers.Done(); defer workerCancel(); r.runOutbound(workerCtx, destination, worker) }()
	}

	// 先判满再拷贝：队列或全局字节预算已满时直接丢弃，避免为必丢的包分配并
	// 拷贝内存。预算约束所有目标 worker 的在途字节总量（默认 16MiB）。
	if len(worker.packets) >= cap(worker.packets) || !r.acquireBudget(len(packet)) {
		worker.dropped++
		dropped := worker.dropped
		r.mu.Unlock()
		if dropped == 1 || dropped%100 == 0 {
			return fmt.Errorf("outbound to %s dropped (queue full or budget %d/%d bytes exceeded)",
				destination, atomic.LoadInt64(&r.writeBudget), r.writeBudgetMax)
		}
		return nil
	}

	// Run 复用 TUN 读取缓冲。队列和 worker 异步消费，因此入队前必须复制，
	// 与 Nebula 握手缓存、WireGuard staged queue 的数据所有权语义一致。
	ownedPacket := append([]byte(nil), packet...)
	select {
	case worker.packets <- ownedPacket:
		r.mu.Unlock()
		return nil
	default:
		// 极小概率的竞争：判满到入队之间队列被灌满。释放刚占用的预算并丢弃，
		// 不浪费已拷贝的内存（调用方走丢包重传）。
		r.releaseBudget(len(packet))
		worker.dropped++
		dropped := worker.dropped
		r.mu.Unlock()
		if dropped == 1 || dropped%100 == 0 {
			return fmt.Errorf("outbound to %s dropped (queue full under race)", destination)
		}
		return nil
	}
}

// acquireBudget 尝试占用 n 字节出向队列预算；预算不足返回 false。
// 先判满再拷贝：调用方应在拷贝包之前调用，预算不足时直接丢弃，避免无谓内存拷贝。
func (r *Router) acquireBudget(n int) bool {
	for {
		cur := atomic.LoadInt64(&r.writeBudget)
		if cur+int64(n) > r.writeBudgetMax {
			return false
		}
		if atomic.CompareAndSwapInt64(&r.writeBudget, cur, cur+int64(n)) {
			return true
		}
	}
}

// releaseBudget 归还 n 字节出向队列预算（包被消费或 worker 停止时调用）。
func (r *Router) releaseBudget(n int) {
	atomic.AddInt64(&r.writeBudget, -int64(n))
}

// Close 停止本 Router 的任务；关闭设备解阻读写，不修改成员与好友信息。
func (r *Router) Close() {
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	r.closed = true
	r.closeDone = make(chan struct{})
	cancel := r.cancel
	streams := r.streams
	r.streams = make(map[string]*streamState)
	workers := make([]*outboundWorker, 0, len(r.outbound))
	for _, worker := range r.outbound {
		workers = append(workers, worker)
	}
	r.outbound = make(map[string]*outboundWorker)
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, worker := range workers {
		if worker.cancel != nil {
			worker.cancel()
		}
	}
	if r.device != nil {
		_ = r.device.Close()
	}
	if r.runDone != nil {
		<-r.runDone
	}
	for _, st := range streams {
		_ = st.stream.Reset()
	}
	r.workers.Wait()
	r.pumps.Wait()
	close(r.closeDone)
}

func (r *Router) reapStreams(ctx context.Context) {
	idle := r.streamIdle
	if idle <= 0 {
		idle = streamIdleTimeout
	}
	ticker := time.NewTicker(idle / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.reapIdle(now, idle)
		}
	}
}

func (r *Router) reapIdle(now time.Time, idle time.Duration) {
	var stale []*streamState
	r.mu.Lock()
	for ip, st := range r.streams {
		if st.writing.Load() == 0 && now.Sub(time.Unix(0, st.lastActivity.Load())) >= idle {
			delete(r.streams, ip)
			stale = append(stale, st)
		}
	}
	r.mu.Unlock()
	for _, st := range stale {
		_ = st.stream.Reset()
	}
}

func waitRouter(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
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
			if err := r.forwardImpl(ctx, packet); err != nil {
				failures++
				// 失败冷却：连续失败时对失效对端指数退避、封顶，避免空转打满
				// 资源与日志。ctx 已取消时不再睡眠，尽快退出。
				if failures == 1 || failures%100 == 0 {
					log.Printf("[router] forward to %s failed (count=%d): %v", destination, failures, err)
				}
				if ctx.Err() == nil {
					waitRouter(ctx, r.failureCooldown(failures))
				}
			} else {
				failures = 0
			}
			// 释放在途字节预算：无论成功失败，包都已离开队列。
			r.releaseBudget(len(packet))
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

// failureCooldown 返回第 failures 次连续失败后的退避时长（指数退避、封顶）。
func (r *Router) failureCooldown(failures uint64) time.Duration {
	exp := failures - 1
	if exp > 6 {
		exp = 6
	}
	d := outboundCooldownBase * (1 << exp)
	if d > outboundCooldownMax {
		return outboundCooldownMax
	}
	return d
}

// removeOutboundWorker 摘除 worker 映射，并释放其队列里残留包的在途字节预算
// （停止后预算释放）。以 defer 形式调用，runOutbound 任意出口都会执行。
func (r *Router) removeOutboundWorker(destination string, worker *outboundWorker) {
	r.mu.Lock()
	if r.outbound[destination] == worker {
		delete(r.outbound, destination)
	}
	r.mu.Unlock()
	// 排空队列：worker 已停止，残留包不再转发，直接归还其占用的字节预算。
	var total int
	for {
		select {
		case p := <-worker.packets:
			total += len(p)
		default:
			if total > 0 {
				r.releaseBudget(total)
			}
			return
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

	var state *streamState
	for {
		var err error
		state, err = r.streamTo(ctx, destination)
		if err != nil {
			return err
		}
		r.mu.Lock()
		if r.streams[destination] == state && !r.closed {
			state.writing.Add(1)
			state.lastActivity.Store(time.Now().UnixNano())
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	defer state.writing.Add(-1)

	// 关闭/取消时 Reset 打断阻塞写；正常返回立刻撤销回调。
	resetFn := context.AfterFunc(ctx, func() { _ = state.stream.Reset() })
	defer resetFn()

	state.writeMu.Lock()
	state.stream.SetWriteDeadline(time.Now().Add(outboundWriteDeadline))
	_, err := state.stream.Write(packet)
	if err == nil {
		state.lastActivity.Store(time.Now().UnixNano())
	}
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
		if r.closed {
			r.mu.Unlock()
			return nil, context.Canceled
		}
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
		// 本次拨号尚未注册流；并发入向流可能已被注册，不能误删。
		return nil, err
	}
	state := &streamState{stream: stream}
	state.lastActivity.Store(time.Now().UnixNano())

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = stream.Reset()
		return nil, context.Canceled
	}
	// 拨号期间对端可能已经建立了反向流。保留先注册的健康流，关闭重复流。
	if existing, ok := r.streams[virtualIP]; ok {
		r.mu.Unlock()
		_ = stream.Close()
		return existing, nil
	}
	if len(r.streams) >= maxOutboundWorkers {
		r.mu.Unlock()
		_ = stream.Reset()
		return nil, fmt.Errorf("router stream limit reached")
	}
	r.streams[virtualIP] = state
	r.pumps.Add(1)
	r.mu.Unlock()

	log.Printf("[router] tunnel established to %s via peer=%s remote=%s",
		virtualIP, stream.Conn().RemotePeer().ShortString(), stream.Conn().RemoteMultiaddr())

	// 入向：把对端发来的包写回 TUN。
	go func() { defer r.pumps.Done(); r.pumpFromStream(virtualIP, state) }()
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
	state.lastActivity.Store(time.Now().UnixNano())
	r.mu.Lock()
	if r.closed || (r.streams[virtualIP] == nil && len(r.streams) >= maxOutboundWorkers) {
		r.mu.Unlock()
		_ = stream.Reset()
		return
	}
	old := r.streams[virtualIP]
	r.streams[virtualIP] = state
	r.pumps.Add(1)
	r.mu.Unlock()
	defer r.pumps.Done()
	if old != nil && old.stream != stream {
		_ = old.stream.Reset() // 旧 pump 的 dropStream 只摘自身，不能误删新流。
	}
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
			time.Sleep(time.Millisecond)
			continue
		}
		state.lastActivity.Store(time.Now().UnixNano())
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
	current := r.streams[virtualIP]
	if current != nil && (state == nil || current == state) {
		delete(r.streams, virtualIP)
	} else {
		current = nil
	}
	r.mu.Unlock()
	if current != nil {
		_ = current.stream.Reset()
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
