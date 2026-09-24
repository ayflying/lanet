package main

import "testing"

// TestNormalizeAdvertiseInput 多行对外地址输入归一化：换行/混排分隔符转逗号、
// 去空白与空项、空输入保持空串（= 恢复自动枚举）。
func TestNormalizeAdvertiseInput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"纯LF多行", "1.2.3.4:4001\n[2408::1]:4001", "1.2.3.4:4001,[2408::1]:4001"},
		{"CRLF多行", "1.2.3.4:4001\r\n[2408::1]:4001", "1.2.3.4:4001,[2408::1]:4001"},
		{"混排换行逗号", "1.2.3.4:4001\n,/ip4/9.9.9.9/tcp/1", "1.2.3.4:4001,/ip4/9.9.9.9/tcp/1"},
		{"逐项去空白", " 1.2.3.4:4001 \n [2408::1]:4001 \n", "1.2.3.4:4001,[2408::1]:4001"},
		{"空串", "", ""},
		{"纯空白换行", "\n \r\n ,", ""},
		{"已是逗号分隔", "1.2.3.4:4001,[2408::1]:4001", "1.2.3.4:4001,[2408::1]:4001"},
	}
	for _, c := range cases {
		if got := normalizeAdvertiseInput(c.in); got != c.want {
			t.Errorf("%s: normalizeAdvertiseInput(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}
