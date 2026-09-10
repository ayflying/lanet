package main

import "testing"

// TestResolveNetworkKey 覆盖网络密钥解析与迁移规则：
//   - 命令行/环境变量优先，且显式空串 = 新语义（本机专属默认网络）；
//   - 配置里从未写过该字段（nil）= 老部署 → 保持历史公共网络（legacy）；
//   - 配置显式写了值 → 照用（含显式空串 = 新语义）。
func TestResolveNetworkKey(t *testing.T) {
	cases := []struct {
		name       string
		flagVal    string
		cfgVal     *string
		wantKey    string
		wantLegacy bool
	}{
		{
			name:       "老配置无字段→历史公共网络",
			flagVal:    "@@unset@@",
			cfgVal:     nil,
			wantKey:    "",
			wantLegacy: true,
		},
		{
			name:       "老配置显式空串→新语义本机专属网络",
			flagVal:    "@@unset@@",
			cfgVal:     strPtr(""),
			wantKey:    "",
			wantLegacy: false,
		},
		{
			name:       "配置自定义密钥→照用",
			flagVal:    "@@unset@@",
			cfgVal:     strPtr("my-team"),
			wantKey:    "my-team",
			wantLegacy: false,
		},
		{
			name:       "环境变量覆盖配置密钥",
			flagVal:    "from-env",
			cfgVal:     strPtr("my-team"),
			wantKey:    "from-env",
			wantLegacy: false,
		},
		{
			name:       "环境变量显式空串→本机专属网络（非 legacy）",
			flagVal:    "",
			cfgVal:     strPtr("my-team"),
			wantKey:    "",
			wantLegacy: false,
		},
		{
			name:       "环境变量显式空串且配置为 nil→仍按新语义",
			flagVal:    "",
			cfgVal:     nil,
			wantKey:    "",
			wantLegacy: false,
		},
	}
	for _, c := range cases {
		gotKey, gotLegacy := resolveNetworkKey(c.flagVal, c.cfgVal)
		if gotKey != c.wantKey || gotLegacy != c.wantLegacy {
			t.Errorf("%s: got (key=%q legacy=%v), want (key=%q legacy=%v)",
				c.name, gotKey, gotLegacy, c.wantKey, c.wantLegacy)
		}
	}
}
