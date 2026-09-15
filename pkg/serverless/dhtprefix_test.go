package serverless

import (
	"testing"

	lproto "github.com/ayflying/pvn/pkg/protocol"
)

// TestPrivateDHTPrefixSharedByDefault 私有 DHT 默认落在全网共享前缀上。
//
// 这是 0.5.48 的核心行为变化：不同网络密钥的节点必须使用同一个 DHT 协议前缀，
// 否则各群一张孤立路由表，「路由表里只有自己人 + 唯一种子一挂」就等于全群失联
// （0.5.34~0.5.47 的老行为）。共享前缀让全网节点互相路由，跨群冷启动不再押在
// 单个种子上——而隐私边界仍由 provider key 与控制面协议 ID 承担。
func TestPrivateDHTPrefixSharedByDefault(t *testing.T) {
	ka := GroupKey(ChannelOfficial, "group-a")
	kb := GroupKey(ChannelSDK, "group-b")
	if ka == nil || kb == nil {
		t.Fatal("GroupKey 不应返回 nil")
	}

	pa, pb := privateDHTPrefix(false, ka), privateDHTPrefix(false, kb)
	if pa != pb {
		t.Fatalf("默认前缀必须全网一致（共享一张私有 DHT），实际 %q != %q", pa, pb)
	}
	if pa != PrivateDHTPrefix {
		t.Fatalf("默认前缀应为 %q，实际 %q", PrivateDHTPrefix, pa)
	}
	if len(pa) == 0 || pa[0] != '/' {
		t.Fatalf("前缀必须落在合法多链接命名空间（以 / 开头）: %q", pa)
	}
	// 与公共 IPFS DHT 必须隔离，否则共享的就变成公网那张（空载上行 ~4MB/分钟）。
	if pa == "/ipfs" || len(pa) >= 5 && pa[:5] == "/ipfs" {
		t.Fatalf("私有 DHT 前缀不得落在公共 /ipfs 上: %q", pa)
	}
}

// TestPrivateDHTPrefixLegacyGroupScoped LegacyGroupDHT 过渡开关退回按群派生前缀。
//
// 迁移期网络里还有 0.5.34~0.5.47 的老节点，它们的私有 DHT 是按群派生的；
// 打开该开关即回到同一命名空间，靠 DHT（而非种子）也能找到它们。
// 三点必须成立：同群稳定可复现、异群互不相同、且与共享前缀确实不同（否则开关是空操作）。
func TestPrivateDHTPrefixLegacyGroupScoped(t *testing.T) {
	ka := GroupKey(ChannelOfficial, "group-a")
	kb := GroupKey(ChannelOfficial, "group-b")

	pa := privateDHTPrefix(true, ka)
	if pa != privateDHTPrefix(true, ka) {
		t.Fatalf("过渡开关下同群前缀必须稳定可复现: %q", pa)
	}
	if pa == privateDHTPrefix(true, kb) {
		t.Fatal("过渡开关下异群必须得到不同前缀（等价于每群一张独立 DHT）")
	}
	if want := lproto.DHTPrefixFor(ka); pa != want {
		t.Fatalf("过渡开关应等于 protocol.DHTPrefixFor：实际 %q want %q", pa, want)
	}
	if pa == PrivateDHTPrefix {
		t.Fatalf("过渡开关的前缀不应等于共享前缀 %q，否则开关无效果", PrivateDHTPrefix)
	}
}
