package serverless

import (
	"testing"
)

func TestDiscoveryResolveDualStackAndAmbiguity(t *testing.T) {
	members := map[string]*Member{
		"peer-a": {
			PeerID: "peer-a", VirtualIP: "10.7.1.2", VirtualIPv6: "fd00:6c61:6e65:1::2",
			Addrs: []string{"/ip4/192.0.2.2/tcp/4001"},
		},
		"peer-b": {
			PeerID: "peer-b", VirtualIP: "10.7.1.3", VirtualIPv6: "fd00:6c61:6e65:1::3",
		},
	}
	d := &Discovery{members: members}

	for _, tc := range []struct {
		target string
		want   string
	}{
		{target: "10.7.1.2", want: "peer-a"},
		{target: "fd00:6c61:6e65:1::2", want: "peer-a"},
	} {
		route, ok := d.Resolve(tc.target)
		if !ok || route.PeerID != tc.want {
			t.Fatalf("Resolve(%q) = (%+v, %v), want peer %q", tc.target, route, ok, tc.want)
		}
		if route.VirtualIP != "10.7.1.2" || route.VirtualIPv6 != "fd00:6c61:6e65:1::2" {
			t.Fatalf("Resolve(%q) returned incomplete dual-stack route: %+v", tc.target, route)
		}
	}

	members["peer-b"].VirtualIPv6 = members["peer-a"].VirtualIPv6
	if route, ok := d.Resolve("fd00:6c61:6e65:1::2"); ok {
		t.Fatalf("ambiguous IPv6 address resolved to %+v, want no route", route)
	}
}
