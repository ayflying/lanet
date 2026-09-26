package p2pkit

import (
	"strings"
	"testing"
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
// public 必须指向「公共 DHT 临时引导」开关，缺 /p2p 必须说清缺什么。
func TestValidateSeedSpecMessages(t *testing.T) {
	cases := []struct {
		spec string
		want string
	}{
		{"public", "公共 DHT 临时引导"},
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
