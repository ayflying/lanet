package cmd

import (
	"context"
	"flag"
	"log"

	"github.com/ayflying/pvn/pkg/p2pkit"
)

func runRelay(ctx context.Context) {
	listenAddr := flag.String("listen", "/ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1", "comma-separated libp2p listen addresses")
	flag.Parse()

	host, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs:  splitAddresses(*listenAddr),
		RelayService: true,
		UserAgent:    "pvn-relay/0.1.0",
	})
	if err != nil {
		log.Fatalf("start relay: %v", err)
	}
	defer host.Close()

	log.Printf("PVN relay started: peer_id=%s", host.ID())
	for _, addr := range host.Addrs() {
		log.Printf("listen=%s/p2p/%s", addr, host.ID())
	}

	<-ctx.Done()
	log.Print("PVN relay stopped")
}

func splitAddresses(value string) []string {
	var addresses []string
	var start int
	for index, char := range value {
		if char == ',' {
			if index > start {
				addresses = append(addresses, value[start:index])
			}
			start = index + 1
		}
	}
	if start < len(value) {
		addresses = append(addresses, value[start:])
	}
	return addresses
}
