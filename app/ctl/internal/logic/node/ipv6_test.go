package node

import (
	"context"
	"net/netip"
	"testing"
)

const testGroupCIDR6 = "fd00:6c61:6e65:a::/64"

func newTestRegistry(t *testing.T, cidr, cidr6 string) *Registry {
	t.Helper()
	registry, err := NewRegistry(cidr, cidr6, []string{"valid-token"})
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	return registry
}

// TestEnrollAllocatesPairedIPv6 IPv6 主机号必须与 IPv4 一致，且落在本群 /64 内。
func TestEnrollAllocatesPairedIPv6(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t, "10.7.10.0/24", testGroupCIDR6)

	first, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-a", Name: "a"})
	if err != nil {
		t.Fatalf("enroll peer-a: %v", err)
	}
	if first.VirtualIP != "10.7.10.2" || first.VirtualIPv6 != "fd00:6c61:6e65:a::2" {
		t.Fatalf("首个成员地址 = %s / %s，期望 10.7.10.2 / fd00:6c61:6e65:a::2", first.VirtualIP, first.VirtualIPv6)
	}
	second, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-b", Name: "b"})
	if err != nil {
		t.Fatalf("enroll peer-b: %v", err)
	}
	if second.VirtualIPv6 != "fd00:6c61:6e65:a::3" {
		t.Fatalf("第二个成员 IPv6 = %s，期望 fd00:6c61:6e65:a::3", second.VirtualIPv6)
	}
	prefix := netip.MustParsePrefix(testGroupCIDR6)
	for _, node := range registry.List(ctx) {
		addr, err := netip.ParseAddr(node.VirtualIPv6)
		if err != nil {
			t.Fatalf("解析 %s: %v", node.VirtualIPv6, err)
		}
		if !prefix.Contains(addr) {
			t.Errorf("成员 %s 的 IPv6 %s 不在本群 %s 内", node.PeerID, addr, prefix)
		}
	}
}

// TestRestoreNodeBackfillsMissingIPv6 老库（只有 virtual_ip）恢复时按主机号补齐；
// 已带 IPv6 的按原值恢复；越界或撞车必须报错而不是静默改写。
func TestRestoreNodeBackfillsMissingIPv6(t *testing.T) {
	registry := newTestRegistry(t, "10.7.7.0/24", "fd00:6c61:6e65:7::/64")

	if err := registry.RestoreNode(Node{PeerID: "peer-old", Name: "old", VirtualIP: "10.7.7.2"}); err != nil {
		t.Fatalf("恢复无 IPv6 的老成员: %v", err)
	}
	got := registry.List(context.Background())
	if len(got) != 1 || got[0].VirtualIPv6 != "fd00:6c61:6e65:7::2" {
		t.Fatalf("老成员补齐结果 = %+v，期望 fd00:6c61:6e65:7::2", got)
	}

	if err := registry.RestoreNode(Node{PeerID: "peer-new", Name: "new", VirtualIP: "10.7.7.9",
		VirtualIPv6: "fd00:6c61:6e65:7::9"}); err != nil {
		t.Fatalf("恢复带 IPv6 的成员: %v", err)
	}
	// 越界（别的群组 /64）
	if err := registry.RestoreNode(Node{PeerID: "peer-x", Name: "x", VirtualIP: "10.7.7.11",
		VirtualIPv6: "fd00:6c61:6e65:8::11"}); err == nil {
		t.Fatal("群组外的 IPv6 应被拒绝")
	}
	// 撞车（同一 IPv6 分给两个人）
	if err := registry.RestoreNode(Node{PeerID: "peer-y", Name: "y", VirtualIP: "10.7.7.12",
		VirtualIPv6: "fd00:6c61:6e65:7::9"}); err == nil {
		t.Fatal("重复的 IPv6 应被拒绝")
	}
	// 重复恢复同一 PeerID 是幂等的
	if err := registry.RestoreNode(Node{PeerID: "peer-old", Name: "old", VirtualIP: "10.7.7.2"}); err != nil {
		t.Fatalf("重复恢复应幂等: %v", err)
	}
}

// TestRemoveNodeReclaimsIPv6Pair 踢人后 IPv4 与 IPv6 必须一起回收，下一个成员复用同一对地址。
func TestRemoveNodeReclaimsIPv6Pair(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t, "10.7.3.0/24", "fd00:6c61:6e65:3::/64")

	first, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-a", Name: "a"})
	if err != nil {
		t.Fatalf("enroll peer-a: %v", err)
	}
	if _, err = registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-b", Name: "b"}); err != nil {
		t.Fatalf("enroll peer-b: %v", err)
	}
	removed, err := registry.RemoveNode("peer-a")
	if err != nil {
		t.Fatalf("remove peer-a: %v", err)
	}
	if removed.VirtualIPv6 != first.VirtualIPv6 {
		t.Fatalf("回收的 IPv6 = %s，期望 %s", removed.VirtualIPv6, first.VirtualIPv6)
	}
	third, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-c", Name: "c"})
	if err != nil {
		t.Fatalf("enroll peer-c: %v", err)
	}
	if third.VirtualIP != first.VirtualIP || third.VirtualIPv6 != first.VirtualIPv6 {
		t.Fatalf("新成员地址 = %s / %s，期望复用 %s / %s",
			third.VirtualIP, third.VirtualIPv6, first.VirtualIP, first.VirtualIPv6)
	}
}

// TestNewRegistryRejectsBadIPv6Prefix 只接受 /64 的 IPv6 前缀。
func TestNewRegistryRejectsBadIPv6Prefix(t *testing.T) {
	for _, bad := range []string{"fd00:6c61:6e65::/48", "10.7.10.0/24", "", "not-a-prefix"} {
		if _, err := NewRegistry("10.7.10.0/24", bad, []string{"token"}); err == nil {
			t.Errorf("IPv6 前缀 %q 应被拒绝", bad)
		}
	}
}

// TestEnrollPoolExhaustionKeepsPoolsInSync 两个池必须同进同退：
// 253 人可入网且地址互不相同，第 254 人报池耗尽（不是静默分配半个地址）。
func TestEnrollPoolExhaustionKeepsPoolsInSync(t *testing.T) {
	ctx := context.Background()
	registry := newTestRegistry(t, "10.7.20.0/24", "fd00:6c61:6e65:14::/64")

	seen := make(map[string]bool, 253)
	for i := 0; i < 253; i++ {
		peerID := "peer-" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + string(rune('0'+i%10))
		node, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: peerID, Name: peerID})
		if err != nil {
			t.Fatalf("第 %d 个成员入网失败: %v", i+1, err)
		}
		if node.VirtualIPv6 == "" {
			t.Fatalf("第 %d 个成员没有 IPv6", i+1)
		}
		if seen[node.VirtualIPv6] {
			t.Fatalf("第 %d 个成员 IPv6 重复: %s", i+1, node.VirtualIPv6)
		}
		seen[node.VirtualIPv6] = true
	}
	if _, err := registry.Enroll(ctx, EnrollRequest{Token: "valid-token", PeerID: "peer-overflow", Name: "x"}); err == nil {
		t.Fatal("池耗尽后仍分配成功")
	}
}
