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
	"github.com/libp2p/go-libp2p/core/peer"
)

// virtioNetHdrLen Linux 端 wireguard/tun 以 IFF_VNET_HDR 打开 TUN 时的
// virtio 网络头长度（库源码 offload_linux.go: unsafe.Sizeof(virtioNetHdr{})）。
var lanetVirtualIPv6Prefix = netip.MustParsePrefix("fd00:6c61:6e65::/48")

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
	// streamRetireGrace 被仲裁淘汰的流：先 Close 让对端读到 EOF 干净退出，
	// 宽限后再 Reset 释放本地读侧。避免对方收到 STREAM_RESET 打出
	// "stream reset by remote, error code 0"（代码 0 = NO_ERROR，来自 Reset()
	// 而非协议拒绝），从而切断秒级重建循环的反馈信号。
	streamRetireGrace = 2 * time.Second
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

	mu   sync.Mutex
	// flows 按「对端 PeerID」索引：任一时刻对每个对端有且仅有一条活跃双工流。
	// 不能按虚拟 IP 索引——同一 peer 同时拥有 IPv4/IPv6 两个虚拟地址，
	// 按 IP 建表会让同一 peer 出现两条独立流，进而互相 Reset 成死循环。
	flows map[peer.ID]*peerFlow
	// flowByAddr 是虚拟地址（IPv4/IPv6）到对端 PeerID 的别名索引：
	// 同一 peer 的两个地址都指向同一条流。目的是让稳态下的出向查表是 O(1)，
	// 避免每个包都去扫一遍 NetMap 成员表。
	flowByAddr map[string]peer.ID
	// selfID 本机 PeerID，是出向流的发起方标识，参与对称仲裁。
	selfID peer.ID
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
	// dialing single-flight：同一对端只允许一个 goroutine 拨号，其余调用等待
	// 复用结果。没有它，TUN 读循环里每个丢包的 ping 都会各自触发一次拨号，
	// 几十个并发拨号会打爆 libp2p 资源限制（观测到 resource limit exceeded /
	// NO_RESERVATION），拖垮重连。按 PeerID 而非 IP 归并，确保同一 peer 的
	// IPv4 与 IPv6 包共用一次拨号。
	dialing    map[peer.ID]*dialCall
	closed     bool
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	pumps      sync.WaitGroup
	streamIdle time.Duration
	// retireGrace 淘汰一条竞争失败流后到强制 Reset 之间的宽限期：Close 先让
	// 对端读到 EOF 干净退出，宽限期结束仍未退出的再 Reset 释放读侧资源。
	retireGrace time.Duration
	closeDone   chan struct{}
	runDone     chan struct{}
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

// peerFlow 是一个对端的一条隧道双工流。
//
// network.Stream 是字节流，若多个 goroutine 并发 Write，IP 包字节可能交错，
// 接收端将无法按 IPv4 total_length 分帧 —— 同一字节流上的包写入必须串行。
type peerFlow struct {
	stream network.Stream
	// peer 是流的对端 PeerID，也是流表的键。
	peer peer.ID
	// initiator 是这条流的发起方：出向流是本机，入向流是对端。
	// 双方都拿得到对方的 PeerID，因此两端各自算出的仲裁赢家必然一致。
	initiator peer.ID
	// addrs 是该 peer 的全部虚拟地址别名（IPv4 + IPv6），用于建立别名索引。
	addrs []string
	// retired 表示这条流已被淘汰：不再向 TUN 投递数据，也不再作为查表结果。
	// 置位后 read 循环会自行退出并对流做最终清理。
	retired atomic.Bool
	// writeMu 串行化同一字节流上的包写入：IPv4 与 IPv6 的出向 worker 是两条
	// 独立队列，但最终汇聚到同一条流，不加锁会让字节交错、接收端无法分帧。
	writeMu sync.Mutex
	// writes 统计尚未完成的并发写，用于 reapIdle 避开在途写。
	writes     atomic.Int32
	lastActive atomic.Int64
}

