package firewall

import "testing"

func TestDenyAllByDefault(t *testing.T) {
	f := New()
	if f.Allow("100.64.0.2", 3389) {
		t.Fatal("deny-all must reject by default")
	}
}

func TestAllowAll(t *testing.T) {
	f := New()
	f.Set(ModeAllowAll, nil)
	if !f.Allow("100.64.0.2", 1) || !f.Allow("100.64.9.9", 65535) {
		t.Fatal("allow-all must accept any source/port")
	}
}

func TestAllowListRules(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{
		{Source: "*", Port: "3389"},
		{Source: "100.64.0.5", Port: "5000-5010"},
		{Source: "100.64.1.0/24", Port: "80"},
	})
	cases := []struct {
		ip   string
		port int
		want bool
	}{
		{"100.64.0.2", 3389, true},  // 通配来源
		{"100.64.0.2", 3390, false}, // 端口不匹配
		{"100.64.0.5", 5000, true},  // 范围下界
		{"100.64.0.5", 5010, true},  // 范围上界
		{"100.64.0.5", 5011, false}, // 范围外
		{"100.64.0.6", 5000, false}, // 来源不匹配
		{"100.64.1.77", 80, true},   // CIDR 内
		{"100.64.2.77", 80, false},  // CIDR 外
		{"", 3389, false},           // 未知来源拒绝
	}
	for _, c := range cases {
		if got := f.Allow(c.ip, c.port); got != c.want {
			t.Fatalf("Allow(%q, %d) = %v, want %v", c.ip, c.port, got, c.want)
		}
	}
}

func TestHotUpdate(t *testing.T) {
	f := New()
	if f.Allow("100.64.0.2", 80) {
		t.Fatal("must deny before update")
	}
	f.Set(ModeAllowList, []Rule{{Source: "*", Port: "80"}})
	if !f.Allow("100.64.0.2", 80) {
		t.Fatal("must allow after update")
	}
	mode, rules := f.Snapshot()
	if mode != ModeAllowList || len(rules) != 1 {
		t.Fatalf("snapshot = %s, %d rules", mode, len(rules))
	}
}
