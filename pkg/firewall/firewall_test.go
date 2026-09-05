package firewall

import "testing"

func TestDenyAllByDefault(t *testing.T) {
	f := New()
	if f.Allow("100.64.0.2", ProtoTCP, 3389) {
		t.Fatal("deny-all must reject by default")
	}
	if f.AllowStream("100.64.0.2", "/pvn/tunnel/1.0.0") {
		t.Fatal("deny-all must reject streams by default")
	}
}

func TestAllowAll(t *testing.T) {
	f := New()
	f.Set(ModeAllowAll, nil)
	if !f.Allow("100.64.0.2", ProtoTCP, 1) ||
		!f.Allow("100.64.9.9", ProtoUDP, 65535) ||
		!f.AllowStream("100.64.0.2", "/pvn/tunnel/1.0.0") {
		t.Fatal("allow-all must accept any source/proto/port")
	}
}

func TestAllowListRules(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{
		{Source: "*", Port: "3389"},                // 留空 Proto = tcp
		{Source: "100.64.0.5", Port: "5000-5010"},  // tcp 范围
		{Source: "100.64.1.0/24", Port: "80"},      // CIDR
		{Source: "*", Proto: ProtoUDP, Port: "53"}, // UDP DNS
		{Source: "100.64.0.7", Proto: ProtoUDP, Port: "5000-6000"},
	})
	cases := []struct {
		ip    string
		proto string
		port  int
		want  bool
	}{
		{"100.64.0.2", ProtoTCP, 3389, true},  // 通配来源（默认 tcp）
		{"100.64.0.2", ProtoTCP, 3390, false}, // 端口不匹配
		{"100.64.0.2", ProtoUDP, 3389, false}, // tcp 规则不放行 udp
		{"100.64.0.5", ProtoTCP, 5000, true},  // 范围下界
		{"100.64.0.5", ProtoTCP, 5010, true},  // 范围上界
		{"100.64.0.5", ProtoTCP, 5011, false}, // 范围外
		{"100.64.0.6", ProtoTCP, 5000, false}, // 来源不匹配
		{"100.64.1.77", ProtoTCP, 80, true},   // CIDR 内
		{"100.64.2.77", ProtoTCP, 80, false},  // CIDR 外
		{"100.64.0.2", ProtoUDP, 53, true},    // udp 规则
		{"100.64.0.2", ProtoTCP, 53, false},   // udp 规则不放行 tcp
		{"100.64.0.7", ProtoUDP, 5999, true},  // udp 范围内
		{"100.64.0.7", ProtoUDP, 6001, false}, // udp 范围外
		{"", ProtoTCP, 3389, false},           // 未知来源拒绝
		{"", ProtoUDP, 53, false},             // 未知来源拒绝（udp）
	}
	for _, c := range cases {
		if got := f.Allow(c.ip, c.proto, c.port); got != c.want {
			t.Fatalf("Allow(%q, %q, %d) = %v, want %v", c.ip, c.proto, c.port, got, c.want)
		}
	}
}

func TestAllowStreamRules(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{
		{Source: "*", Proto: "/pvn/tunnel/1.0.0"},
		{Source: "100.64.0.7", Proto: "/myapp/1.0.0"},
	})
	cases := []struct {
		ip   string
		id   string
		want bool
	}{
		{"100.64.0.2", "/pvn/tunnel/1.0.0", true}, // 通配来源协议规则
		{"100.64.0.2", "/myapp/1.0.0", false},     // 未配置的协议拒绝
		{"100.64.0.7", "/myapp/1.0.0", true},      // 精确来源协议规则
		{"100.64.0.9", "/myapp/1.0.0", false},     // 精确来源不匹配
		{"100.64.0.2", "tcp", false},              // 协议 ID 规则不匹配传输层字面量
	}
	for _, c := range cases {
		if got := f.AllowStream(c.ip, c.id); got != c.want {
			t.Fatalf("AllowStream(%q, %q) = %v, want %v", c.ip, c.id, got, c.want)
		}
	}
}

// "*" 协议规则放行全部应用流。
func TestAllowStreamAnyProto(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{{Source: "*", Proto: ProtoAny}})
	if !f.AllowStream("100.64.0.2", "/anything/1.0.0") || !f.AllowStream("100.64.0.9", "/unknown/2.0") {
		t.Fatal("* proto rule must allow all streams")
	}
}

// 协议流规则不应被端口字段干扰：带 Port 的协议 ID 规则不生效（Port 必须空或 "*"）。
func TestAllowStreamRejectsPortQualifiedRules(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{
		{Source: "*", Proto: "/pvn/tunnel/1.0.0", Port: "8080"},
	})
	if f.AllowStream("100.64.0.2", "/pvn/tunnel/1.0.0") {
		t.Fatal("stream rule with concrete port must not match")
	}
}

// 规则里的协议 ID 以 "/" 开头，不得被误当作 CIDR 解析。
func TestSlashProtoNotTreatedAsCIDR(t *testing.T) {
	f := New()
	f.Set(ModeAllowList, []Rule{
		{Source: "*", Proto: "/pvn/tunnel/1.0.0"},
	})
	if f.Allow("100.64.0.2", ProtoTCP, 80) {
		t.Fatal("protocol rule must not allow tcp ports")
	}
}

func TestHotUpdate(t *testing.T) {
	f := New()
	if f.Allow("100.64.0.2", ProtoTCP, 80) {
		t.Fatal("must deny before update")
	}
	f.Set(ModeAllowList, []Rule{{Source: "*", Port: "80"}})
	if !f.Allow("100.64.0.2", ProtoTCP, 80) {
		t.Fatal("must allow after update")
	}
	mode, rules := f.Snapshot()
	if mode != ModeAllowList || len(rules) != 1 {
		t.Fatalf("snapshot = %s, %d rules", mode, len(rules))
	}
}