func (f *peerFlow) touch() { f.lastActive.Store(time.Now().UnixNano()) }

func New(device Device, tunnelSvc *tunnel.Service) *Router {
	r := &Router{
		device:         device,
		tunnel:         tunnelSvc,
		flows:          make(map[peer.ID]*peerFlow),
		flowByAddr:     make(map[string]peer.ID),
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  maxOutboundWorkers,
		outboundIdle:   outboundWorkerIdle,
		dialing:        make(map[peer.ID]*dialCall),
		writeBudgetMax: outboundQueueBudget,
		streamIdle:     streamIdleTimeout,
		retireGrace:    streamRetireGrace,
	}
	if tunnelSvc != nil {
		r.selfID = tunnelSvc.LocalPeerID()
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
	destination, err := packetDestination(packet)
	if err != nil {
		return err
	}
	if destination == "" {
		return nil
	}
	destinationAddr, err := netip.ParseAddr(destination)
	if err != nil || (!destinationAddr.IsGlobalUnicast() && !lanetVirtualIPv6Prefix.Contains(destinationAddr)) || (destinationAddr.Is6() && destinationAddr.IsPrivate() && !lanetVirtualIPv6Prefix.Contains(destinationAddr)) {
		return nil
	}

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
	// 流表按对端归一后每个对端只有一条流，直接整表取走即可；别名索引同步清空。
	flows := r.flows
	r.flows = make(map[peer.ID]*peerFlow)
	r.flowByAddr = make(map[string]peer.ID)
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
	// 主动关闭一律 Reset：不再需要对方的读循环配合，立即释放底层资源。
	for _, flow := range flows {
		flow.retired.Store(true)
		_ = flow.stream.Reset()
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

// reapIdle 回收空闲流。流表按对端归一，删掉一条流同时清掉它的全部地址别名。
func (r *Router) reapIdle(now time.Time, idle time.Duration) {
	var stale []*peerFlow
	r.mu.Lock()
	for _, flow := range r.flows {
		if flow.writes.Load() == 0 && now.Sub(time.Unix(0, flow.lastActive.Load())) >= idle {
			stale = append(stale, flow)
		}
	}
	r.mu.Unlock()
	for _, flow := range stale {
		r.retireFlow(flow, "idle")
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

// packetDestination 校验 IP 头并返回目的地址；空字符串表示不支持的版本，静默丢弃。
func packetDestination(packet []byte) (string, error) {
	if len(packet) == 0 {
		return "", fmt.Errorf("packet too short: 0")
	}
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return "", fmt.Errorf("IPv4 packet too short: %d", len(packet))
		}
		headerLen := int(packet[0]&0x0f) * 4
		if headerLen < 20 || len(packet) < headerLen {
			return "", fmt.Errorf("invalid or truncated IPv4 header: ihl=%d length=%d", headerLen, len(packet))
		}
		totalLen := int(packet[2])<<8 | int(packet[3])
		if totalLen < headerLen || totalLen > len(packet) {
			return "", fmt.Errorf("invalid or truncated IPv4 packet: total length=%d length=%d", totalLen, len(packet))
		}
		addr, ok := netip.AddrFromSlice(packet[16:20])
		if !ok {
			return "", fmt.Errorf("invalid IPv4 destination")
		}
		return addr.String(), nil
	case 6:
		if len(packet) < 40 {
			return "", fmt.Errorf("IPv6 packet too short: %d", len(packet))
		}
		payloadLen := int(packet[4])<<8 | int(packet[5])
		if payloadLen == 0 && packet[6] == 0 && len(packet) > 40 {
			return "", nil // Hop-by-Hop 包的 jumbogram option 链由分帧器拒绝。
		}
		if payloadLen+40 > len(packet) {
			return "", fmt.Errorf("truncated IPv6 packet: payload length=%d length=%d", payloadLen, len(packet))
		}
		addr, ok := netip.AddrFromSlice(packet[24:40])
		if !ok {
			return "", fmt.Errorf("invalid IPv6 destination")
		}
		return addr.String(), nil
	default:
		return "", nil
	}
}

// lookupPeer 把虚拟地址解析为对端 PeerID。
// 先查别名索引（稳态 O(1)，同一 peer 的 IPv4/IPv6 都已指向同一 PeerID）；
// 未命中再查一次 NetMap 并把该 peer 的全部别名回填，避免每个包都扫成员表。
func (r *Router) lookupPeer(destination string) (peer.ID, error) {
	r.mu.Lock()
	target, ok := r.flowByAddr[destination]
	r.mu.Unlock()
	if ok && target != "" {
		return target, nil
	}
	if r.tunnel == nil {
		return "", fmt.Errorf("tunnel service unavailable")
	}
	resolved, aliases, found := r.tunnel.PeerRoute(destination)
	if !found {
		return "", fmt.Errorf("virtual IP %s not in group netmap", destination)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", context.Canceled
	}
	// 回填前先核对：期间可能已有别的路径把这个地址绑到了同一 peer 上。
	if current, bound := r.flowByAddr[destination]; bound && current != "" {
		resolved = current
	} else {
		for _, addr := range aliases {
			if owner, bound := r.flowByAddr[addr]; !bound || owner == "" || owner == resolved {
				r.flowByAddr[addr] = resolved
			}
		}
	}
	r.mu.Unlock()
	return resolved, nil
}

// forwardPacket 解析目的 IP，找到对端流并经隧道发送。
func (r *Router) forwardPacket(ctx context.Context, packet []byte) error {
	destination, err := packetDestination(packet)
	if err != nil || destination == "" {
		return err
	}
	target, err := r.lookupPeer(destination)
	if err != nil {
		return err
	}

	var flow *peerFlow
	for {
		var err error
		flow, err = r.flowTo(ctx, target, destination)
		if err != nil {
			return err
		}
		r.mu.Lock()
		// 必须复核流仍然在位：拿流到加锁之间它可能已被对端的新流顶替。
		if r.flows[target] == flow && !flow.retired.Load() && !r.closed {
			flow.writes.Add(1)
			flow.touch()
			r.mu.Unlock()
			break
		}
		r.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	defer flow.writes.Add(-1)

	// 关闭/取消时 Reset 打断阻塞写；正常返回立刻撤销回调。
	resetFn := context.AfterFunc(ctx, func() { _ = flow.stream.Reset() })
	defer resetFn()

	flow.writeMu.Lock()
	flow.stream.SetWriteDeadline(time.Now().Add(outboundWriteDeadline))
	_, err = flow.stream.Write(packet)
	if err == nil {
		flow.touch()
	}
	flow.writeMu.Unlock()
	if err != nil {
		r.retireFlow(flow, "write failed")
		return fmt.Errorf("write to %s (peer %s): %w", destination, target.ShortString(), err)
	}
	return nil
}

// flowTo 返回到对端 target 的活跃流，没有则建立。
//
// 拨号按 PeerID single-flight 合并：同一 peer 的 IPv4 与 IPv6 包共用一次拨号，
// 并发调用只发起一次真实拨号，其余等待结果并复用同一条流，避免拨号风暴。
func (r *Router) flowTo(ctx context.Context, target peer.ID, destination string) (*peerFlow, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, context.Canceled
		}
		if flow, ok := r.flows[target]; ok && !flow.retired.Load() {
			r.mu.Unlock()
			return flow, nil
		}
		if call, ok := r.dialing[target]; ok {
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
		r.dialing[target] = call
		r.mu.Unlock()

		flow, err := r.dialStream(ctx, target, destination)
		r.mu.Lock()
		delete(r.dialing, target)
		r.mu.Unlock()
		call.err = err
		close(call.done)
		return flow, err
	}
}

// dialStream 真实拨号，并用对称仲裁把流装进流表。
func (r *Router) dialStream(ctx context.Context, target peer.ID, destination string) (*peerFlow, error) {
	stream, viaRelay, err := r.tunnel.OpenStreamToVirtualIP(ctx, destination)
	if err != nil {
		// 本次拨号尚未注册流；并发入向流可能已被注册，不能误删。
		return nil, err
	}
	// 以流实际连上的对端为准（NetMap 过期时也能归一到正确对端）。
	if remote := stream.Conn().RemotePeer(); remote != "" {
		target = remote
	}
	_, aliases, _ := r.tunnel.PeerRoute(destination)
	candidate := newPeerFlow(stream, target, r.selfID, aliases)
	kept, installed := r.installFlow(candidate)
	if !installed {
		return nil, fmt.Errorf("router flow rejected for peer %s", target.ShortString())
	}
	if kept != candidate {
		// 仲裁判给了在位的老流：复用它，新拨的流已被优雅关闭。
		return kept, nil
	}
	log.Printf("[router] tunnel established to %s peer=%s initiator=self via=%s remote=%s",
		destination, target.ShortString(), viaRelayLabel(viaRelay), stream.Conn().RemoteMultiaddr())
	return candidate, nil
}

func viaRelayLabel(viaRelay bool) string {
	if viaRelay {
		return "relay"
	}
	return "direct"
}

// ServeInboundStream 处理仅有 IPv4 地址的旧节点入向流。
func (r *Router) ServeInboundStream(virtualIP string, stream network.Stream) {
	r.ServeInboundStreamAliases(virtualIP, "", stream)
}

// ServeInboundStreamAliases 注册一个对端流：v4/v6 两个虚拟地址共用这一条流。
//
// 关键：入向与出向走同一套仲裁（installFlow）。两端各自独立算出的赢家必然
// 相同（比较的是发起方 PeerID，双方都知道双方是谁），因此不会再出现
// 「A 留下自己拨的流、B 留下自己拨的流、互相对拆」的秒级重建死循环。
func (r *Router) ServeInboundStreamAliases(v4, v6 string, stream network.Stream) {
	aliases := make([]string, 0, 2)
	if v4 != "" {
		aliases = append(aliases, v4)
	}
	if v6 != "" && v6 != v4 {
		aliases = append(aliases, v6)
	}
	remote := stream.Conn().RemotePeer()
	if len(aliases) == 0 || remote == "" {
		// 认不出来的流：Close 而非 Reset，避免对端打出误导性的
		// "stream reset by remote, error code 0" 并误判自己被踢。
		log.Printf("[router] inbound tunnel stream ignored: unknown peer=%s", remote.ShortString())
		_ = stream.Close()
		return
	}
	candidate := newPeerFlow(stream, remote, remote, aliases)
	kept, installed := r.installFlow(candidate)
	if !installed {
		return
	}
	if kept != candidate {
		return // 仲裁判给了在位的老流，本流已被优雅关闭，无需 pump。
	}
	log.Printf("[router] inbound tunnel established from %s peer=%s initiator=remote remote=%s",
		aliases[0], remote.ShortString(), stream.Conn().RemoteMultiaddr())
	r.pumps.Add(1)
	defer r.pumps.Done()
	r.pumpFromStream(candidate)
}

// writeInbound 把一个完整的 IP 包过防火墙后写回 TUN。
func (r *Router) writeInbound(bufs [][]byte, sizes []int, pkt []byte, writeOffset int) {
	n := len(pkt)
	// 统一入向防火墙：源虚拟 IP + 协议 + 目标端口，拒绝即丢包。
	if !CheckPacket(r.fw, pkt) {
		if r.fw != nil {
			src, proto, port := firewallPacketLogFields(pkt)
			dropLog(src, proto, port)
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

func (r *Router) pumpFromStream(flow *peerFlow) {
	defer r.retireFlow(flow, "closed by peer")
	writeOffset := packetWriteOffset()
	bufs := make([][]byte, 1)
	sizes := make([]int, 1)
	readBuf := make([]byte, maxPacketSize)
	framer := &ipFramer{}
	for {
		n, err := flow.stream.Read(readBuf)
		if err != nil {
			return
		}
		// 被仲裁淘汰后立即停止往 TUN 投递：老流与新流同时活着会让对端的包
		// 重复进入本机协议栈（一次会话出现两份相同的 TCP 包）。
		if flow.retired.Load() {
			return
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		flow.touch()
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

func newPeerFlow(stream network.Stream, target, initiator peer.ID, addrs []string) *peerFlow {
	flow := &peerFlow{stream: stream, peer: target, initiator: initiator}
	flow.lastActive.Store(time.Now().UnixNano())
	for _, addr := range addrs {
		if addr != "" {
			flow.addrs = append(flow.addrs, addr)
		}
	}
	return flow
}

// installFlow 按对称仲裁把 candidate 装进流表，返回留存的那条流。
//
// 仲裁规则：比较两条流的发起方 PeerID，字典序小的一方获胜。
// 出向流的发起方是本机，入向流的发起方是对端 —— 两端都同时知道这两个
// PeerID，因此各自独立算出的赢家必然相同。这是消除「互相对拆」死循环的
// 根本保证：任何一端都不可能留下一条被对端 Reset 掉的流。
//
// 发起方相同（对端重拨）时保留在位的老流。发起方自己不会并发拨两条流
// （flowTo 按 PeerID single-flight 合并），且它在重拨前已经 Reset 掉旧流，
// 对端读侧随即报 EOF 摘除旧流，通常不会走到这个平局分支；即便走到也是
// 收敛的（失败的一方 Close，重拨代价由 runOutbound 的退避兜住）。
//
// installed=false 表示 Router 已关闭或流数超限，调用方不要再使用该流。
func (r *Router) installFlow(candidate *peerFlow) (kept *peerFlow, installed bool) {
	r.mu.Lock()
	if r.closed || r.flows == nil {
		r.mu.Unlock()
		_ = candidate.stream.Close()
		return nil, false
	}
	existing := r.flows[candidate.peer]
	if existing == candidate {
		r.mergeAddrsLocked(existing, candidate.addrs)
		r.mu.Unlock()
		return candidate, true
	}
	if existing != nil && !candidateWins(existing, candidate) {
		// 败者也可能是别名更全的那一条（例如本机直连拨号时 NetMap 还没给出
		// 对端的 IPv6，稍后到达的入向流才补齐），先把它知道的别名并入存活流再退场。
		r.mergeAddrsLocked(existing, candidate.addrs)
		r.mu.Unlock()
		// 败者优雅退场：Close 让对端读到 EOF 干净退出，而不是收到
		// STREAM_RESET(code 0) 后重拨回来再次参与竞争。
		_ = candidate.stream.Close()
		r.scheduleForceReset(candidate)
		return existing, true
	}
	if existing == nil && len(r.flows) >= maxOutboundWorkers {
		r.mu.Unlock()
		_ = candidate.stream.Close()
		log.Printf("[router] 流数上限 %d 已满，拒绝 peer %s 的入向流",
			maxOutboundWorkers, candidate.peer.ShortString())
		return nil, false
	}
	r.flows[candidate.peer] = candidate
	// 先清掉该 peer 的全部旧别名，再挂上新流的别名，保证别名索引与流表一致。
	for addr, owner := range r.flowByAddr {
		if owner == candidate.peer {
			delete(r.flowByAddr, addr)
		}
	}
	// 别名取并集：新流知道的 ∪ 旧流知道的。入向流由 client.go 按 PeerID 查表拿到
	// v4+v6，通常更全；但先到的直连流可能只有 v4（拨号瞬间 NetMap 尚未下发 IPv6）。
	// 只保留新流自己的别名会让存活流丢掉 IPv6，指向该地址的下一个包又会再次拨号 ——
	// 那正是本次「同一 peer 两条流」故障的形态。
	r.mergeAddrsLocked(candidate, candidate.addrs)
	if existing != nil {
		r.mergeAddrsLocked(candidate, existing.addrs)
	}
	r.mu.Unlock()

	if existing != nil {
		existing.retired.Store(true)
		r.mu.Lock()
		if r.flows[candidate.peer] == existing {
			delete(r.flows, candidate.peer)
			for addr, owner := range r.flowByAddr {
				if owner == candidate.peer {
					delete(r.flowByAddr, addr)
				}
			}
		}
		r.mu.Unlock()
		log.Printf("[router] tunnel replaced for peer %s: 保留发起方 %s，淘汰发起方 %s",
			candidate.peer.ShortString(), initiatorLabel(candidate.initiator), initiatorLabel(existing.initiator))
		_ = existing.stream.Close()
		r.scheduleForceReset(existing)
	}
	return candidate, true
}

// mergeAddrsLocked 把 addrs 并入 flow 的别名集合（幂等），调用方必须持有 r.mu。
func (r *Router) mergeAddrsLocked(flow *peerFlow, addrs []string) {
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		// 已被占用就不抢占：别名只归一个 peer，避免索引指向错误的流。
		if _, taken := r.flowByAddr[addr]; taken {
			continue
		}
		r.flowByAddr[addr] = flow.peer
		flow.addrs = append(flow.addrs, addr)
	}
}

// candidateWins 报告 candidate 是否有权顶替 existing（两端算法一致）。
func candidateWins(existing, candidate *peerFlow) bool {
	if existing == nil {
		return true
	}
	if existing.peer != candidate.peer {
		return false // 不同对端从不竞争。
	}
	return candidate.initiator < existing.initiator
}

func initiatorLabel(id peer.ID) string {
	if id == "" {
		return "unknown"
	}
	return id.ShortString()
}

// scheduleForceReset 在宽限期后强制 Reset 一条已优雅关闭的流。
// Close 通常已经让读循环返回并释放资源；这里只是兜底，防止某些传输
// 实现上 Close 后仍占着底层缓冲。
func (r *Router) scheduleForceReset(flow *peerFlow) {
	grace := r.retireGrace
	if grace <= 0 {
		_ = flow.stream.Reset()
		return
	}
	timer := time.NewTimer(grace)
	go func() {
		defer timer.Stop()
		<-timer.C
		_ = flow.stream.Reset()
	}()
}

// retireFlow 把流移出流表并关闭它。idempotent：重复调用只有第一次生效，
// 避免并发路径下同一条流被反复 Reset 而把对端日志刷成一片。
func (r *Router) retireFlow(flow *peerFlow, reason string) {
	if flow == nil || !flow.retired.CompareAndSwap(false, true) {
		return
	}
	r.mu.Lock()
	if r.flows[flow.peer] == flow {
		delete(r.flows, flow.peer)
		for addr, owner := range r.flowByAddr {
			if owner == flow.peer {
				delete(r.flowByAddr, addr)
			}
		}
	}
	r.mu.Unlock()

	if r.closed {
		_ = flow.stream.Reset()
		return
	}
	log.Printf("[router] tunnel closed for peer %s (%s)", flow.peer.ShortString(), reason)
	_ = flow.stream.Close()
	r.scheduleForceReset(flow)
}

// 有用的小工具：把 IPv4 头中的协议字段取出来（TCP=6 UDP=17），调试用。
func protocolOf(packet []byte) byte {
	if len(packet) < 20 {
		return 0
	}
	return packet[9]
}

var _ = binary.BigEndian // 保留引用，后续分片/校验和扩展用
