package main

import (
	"reflect"
	"testing"
)

// TestParseBootstrapSeeds 覆盖「连接种子只认地址」的解析规则。
// 回归背景：连接种子字段曾接受 none / public 字面量，public 还要求同时开启
// 公共 DHT 才生效；用户填了 public 却只走私有 DHT + mDNS，节点静默孤立。
// 现在公共引导只由「公共 DHT 临时引导」开关决定，种子字段里只剩地址。
func TestParseBootstrapSeeds(t *testing.T) {
	const seed = "/ip4/43.136.124.167/udp/4001/quic-v1/p2p/12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf"
	const seed2 = "/ip4/192.168.50.243/tcp/4001/p2p/12D3KooWGuwB2SpnvuzXBC1HpXrnwNPJC45GSMf2EMzWK9VWow8h"
	cases := []struct {
		name       string
		raw        string
		wantAddrs  []string
		wantLegacy []string
	}{
		{"留空", "", nil, nil},
		{"只有空白与逗号", " , ,  ", nil, nil},
		{"单个种子", seed, []string{seed}, nil},
		{"两端带空格", "  " + seed + "  ", []string{seed}, nil},
		{"逗号分隔两个种子", seed + "," + seed2, []string{seed, seed2}, nil},
		{"历史字面量 none", "none", nil, []string{"none"}},
		{"历史字面量 public", "public", nil, []string{"public"}},
		{"public 混在列表里", seed + ",public", []string{seed}, []string{"public"}},
		{"none 与 public 同时出现", "none,public", nil, []string{"none", "public"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addrs, legacy := parseBootstrapSeeds(c.raw)
			if !reflect.DeepEqual(addrs, c.wantAddrs) {
				t.Fatalf("parseBootstrapSeeds(%q) addrs = %#v, want %#v", c.raw, addrs, c.wantAddrs)
			}
			if !reflect.DeepEqual(legacy, c.wantLegacy) {
				t.Fatalf("parseBootstrapSeeds(%q) legacy = %#v, want %#v", c.raw, legacy, c.wantLegacy)
			}
		})
	}
}

// TestDerefStr 覆盖 *string 配置读取：nil（老配置没写该字段）按空串处理，
// 而不是当成某个默认字面量——种子字段的「未配置」与「显式留空」语义相同。
func TestDerefStr(t *testing.T) {
	if got := derefStr(nil); got != "" {
		t.Fatalf("derefStr(nil) = %q, want 空串", got)
	}
	if got := derefStr(strPtr("none")); got != "none" {
		t.Fatalf("derefStr(strPtr(none)) = %q, want none", got)
	}
	if got := derefStr(strPtr("")); got != "" {
		t.Fatalf("derefStr(strPtr(\"\")) = %q, want 空串", got)
	}
}
