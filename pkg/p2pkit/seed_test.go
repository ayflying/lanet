package p2pkit

import (
	"net"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// testSeedPeerID 用真实 base58 节点 ID：multiaddr 解析会校验 p2p 段，
// 占位串（如 12D3KooWtest）会直接解析失败，测不出想测的分支。
const testSeedPeerID = "12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf"

// TestValidateSeedSpec 覆盖「连接种子只接受地址或留空」这条规则。
// 回归背景：控制台曾接受 public / none 字面量，用户以为 public 等于挂上公共
// 网络，实际公共 DHT 关闭时整条被丢弃，节点静默只走私有 DHT + mDNS。
func TestValidateSeedSpec(t *testing.T) {
	seed := "/ip4/43.136.124.167/udp/4001/quic-v1/p2p/" + testSeedPeerID
	seedTCP := "/ip4/43.136.124.167/tcp/4001/p2p/" + testSeedPeerID
	cases := []struct {
		name   string
		spec   string
		wantOK bool
	}{
		{"留空", "", true},
		{"只有空白", "   ", true},
		{"单个 quic 种子", seed, true},
		{"两端带空格", "  " + seed + "  ", true},
		{"逗号分隔两个种子", seedTCP + "," + seed, true},
		{"逗号分隔含空项", seedTCP + " , ," + seed, true},
		{"dnsaddr 豁免 p2p 要求", "/dnsaddr/bootstrap.libp2p.io", true},
		{"历史字面量 none", "none", false},
		{"历史字面量 public", "public", false},
		{"混在列表里的 public", seedTCP + ",public", false},
		{"缺 /p2p 组件", "/ip4/43.136.124.167/tcp/4001", false},
		{"裸节点 ID", testSeedPeerID, false},
		{"裸 IP 端口", "43.136.124.167:4001", false},
		{"中文垃圾输入", "公共节点", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSeedSpec(c.spec)
			if c.wantOK && err != nil {
				t.Fatalf("ValidateSeedSpec(%q) = %v, want nil", c.spec, err)
			}
			if !c.wantOK && err == nil {
				t.Fatalf("ValidateSeedSpec(%q) = nil, want error", c.spec)
			}
		})
	}
}

// TestValidateSeedSpecMessages 校验失败文案要能指导用户改：
// public/none 必须指向「留空 = 用内置入口」，缺 /p2p 必须说清缺什么。
func TestValidateSeedSpecMessages(t *testing.T) {
	cases := []struct {
		spec string
		want string
	}{
		{"public", "内置入口种子"},
		{"none", "留空"},
		{"/ip4/1.2.3.4/tcp/4001", "/p2p/<节点ID>"},
	}
	for _, c := range cases {
		err := ValidateSeedSpec(c.spec)
		if err == nil {
			t.Fatalf("ValidateSeedSpec(%q) = nil, want error", c.spec)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("ValidateSeedSpec(%q) 文案 = %q, 期望包含 %q", c.spec, err.Error(), c.want)
		}
	}
}

