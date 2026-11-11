package serverless

import (
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// CanRelayPeer 只授权底层中继协作，不授予好友、虚拟路由或业务协议权限。
// 必须是本机直接挑战验组成功且尚未过期的身份；第三方线索、种子记录、
// peerstore 地址以及好友记录本身都不能替代证明。底层另有独立请求/资源预算。
func (d *Discovery) CanRelayPeer(id peer.ID) bool {
	if d == nil || id == "" || d.serviceCtx == nil || d.serviceCtx.Err() != nil {
		return false
	}
	return d.control.verified(id, time.Now())
}

func (c *controlState) verified(id peer.ID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.peers[id]
	if !ok {
		return false
	}
	if !now.Before(e.expires) {
		delete(c.peers, id)
		return false
	}
	return true
}
