package netmapclient

import (
	"reflect"
	"testing"
)

// TestFilterAnnounceAddrs 通告地址清洗：脏地址剔除、好地址保留、畸形原样带过。
func TestFilterAnnounceAddrs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "drops-loopback-unspecified-overlay",
			in: []string{
				"/ip4/127.0.0.1/tcp/4001",      // 对端拨它=拨自己
				"/ip4/0.0.0.0/tcp/4001",        // 监听通配，不是可拨地址
				"/ip4/10.7.207.102/tcp/4001",   // lanet overlay，经自身隧道成环
				"/ip4/192.168.50.170/tcp/4001", // 真实局域网地址，保留
			},
			want: []string{"/ip4/192.168.50.170/tcp/4001"},
		},
		{
			name: "keeps-public-and-sorts-first",
			in: []string{
				"/ip4/192.168.50.170/tcp/4001",
				"/ip6/2408:824e::99/tcp/4001", // 公网 IPv6 应排在私有 IPv4 前
			},
			want: []string{"/ip6/2408:824e::99/tcp/4001", "/ip4/192.168.50.170/tcp/4001"},
		},
		{
			name: "dedupes-and-collapses",
			in: []string{
				"/ip4/192.168.50.170/tcp/4001",
				"/ip4/192.168.50.170/tcp/4001",         // 完全重复
				"/ip4/192.168.50.171/tcp/4001",         // 同一 /24 + 同一传输：折叠掉
				"/ip4/192.168.50.172/udp/4001/quic-v1", // 同链路不同传输：保留
			},
			want: []string{"/ip4/192.168.50.170/tcp/4001", "/ip4/192.168.50.172/udp/4001/quic-v1"},
		},
		{
			name: "unparsable-passed-through",
			in:   []string{"not-a-multiaddr", "/ip4/192.168.50.170/tcp/4001"},
			want: []string{"/ip4/192.168.50.170/tcp/4001", "not-a-multiaddr"},
		},
		{
			name: "blank-dropped",
			in:   []string{"", "  ", "/ip4/192.168.50.170/tcp/4001"},
			want: []string{"/ip4/192.168.50.170/tcp/4001"},
		},
		{
			name: "all-junk-yields-empty",
			in:   []string{"/ip4/127.0.0.1/tcp/4001", "/ip4/0.0.0.0/tcp/4001"},
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FilterAnnounceAddrs(c.in)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("want %v，got %v", c.want, got)
			}
		})
	}
}
