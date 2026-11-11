package lanet

import (
	"context"
	"errors"
	"time"

	"github.com/ayflying/pvn/pkg/tunnel"
)

// ReportProbeResult 反馈完成的端到端echo，只采集该次真实流的路径。
// RTT必须只计echo往返、不含发现/拨号，避免把冷启动耗时误判为中转劣化。
// 本地生命周期取消不算链路坏；质量判断交由有界路径状态累计，单次慢包不切换。
func (c *Client) ReportProbeResult(stream Stream, rtt time.Duration, probeErr error) {
	raw, ok := stream.(streamAdapter)
	if !ok || raw.Stream == nil || c.tunnelSvc == nil || errors.Is(probeErr, context.Canceled) {
		return
	}
	isolated := false
	if probeErr != nil {
		// 协议拒绝、对端echo配额Reset、错误回显并不能证明中转故障。
		// 只把完整测量窗口超时或底层已关闭作为故障证据。
		if !errors.Is(probeErr, context.DeadlineExceeded) && !raw.Stream.Conn().IsClosed() {
			return
		}
		isolated = c.tunnelSvc.ReportPathFailure(raw.Stream, probeErr)
	} else {
		isolated = c.tunnelSvc.ReportPathQuality(raw.Stream, tunnel.PathQuality{Degraded: rtt > 2*time.Second, RTT: rtt})
	}
	if !isolated {
		return
	}
	// 只退役该目标当前匹配的TUN路径。不会关闭libp2p连接、其他协议流；
	// 后续数据触发有界重拨，属于保守断流重建而非无损迁移。
	c.tunMu.Lock()
	router := c.tunRouter
	c.tunMu.Unlock()
	if router != nil {
		router.InvalidatePath(raw.Stream.Conn().RemotePeer(), raw.Stream.Conn().ID())
	}
}
