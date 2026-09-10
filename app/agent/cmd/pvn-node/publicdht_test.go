package main

import (
	"testing"

	"github.com/ayflying/pvn/pkg/netmapclient"
)

// TestResolvePublicDHT 覆盖参数优先级矩阵：显式命令行/环境变量 > 配置文件。
// 回归背景：早期实现是 `*publicDHT || nc.EnablePublicDHT`（flag.Bool），
// 命令行一旦含 -public-dht 就恒为 true，且无法显式关闭配置文件里的 true。
func TestResolvePublicDHT(t *testing.T) {
	cases := []struct {
		name    string
		flagVal string
		cfgVal  bool
		want    bool
	}{
		// 未传（哨兵）：一律采用配置文件值
		{"未传+配置false", "@@unset@@", false, false},
		{"未传+配置true", "@@unset@@", true, true},
		// 显式开启 true：覆盖配置为 false
		{"传true+配置false", "true", false, true},
		{"传1+配置false", "1", false, true},
		{"传TRUE大小写+配置false", "TRUE", false, true},
		{"传True大小写+配置false", "True", false, true},
		// 显式关闭 false：覆盖配置为 true（旧实现在此必然失败）
		{"传false+配置true", "false", true, false},
		{"传0+配置true", "0", true, false},
		{"传FALSE大小写+配置true", "FALSE", true, false},
		// 空字符串视为显式关闭（不是未传）
		{"传空串+配置true", "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvePublicDHT(c.flagVal, c.cfgVal); got != c.want {
				t.Fatalf("resolvePublicDHT(%q, %v) = %v, want %v",
					c.flagVal, c.cfgVal, got, c.want)
			}
		})
	}
}

// TestShouldProbe 覆盖探测过滤：跳过自己、无虚拟 IP、以及「幽灵成员」。
// 回归背景：probe 循环曾对成员表内每个成员无条件拨号，导致已下线节点
// （仅有 DHT 陈旧记录、从未握手成功）被每轮反复探测，刷日志且白耗流量。
func TestShouldProbe(t *testing.T) {
	const selfIP = "10.7.243.173"
	cases := []struct {
		name string
		self string
		m    netmapclient.Member
		want bool
	}{
		{
			name: "自己（虚拟 IP 相同）",
			self: selfIP,
			m:    netmapclient.Member{VirtualIP: selfIP, Version: "0.5.19", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			want: false,
		},
		{
			name: "虚拟 IP 缺失",
			self: selfIP,
			m:    netmapclient.Member{Version: "0.5.19", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			want: false,
		},
		{
			name: "幽灵成员：未握手且无地址",
			self: selfIP,
			m:    netmapclient.Member{VirtualIP: "10.7.174.223"},
			want: false,
		},
		{
			name: "已握手（有 Version）无地址：仍探测",
			self: selfIP,
			m:    netmapclient.Member{VirtualIP: "10.7.72.116", Version: "0.5.17"},
			want: true,
		},
		{
			name: "有地址但未握手：仍探测（可能刚上线）",
			self: selfIP,
			m:    netmapclient.Member{VirtualIP: "10.7.9.215", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			want: true,
		},
		{
			name: "正常在线成员",
			self: selfIP,
			m:    netmapclient.Member{VirtualIP: "10.7.72.116", Version: "0.5.17", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldProbe(c.self, c.m); got != c.want {
				t.Fatalf("shouldProbe(%q, %+v) = %v, want %v", c.self, c.m, got, c.want)
			}
		})
	}
}