// TestDefaultEntrySeed 内置入口种子必须自身合法且身份明确：
// 它是「留空也能入网」的唯一依靠，写错了所有未填种子的新节点都会静默孤立。
func TestDefaultEntrySeed(t *testing.T) {
	if strings.TrimSpace(DefaultEntrySeed) == "" {
		t.Fatal("DefaultEntrySeed 为空：留空连接种子的节点将无法冷启动入网")
	}
	if err := ValidateSeedSpec(DefaultEntrySeed); err != nil {
		t.Fatalf("DefaultEntrySeed 自身不合法: %v", err)
	}
	if !strings.Contains(DefaultEntrySeed, "/p2p/"+testSeedPeerID) &&
		!strings.Contains(DefaultEntrySeed, "/dnsaddr/") {
		// 只作为兜底断言：真正的校验在下面（解析 + 节点 ID + 公网 + 固定端口）。
		t.Logf("提示：内置入口指向的节点 ID 不是测试常量 %s（当前 %s）", testSeedPeerID, DefaultEntrySeed)
	}
	a, err := ma.NewMultiaddr(DefaultEntrySeed)
	if err != nil {
		t.Fatalf("DefaultEntrySeed 无法解析: %v", err)
	}
	rawID, err := a.ValueForProtocol(ma.P_P2P)
	if err != nil {
		t.Fatalf("DefaultEntrySeed 缺少 /p2p 组件: %v", err)
	}
	if _, err := peer.Decode(rawID); err != nil {
		t.Fatalf("DefaultEntrySeed 的节点 ID 非法: %v", err)
	}
	// 入口必须落在公网且端口固定：局域网地址或随机端口都当不了别人的入口。
	if _, err := a.ValueForProtocol(ma.P_UDP); err != nil {
		if _, errTCP := a.ValueForProtocol(ma.P_TCP); errTCP != nil {
			t.Fatalf("DefaultEntrySeed 缺少 tcp/udp 端口组件: %s", DefaultEntrySeed)
		}
	}
	if ipStr, err := a.ValueForProtocol(ma.P_IP4); err == nil {
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
			t.Fatalf("DefaultEntrySeed 的 IP 不是公网地址（%s），无法作为入口: %s", ipStr, DefaultEntrySeed)
		}
	}
}

// TestStripLegacySeedLiterals 归一历史字面量：只剔字面量，真地址原样保留。
func TestStripLegacySeedLiterals(t *testing.T) {
	real := "/ip4/1.2.3.4/tcp/4001/p2p/" + testSeedPeerID
	cases := []struct {
		name       string
		spec       string
		wantClean  string
		wantLegacy []string
	}{
		{"空", "", "", nil},
		{"纯 none", "none", "", []string{"none"}},
		{"纯 public", "public", "", []string{"public"}},
		{"真地址", real, real, nil},
		{"地址混 none", real + ",none", real, []string{"none"}},
		{"none 混 public", "none , public", "", []string{"none", "public"}},
		{"带空格地址", "  " + real + "  ", real, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean, legacy := StripLegacySeedLiterals(c.spec)
			if clean != c.wantClean {
				t.Fatalf("clean = %q, want %q", clean, c.wantClean)
			}
			if len(legacy) != len(c.wantLegacy) {
				t.Fatalf("legacy = %v, want %v", legacy, c.wantLegacy)
			}
			for i := range legacy {
				if legacy[i] != c.wantLegacy[i] {
					t.Fatalf("legacy = %v, want %v", legacy, c.wantLegacy)
				}
			}
		})
	}
}

// TestResolveSeedSpec 覆盖「自定义 > 内置入口 > 无」的优先级。
// 回归背景：留空种子在跨网冷启动时必然孤立（私有 DHT 路由表为空），
// 现在留空改为使用内置入口；显式关闭内置入口才回到只走私有 DHT + mDNS。
func TestResolveSeedSpec(t *testing.T) {
	real := "/ip4/1.2.3.4/tcp/4001/p2p/" + testSeedPeerID
	cases := []struct {
		name       string
		custom     string
		disable    bool
		wantSpec   string
		wantSource SeedSource
	}{
		{"留空用内置", "", false, DefaultEntrySeed, SeedSourceDefault},
		{"留空但关闭内置", "", true, "", SeedSourceNone},
		{"自定义覆盖内置", real, false, real, SeedSourceCustom},
		{"自定义且关闭内置仍用自定义", real, true, real, SeedSourceCustom},
		{"历史 none 落到内置", "none", false, DefaultEntrySeed, SeedSourceDefault},
		{"历史 public 落到内置", "public", false, DefaultEntrySeed, SeedSourceDefault},
		{"历史 none 且关闭内置", "none", true, "", SeedSourceNone},
		{"只有空格用内置", "   ", false, DefaultEntrySeed, SeedSourceDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, src := ResolveSeedSpec(c.custom, c.disable)
			if spec != c.wantSpec {
				t.Fatalf("spec = %q, want %q", spec, c.wantSpec)
			}
			if src != c.wantSource {
				t.Fatalf("source = %q, want %q", src, c.wantSource)
			}
		})
	}
}
