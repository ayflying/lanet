package p2pkit

import (
	"context"
	"fmt"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

type HostSpec struct {
	ListenAddrs  []string
	RelayService bool
	RelaySource  autorelay.PeerSource
	UserAgent    string
}

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
