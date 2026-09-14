package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 身份路径必须锚定在**配置文件目录**、且为绝对路径。
//
// 回归防护：0.5.39 及以前 defaultIdentityPath 返回裸相对名 "node.key"，
// 按进程 CWD 解析——Windows 服务由 SCM 拉起时 CWD=C:\Windows\System32，
// 节点于是在那里新建了一个全新身份，PeerID 与虚拟 IP 双双漂移
//（实测：12D3KooWJVB9… / 10.7.207.102 → 12D3KooWSHrqx8… / 10.7.187.177）。
// 旧测试 TestDefaultIdentityPathPortable 曾断言「必须是相对文件名」，
// 把该缺陷锁死，故一并移除。
func TestDefaultIdentityPathAnchoredToConfigDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		// 非 Windows 维持容器约定：固定 /data/node.key，与配置位置无关。
		if got := defaultIdentityPath(`C:\lanet-node\lanet.json`); got != "/data/node.key" {
			t.Fatalf("非 Windows 应固定 /data/node.key，实际 %q", got)
		}
		return
	}
	cfg := filepath.Join(t.TempDir(), "lanet.json")
	got := defaultIdentityPath(cfg)
	if !filepath.IsAbs(got) {
		t.Fatalf("身份路径必须是绝对路径（不依赖 CWD），实际 %q", got)
	}
	if want := filepath.Join(filepath.Dir(cfg), "node.key"); got != want {
		t.Fatalf("身份路径应锚定配置目录：want %q, got %q", want, got)
	}
}

// 换 CWD 不得改变身份路径（服务/计划任务的核心回归点）。
func TestDefaultIdentityPathIndependentOfCWD(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 专有行为（其他平台为固定 /data/node.key）")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "lanet.json")
	before := defaultIdentityPath(cfg)

	old, err := os.Getwd()
	if err != nil {
		t.Skipf("无法获取当前工作目录: %v", err)
	}
	if err := os.Chdir(os.TempDir()); err != nil {
		t.Skipf("无法切换工作目录: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	if after := defaultIdentityPath(cfg); after != before {
		t.Fatalf("身份路径不应随 CWD 变化：%q -> %q", before, after)
	}
}
