package main

import "strings"

// normalizeAdvertiseInput 把控制台多行对外地址输入归一化为逗号分隔：
// CRLF/LF 换行都换成逗号，逐项去空白；连续分隔符合并，空项丢弃。
// 空输入返回空串（= 恢复自动枚举），其余交给 ValidateAdvertiseSpec 严格校验。
func normalizeAdvertiseInput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", ",")
	var parts []string
	for _, raw := range strings.Split(s, ",") {
		if item := strings.TrimSpace(raw); item != "" {
			parts = append(parts, item)
		}
	}
	return strings.Join(parts, ",")
}
