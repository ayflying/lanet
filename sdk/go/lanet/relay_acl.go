package lanet

import (
	"sync/atomic"

	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// relayGroupACL 在 host 创建前安装，Discovery 发布前拒绝所有预约/转发。
// 原子发布避免网络回调与启动赋值竞争；只校验独立同组证明，不改好友策略。
// 流量、并发和请求频率由 p2pkit 的外层硬预算统一限制。
type relayGroupACL struct {
	discovery atomic.Pointer[serverless.Discovery]
}

func (a *relayGroupACL) AllowReserve(id peer.ID, _ ma.Multiaddr) bool {
	return a.discovery.Load().CanRelayPeer(id)
}
func (a *relayGroupACL) AllowConnect(src peer.ID, _ ma.Multiaddr, dst peer.ID) bool {
	d := a.discovery.Load()
	return d.CanRelayPeer(src) && d.CanRelayPeer(dst)
}
