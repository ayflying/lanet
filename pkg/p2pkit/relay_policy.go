package p2pkit

import (
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

const (
	// Ten minutes / 64 MiB per direction permits real tunnels, but never an
	// unbounded circuit. Reconnects also consume admission quotas (see below).
	relayCircuitDuration       = 10 * time.Minute
	relayCircuitBytes    int64 = 64 << 20
)

func relayResources() relayv2.Resources {
	return relayv2.Resources{
		Limit:           &relayv2.RelayLimit{Duration: relayCircuitDuration, Data: relayCircuitBytes},
		ReservationTTL:  20 * time.Minute,
		MaxReservations: 128, MaxCircuits: 4, BufferSize: 2048,
		MaxReservationsPerPeer: 1, MaxReservationsPerIP: 8, MaxReservationsPerASN: 32,
	}
}

// All relay entry points use the same finite resources and admission wrapper,
// including dedicated relays. A nil caller ACL retains the low-level API's open
// membership semantics, NOT unlimited resource semantics.
func nodeRelayOptions(acl relayv2.ACLFilter) []relayv2.Option {
	return []relayv2.Option{
		relayv2.WithResources(relayResources()),
		relayv2.WithACL(newRelayAdmission(acl)),
	}
}

// Resource limits must not expand with the host machine's RAM/FD count. Keep
// protocol/service defaults at the fixed minimal libp2p scale and impose hard
// host/peer connection and stream ceilings, also before relay ACL processing.
func relayResourceManager() (network.ResourceManager, error) {
	defaults := rcmgr.DefaultLimits
	limits := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{Conns: 256, ConnsInbound: 192, ConnsOutbound: 192,
			Streams: 1024, StreamsInbound: 768, StreamsOutbound: 768, FD: 256, Memory: 128 << 20},
		Transient: rcmgr.ResourceLimits{Conns: 64, ConnsInbound: 48, ConnsOutbound: 48,
			Streams: 128, StreamsInbound: 96, StreamsOutbound: 96, FD: 64, Memory: 16 << 20},
		PeerDefault: rcmgr.ResourceLimits{Conns: 8, ConnsInbound: 8, ConnsOutbound: 8,
			Streams: 64, StreamsInbound: 48, StreamsOutbound: 48, FD: 8, Memory: 16 << 20},
	}.Build(defaults.Scale(0, 0))
	return rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(limits))
}
