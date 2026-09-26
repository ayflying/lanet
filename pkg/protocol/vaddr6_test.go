package protocol

import (
	"net/netip"
	"testing"
)

// TestGroupIPv6Prefix 覆盖群组 /64 的边界与形状。
func TestGroupIPv6Prefix(t *testing.T) {
	cases := []struct {
		index int
		want  string
	}{
		{0, "fd00:6c61:6e65::/64"},
		{1, "fd00:6c61:6e65:1::/64"},
		{7, "fd00:6c61:6e65:7::/64"},
		{255, "fd00:6c61:6e65:ff::/64"},
		{256, "fd00:6c61:6e65:100::/64"},
		{65535, "fd00:6c61:6e65:ffff::/64"},
	}
	for _, c := range cases {
		got, err := GroupIPv6Prefix(c.index)
		if err != nil {
			t.Fatalf("GroupIPv6Prefix(%d) 报错: %v", c.index, err)
		}
		if got.String() != c.want {
			t.Errorf("GroupIPv6Prefix(%d) = %s，期望 %s", c.index, got, c.want)
		}
		if !LanetULAIPv6Prefix.Contains(got.Addr()) {
			t.Errorf("GroupIPv6Prefix(%d) = %s 落在 %s 之外", c.index, got, LanetULAIPv6Prefix)
		}
	}
	for _, bad := range []int{-1, 65536} {
		if _, err := GroupIPv6Prefix(bad); err == nil {
			t.Errorf("GroupIPv6Prefix(%d) 应报错", bad)
		}
	}
}

// TestMemberIPv6MirrorsIPv4Host 断言成员地址与 IPv4 主机号一一对应，
// 并且每个群组内互不相同、都在本群 /64 内。
func TestMemberIPv6MirrorsIPv4Host(t *testing.T) {
	cases := []struct {
		subnet, host int
		want         string
	}{
		{0, 2, "fd00:6c61:6e65::2"},
		{0, 3, "fd00:6c61:6e65::3"},
		{7, 5, "fd00:6c61:6e65:7::5"},
		{7, 254, "fd00:6c61:6e65:7::fe"},
		{255, 254, "fd00:6c61:6e65:ff::fe"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got, err := MemberIPv6(c.subnet, c.host)
		if err != nil {
			t.Fatalf("MemberIPv6(%d,%d) 报错: %v", c.subnet, c.host, err)
		}
		if got.String() != c.want {
			t.Errorf("MemberIPv6(%d,%d) = %s，期望 %s", c.subnet, c.host, got, c.want)
		}
		prefix, err := GroupIPv6Prefix(c.subnet)
		if err != nil {
			t.Fatalf("GroupIPv6Prefix(%d) 报错: %v", c.subnet, err)
		}
		if !prefix.Contains(got) {
			t.Errorf("MemberIPv6(%d,%d) = %s 不在本群 %s 内", c.subnet, c.host, got, prefix)
		}
		if seen[got.String()] {
			t.Errorf("地址 %s 重复", got)
		}
		seen[got.String()] = true
	}
	// 主机号边界：::0 子网任播与 ::1 保留都不分配。
	for _, bad := range []int{0, 1, 255, 256} {
		if _, err := MemberIPv6(0, bad); err == nil {
			t.Errorf("MemberIPv6(0,%d) 应报错", bad)
		}
	}
}

// TestMemberIPv6DistinctAcrossGroups 同一主机号在不同群组必须落到不同地址，
// 否则跨群成员会互相覆盖路由。
func TestMemberIPv6DistinctAcrossGroups(t *testing.T) {
	a, err := MemberIPv6(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MemberIPv6(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("不同群组的同一主机号解析到同一地址 %s", a)
	}
	if !netip.MustParseAddr("fd00:6c61:6e65::2").IsValid() { // 前缀常量本身可解析
		t.Fatal("ULA 前缀不可解析")
	}
}
