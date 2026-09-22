// 调度线程上限：限制节点进程的 OS 线程/CPU 占用，不影响网络数据面。
//
// 为什么需要：容器部署若未配 CPU 限额，GOMAXPROCS 默认按宿主核数开满——
// 阿里云 16/32 核宿主上节点会表现出几十个调度线程、空闲也吃 CPU 排班。
// Go 的网络 IO（libp2p/QUIC/TUN/转发）全部走 epoll/kqueue poller，
// goroutine 阻塞在 poller 上不占线程，因此收紧 GOMAXPROCS 不会降低
// 转发/隧道吞吐，只会减少「同时执行 Go 代码」的线程排班。
//
// 生效优先级：LANET_MAX_PROCS / -maxprocs 显式配置 > 容器 CPU 配额
// （Go 1.25 起原生读取 cgroup，配了 cpus 的容器自动生效）> 默认
// min(核数, 4)。
package main

import (
	"log"
	"runtime"
)

// maxProcsDefaultCap 无 CPU 配额宿主上的默认调度线程上限。
// P2P 节点的流量面（加密、转发、TUN 读写）是 epoll 驱动的，4 个调度
// 线程足以线速跑满常见链路；取 4 换取更安静的 CPU/线程占用。
const maxProcsDefaultCap = 4

// applyMaxProcs 应用调度线程上限，返回实际生效值（仅用于日志/测试）。
//
//   - n > 0：显式设置（超过核数时收敛到核数）；
//   - n <= 0：自动——当前 GOMAXPROCS 已低于核数（容器 CPU 配额或
//     GOMAXPROCS 环境变量已生效）时尊重现状不干预；否则 cap 到
//     min(核数, maxProcsDefaultCap)。
func applyMaxProcs(n int) int {
	cores := runtime.NumCPU()
	if n > 0 {
		if n > cores {
			n = cores
		}
		previous := runtime.GOMAXPROCS(n)
		log.Printf("[node] 调度线程上限 GOMAXPROCS=%d（显式配置，原值 %d，核数 %d）", n, previous, cores)
		return n
	}
	current := runtime.GOMAXPROCS(0)
	if current < cores {
		// Go 1.25 起默认感知 cgroup CPU 配额；GOMAXPROCS 低于核数说明
		// 配额或环境变量已在约束，尊重现状。
		log.Printf("[node] 调度线程 GOMAXPROCS=%d（已受 CPU 配额/环境变量约束，不调整，核数 %d）", current, cores)
		return current
	}
	if current <= maxProcsDefaultCap {
		return current
	}
	runtime.GOMAXPROCS(maxProcsDefaultCap)
	log.Printf("[node] 调度线程上限 GOMAXPROCS=%d（自动：min(核数 %d, %d)；LANET_MAX_PROCS 可覆盖）",
		maxProcsDefaultCap, cores, maxProcsDefaultCap)
	return maxProcsDefaultCap
}
