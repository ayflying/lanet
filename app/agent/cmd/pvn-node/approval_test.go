package main

import "testing"

// TestResolveTriBool 三态布尔解析：显式传值 > 配置文件 > 默认值。
// 用于「默认开启、允许关闭」的连接审批开关——必须能区分
// 「命令行没传」（读配置）与「显式传 false」（覆盖配置）。
func TestResolveTriBool(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name   string
		flag   string
		cfg    *bool
		def    bool
		expect bool
	}{
		// 未传（哨兵）：走配置文件，配置缺失走默认。
		{"未传+配置nil+默认true", "@@unset@@", nil, true, true},
		{"未传+配置nil+默认false", "@@unset@@", nil, false, false},
		{"未传+配置true+默认false", "@@unset@@", &tr, false, true},
		{"未传+配置false+默认true", "@@unset@@", &fa, true, false},
		{"空串等同未传", "", &tr, false, true},
		// 显式传值：覆盖配置文件。
		{"传true+配置false", "true", &fa, false, true},
		{"传TRUE大小写+配置false", "TRUE", &fa, false, true},
		{"传1+配置false", "1", &fa, false, true},
		{"传yes+配置false", "yes", &fa, false, true},
		{"传on+配置false", "on", &fa, false, true},
		{"传false+配置true", "false", &tr, true, false},
		{"传FALSE大小写+配置true", "FALSE", &tr, true, false},
		{"传0+配置true", "0", &tr, true, false},
		{"传no+配置true", "no", &tr, true, false},
		{"传off+配置true", "off", &tr, true, false},
		{"带空白仍可识别", "  true  ", &fa, false, true},
		// 非法值：按默认处理（并打日志提示），不影响启动。
		{"非法值+默认true", "maybe", nil, true, true},
		{"非法值+默认false", "maybe", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTriBool(c.flag, c.cfg, c.def); got != c.expect {
				t.Fatalf("resolveTriBool(%q, %v, %v) = %v, want %v", c.flag, c.cfg, c.def, got, c.expect)
			}
		})
	}
}
