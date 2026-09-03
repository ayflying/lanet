package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/peer"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	relay, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs:  []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
		RelayService: true,
		UserAgent:    "pvn-relay-check-relay/0.1.0",
	})
	if err != nil {
		log.Fatalf("start local relay: %v", err)
	}
	defer relay.Close()

	agent, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
		UserAgent:   "pvn-relay-check-agent/0.1.0",
	})
	if err != nil {
		log.Fatalf("start local agent: %v", err)
	}
	defer agent.Close()

	relayInfo := p2pkit.AddrInfo(relay)
	if err = agent.Connect(ctx, relayInfo); err != nil {
		log.Fatalf("connect to relay: %v", err)
	}

	reservation, err := relayclient.Reserve(ctx, agent, relayInfo)
	if err != nil {
		log.Fatalf("reserve relay circuit: %v", err)
	}

	circuitAddr, err := relayCircuitAddress(relayInfo, agent.ID())
	if err != nil {
		log.Fatalf("create circuit address: %v", err)
	}

	fmt.Printf("reservation-ok relay=%s expires=%s limit_duration=%s limit_data=%d\n", relay.ID(), reservation.Expiration.Format(time.RFC3339), reservation.LimitDuration, reservation.LimitData)
	fmt.Printf("relay-address=%s\n", circuitAddr)
}

func relayCircuitAddress(relay peer.AddrInfo, destination peer.ID) (ma.Multiaddr, error) {
	if len(relay.Addrs) == 0 {
		return nil, fmt.Errorf("relay has no listen address")
	}

	address := relay.Addrs[0]
	peerComponent, err := ma.NewMultiaddr("/p2p/" + relay.ID.String())
	if err != nil {
		return nil, fmt.Errorf("create relay peer component: %w", err)
	}
	circuitComponent, err := ma.NewMultiaddr("/p2p-circuit/p2p/" + destination.String())
	if err != nil {
		return nil, fmt.Errorf("create circuit component: %w", err)
	}
	return address.Encapsulate(peerComponent).Encapsulate(circuitComponent), nil
}
