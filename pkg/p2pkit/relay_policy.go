package p2pkit

import relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"

// nodeRelayOptions 让普通节点的中继可承载长期隧道，而不只是短时打洞。
// libp2p 默认每条 circuit 连接仅存活两分钟、每方向128KiB，应用流即使
// 刚建立也可能复用即将耗尽的连接，造成周期断线和秒级重建。
// 这里只移除单连接时长/字节上限，不放开默认预约数量、每IP预约数量、
// circuit并发和底层资源管理器限制；两种普通中继启动入口统一复用。
func nodeRelayOptions() []relayv2.Option {
	return []relayv2.Option{relayv2.WithLimit(nil)}
}
