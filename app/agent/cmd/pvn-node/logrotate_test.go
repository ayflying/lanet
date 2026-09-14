package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写入量小于阈值时不应触发轮转。
func TestRotatingFileNoRotateBelowLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.log")

	r, err := newRotatingFile(path, 1024, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	for i := 0; i < 10; i++ {
		if _, err := r.Write([]byte("hello\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf("不应产生备份文件 .1")
	}
}

// 跨越阈值时应轮转：旧内容进 .1，当前文件重新从零开始。
func TestRotatingFileRotatesOnThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.log")

	// 阈值 1000：OLD(12B) + 500B = 512B 不轮转；再写 500B 时 512+500>1000 触发一次轮转。
	// 只触发一轮，.1 里就是「OLD + 第一段 500B」，不会被后续轮转覆盖。
	r, err := newRotatingFile(path, 1000, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	if _, err := r.Write([]byte("OLD-CONTENT\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := r.Write([]byte(strings.Repeat("x", 500))); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf("尚未越过阈值，不应轮转")
	}
	if _, err := r.Write([]byte(strings.Repeat("y", 500))); err != nil {
		t.Fatalf("write: %v", err)
	}

	bak, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("期望存在备份 .1: %v", err)
	}
	if !strings.Contains(string(bak), "OLD-CONTENT") {
		t.Fatalf("备份 .1 应含此前写入的内容，实际: %q", string(bak))
	}
	if want := 12 + 500; len(bak) != want {
		t.Fatalf("备份 .1 应为轮转前的完整内容 %d 字节，实际 %d", want, len(bak))
	}

	// 轮转后当前文件只含轮转后的新写入。
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读当前日志: %v", err)
	}
	if len(cur) != 500 || strings.Contains(string(cur), "OLD-CONTENT") {
		t.Fatalf("轮转后当前文件应为轮转后的新写入（500B，不含旧内容），实际 %d 字节", len(cur))
	}
}

// 备份份数应封顶：最多 .1 … .maxBackups，更旧的被删除。
func TestRotatingFileCapsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.log")

	r, err := newRotatingFile(path, 64, 2)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	// 触发多轮：每轮写 >64 字节。
	for round := 0; round < 6; round++ {
		for i := 0; i < 10; i++ {
			if _, err := r.Write([]byte(strings.Repeat("y", 16) + "\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}

	for i := 1; i <= 2; i++ {
		if _, err := os.Stat(filepath.Join(dir, "lanet.log."+string(rune('0'+i)))); err != nil {
			t.Fatalf("期望存在备份 .%d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatalf("不应存在第 3 份备份（maxBackups=2）")
	}
}

// 启动时若历史日志已超限，应立即轮转一次，避免「一启动就写爆」。
func TestRotatingFileRotatesOversizedExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.log")

	if err := os.WriteFile(path, []byte(strings.Repeat("z", 500)), 0o600); err != nil {
		t.Fatalf("预置大日志: %v", err)
	}

	r, err := newRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	defer r.Close()

	bak, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("启动时应轮转出 .1: %v", err)
	}
	if int64(len(bak)) != 500 {
		t.Fatalf("旧内容应完整进 .1，实际 %d 字节", len(bak))
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 当前日志: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("轮转后当前文件应为空，实际 %d 字节", st.Size())
	}
}