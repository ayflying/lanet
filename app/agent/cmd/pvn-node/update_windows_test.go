//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// 本文件锁住 Windows 上「替换可能正被映像锁定的 exe」的语义。
// 背景：MoveFileEx(REPLACE_EXISTING) 覆盖被映射的 exe 一律 Access is denied，
// 只能改名腾位 + 写入；详见 update_windows.go 的实测记录。

// TestReplaceLockedBinaryRenamesOldAway 基本替换：新程序落在原路径，
// 旧程序退到 .old（运行中的实例仍靠它继续跑，下次启动时清理）。
func TestReplaceLockedBinaryRenamesOldAway(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lanet.exe")
	writeFile(t, exe, "old-binary")
	candidate := filepath.Join(dir, "candidate.exe")
	writeFile(t, candidate, "new-binary")

	if err := replaceLockedBinary(candidate, exe); err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	if got := readFile(t, exe); got != "new-binary" {
		t.Fatalf("原路径不是新程序：%q", got)
	}
	if got := readFile(t, exe+".old"); got != "old-binary" {
		t.Fatalf("旧程序未退到 .old：%q", got)
	}
	assertNotExist(t, exe+".part")
}

// TestReplaceLockedBinaryFallbackWhenOldLocked 上一轮遗留的 .old 还被占用
// （旧进程尚未退出）时必须换用唯一腾位名：否则 rename 的目标已存在，又会走
// REPLACE_EXISTING 被映像锁拒绝，更新彻底卡死。
func TestReplaceLockedBinaryFallbackWhenOldLocked(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lanet.exe")
	writeFile(t, exe, "old-binary")
	candidate := filepath.Join(dir, "candidate.exe")
	writeFile(t, candidate, "new-binary")
	old := exe + ".old"
	writeFile(t, old, "still-mapped")

	// 只允许读共享（不允许删除）：等价于「该文件仍被映射，删不掉也覆盖不了」。
	p, err := windows.UTF16PtrFromString(old)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("独占打开 .old 失败: %v", err)
	}
	defer windows.CloseHandle(h)

	if err := replaceLockedBinary(candidate, exe); err != nil {
		t.Fatalf("被占用的 .old 不应阻断替换: %v", err)
	}
	if got := readFile(t, exe); got != "new-binary" {
		t.Fatalf("原路径不是新程序：%q", got)
	}
	if got := readFile(t, old); got != "still-mapped" {
		t.Fatalf("被占用的 .old 不应被改动：%q", got)
	}
	matches, err := filepath.Glob(exe + ".old-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("未使用唯一腾位名：%v %v", matches, err)
	}
	if got := readFile(t, matches[0]); got != "old-binary" {
		t.Fatalf("旧程序未落在腾位名上：%q", got)
	}
	if !strings.HasPrefix(filepath.Base(matches[0]), "lanet.exe.old-") {
		t.Fatalf("腾位名格式不符：%s", matches[0])
	}
}

// TestReplaceLockedBinaryMissingSourceKeepsTarget 新程序缺失时必须原样保留旧程序，
// 不能先把旧程序改走再失败（那会让节点彻底没有可执行文件）。
func TestReplaceLockedBinaryMissingSourceKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lanet.exe")
	writeFile(t, exe, "old-binary")

	if err := replaceLockedBinary(filepath.Join(dir, "not-exist.exe"), exe); err == nil {
		t.Fatal("新程序缺失时必须报错")
	}
	if got := readFile(t, exe); got != "old-binary" {
		t.Fatalf("失败后原路径被破坏：%q", got)
	}
	if _, err := os.Stat(exe + ".old"); err == nil {
		t.Fatal("失败后不应留下腾位文件")
	}
}
