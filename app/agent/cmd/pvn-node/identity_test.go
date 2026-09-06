package main

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestDefaultIdentityPathPortable 身份路径固定为裸文件名 node.key（可移植），
// 不绑定生成时刻的 exe 绝对目录——文件夹移动/改名不断链；
// 路径不写入配置文件、不在控制台展示，文件不存在即新用户（SDK 自动创建）。
func TestDefaultIdentityPathPortable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 语义")
	}
	p := defaultIdentityPath(`D:\somewhere`)
	if filepath.IsAbs(p) {
		t.Fatalf("身份路径必须为相对文件名，实际 %q", p)
	}
	if p != "node.key" {
		t.Fatalf("身份路径应为 node.key，实际 %q", p)
	}
}
