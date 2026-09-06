package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDefaultIdentityPathPortable Windows 默认身份路径必须是裸文件名（可移植），
// 不能绑定生成时刻的 exe 绝对目录——否则文件夹一移动配置就失效。
func TestDefaultIdentityPathPortable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 语义")
	}
	p := defaultIdentityPath(`D:\somewhere`)
	if filepath.IsAbs(p) {
		t.Fatalf("默认身份路径必须为相对文件名，实际 %q", p)
	}
	if p != "node.key" {
		t.Fatalf("默认身份路径应为 node.key，实际 %q", p)
	}
}

// TestResolveIdentityPath 覆盖 resolveIdentityPath 全部分支：
// 裸文件名相对配置目录、绝对路径存在原样用、失效绝对路径自愈到 exe 同目录。
func TestResolveIdentityPath(t *testing.T) {
	configDir := t.TempDir()
	exeDir := t.TempDir()

	// 1) 空值 → 默认路径
	got, healed := resolveIdentityPath("", configDir, exeDir)
	if healed || got != defaultIdentityPath(exeDir) {
		t.Fatalf("空值应返回默认路径，实际 %q healed=%v", got, healed)
	}

	// 2) 裸文件名 → 相对配置目录解析
	got, healed = resolveIdentityPath("node.key", configDir, exeDir)
	if healed {
		t.Fatalf("裸文件名不应触发自愈")
	}
	if want := filepath.Join(configDir, "node.key"); got != want {
		t.Fatalf("裸文件名应相对配置目录解析：got %q want %q", got, want)
	}

	// 3) 绝对路径且文件存在 → 原样使用
	existing := filepath.Join(exeDir, "custom.key")
	if err := os.WriteFile(existing, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, healed = resolveIdentityPath(existing, configDir, exeDir)
	if healed || got != existing {
		t.Fatalf("存在的绝对路径应原样使用：got %q healed=%v", got, healed)
	}

	// 4) 绝对路径失效 + exe 同目录有同名文件 → 自愈并提示回写
	stale := filepath.Join(configDir, "old-place", "node.key") // 该目录不存在
	moved := filepath.Join(exeDir, "node.key")
	if err := os.WriteFile(moved, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, healed = resolveIdentityPath(stale, configDir, exeDir)
	if !healed {
		t.Fatalf("失效绝对路径应触发自愈")
	}
	if got != moved {
		t.Fatalf("自愈应指向 exe 同目录同名文件：got %q want %q", got, moved)
	}

	// 5) 绝对路径失效且无处可找 → 沿用原路径（首次启动在那里生成），不误报自愈
	// （用独立文件名，避免命中用例 4 留在 exe 目录的 node.key）
	lost := filepath.Join(configDir, "gone", "missing.key")
	got, healed = resolveIdentityPath(lost, configDir, exeDir)
	if healed {
		t.Fatalf("无处自愈时不应误报 healed")
	}
	if got != lost {
		t.Fatalf("无处自愈时应沿用原路径：got %q want %q", got, lost)
	}

	// 6) 自愈候选与原路径相同（exe 同目录即原目录）时不死循环
	same := filepath.Join(exeDir, "node.key")
	got, healed = resolveIdentityPath(same, configDir, exeDir)
	if got != same {
		t.Fatalf("候选与原路径相同时应返回原路径：got %q", got)
	}
	_ = healed // 文件存在 → healed=false；文件不存在 → 候选==原路径不成立自愈，均安全
}
