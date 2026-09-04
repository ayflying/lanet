package p2pkit

import (
	"context"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

type HostSpec struct {
	ListenAddrs  []string
	RelayService bool
	RelaySource  autorelay.PeerSource
	UserAgent    string
}

// NewHost 创建 libp2p Host。
//
// 注意 AutoRelay 的行为边界：EnableAutoRelayWithPeerSource 只在节点
// 「认为自身不可达（private/NAT 后）」时才会向 relay 预约。公网可达的
// 节点不会预约，导致其他节点经中继找不到它（NO_RESERVATION）。
// 因此 agent 主流程在入网后还须调用 EnsureRelayReservation 主动预约，
// 保证「任意成员都能被经中继访问」这一兜底语义。
func NewHost(ctx context.Context, spec HostSpec) (host.Host, error) {
	options := []libp2p.Option{
		libp2p.UserAgent(spec.UserAgent),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
	}

	if len(spec.ListenAddrs) > 0 {
		options = append(options, libp2p.ListenAddrStrings(spec.ListenAddrs...))
	}
	if spec.RelayService {
		options = append(options,
			libp2p.ForceReachabilityPublic(),
			libp2p.EnableRelayService(relayv2.WithInfiniteLimits()),
		)
	}
	if spec.RelaySource != nil {
		options = append(options,
			libp2p.ForceReachabilityPrivate(),
			libp2p.EnableHolePunching(),
			libp2p.EnableAutoRelayWithPeerSource(spec.RelaySource),
		)
	}

	h, err := libp2p.New(options...)
	if err != nil {
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}
	return h, nil
}

func AddrInfo(h host.Host) peer.AddrInfo {
	return peer.AddrInfo{
		ID:    h.ID(),
		Addrs: h.Addrs(),
	}
}

// EnsureRelayReservation 向候选中继逐个发起预约，成功一次即返回。
// 用途：agent 入网后主动在 relay 上留下预约（Reservation），
// 使任意其他成员都能经中继访问本节点——AutoRelay 只在节点
// 自认不可达时才预约，公网可达节点必须靠这里兜底。
// source 为 nil 或全部候选预约失败时返回错误（不阻塞主流程，可周期重试）。
func EnsureRelayReservation(ctx context.Context, h host.Host, source autorelay.PeerSource, number int) error {
	if source == nil {
		return fmt.Errorf("nil relay source")
	}
	if number < 1 {
		number = 2
	}
	for candidate := range source(ctx, number) {
		reserveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := relayclient.Reserve(reserveCtx, h, candidate)
		cancel()
		if err == nil {
			return nil
		}
		// 记录但不中断：继续尝试下一个候选。
	}
	return fmt.Errorf("no relay reservation succeeded")
}
