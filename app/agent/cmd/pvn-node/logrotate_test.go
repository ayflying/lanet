package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingFileBoundaryAndEmptyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanet.log")
	r, err := newRotatingFile(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("刚好达到阈值不应轮转")
	}
	if _, err := r.Write([]byte("oversized-entry")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write(nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "oversized-entry" {
		t.Fatalf("空写入不应轮转超长记录: %q %v", got, err)
	}
}

func TestRotatingFileBackupFallbackSurvivesRepeatedWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanet.log")
	r, err := newRotatingFile(path, 8, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.warn = func(string) {}
	if _, err := r.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	r.renameHook = func(old, new string) error {
		if err := os.Rename(old, new); err != nil {
			return err
		}
		return os.Mkdir(old, 0o700) // 阻断新文件创建，但保留 .1 可写。
	}
	for _, entry := range []string{"next", "again", "last"} {
		if _, err := r.Write([]byte(entry)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path + ".1")
	if err != nil || string(got) != "originalnextagainlast" {
		t.Fatalf("重复轮转丢失备份: %q %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("recovered")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "recovered" {
		t.Fatalf("未恢复原路径: %q %v", got, err)
	}
}

func TestRotatingFileConcurrentWriteAndClose(t *testing.T) {
	r, err := newRotatingFile(filepath.Join(t.TempDir(), "lanet.log"), 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if _, err := r.Write([]byte("record\n")); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("并发写入: %v", err)
				}
			}
		}()
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("closed")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("关闭后写入: %v", err)
	}
}

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

// 改名失败（磁盘满/目标被占用）时不得截断原日志，且仍能继续追加写入（审计 item 7）。
// 用注入的改名失败钩子稳定触发失败路径（Windows 下 os.Rename 到已存在目录会改为移入
// 目录而非报错，无法直接用它制造失败）。
func TestRotatingFileKeepsWritingOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lanet.log")
	if err := os.WriteFile(path, []byte("old-log-line\n"), 0o600); err != nil {
		t.Fatalf("预置日志: %v", err)
	}

	var warned int
	r, err := newRotatingFile(path, 16, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	r.warn = func(string) { warned++ }
	// 注入改名失败：模拟磁盘满/权限/被占用导致 os.Rename 失败。
	r.renameHook = func(string, string) error { return fmt.Errorf("injected rename failure") }
	defer r.Close()

	// 触发轮转：现有 12B + 新内容 > 16B 阈值。
	if _, err := r.Write([]byte("new-big-entry-that-exceeds-threshold\n")); err != nil {
		t.Fatalf("改名失败后写入应成功: %v", err)
	}

	// 原日志不得被截断：旧内容仍在，新内容已追加。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读当前日志: %v", err)
	}
	if !strings.Contains(string(got), "old-log-line") {
		t.Fatalf("原日志被截断，旧内容丢失: %q", string(got))
	}
	if !strings.Contains(string(got), "new-big-entry") {
		t.Fatalf("新内容未写入: %q", string(got))
	}
	if _, stErr := os.Stat(path + ".1"); stErr == nil {
		t.Fatal("改名失败不应产生 .1 备份文件")
	}
	if warned == 0 {
		t.Fatal("改名失败未触发告警")
	}
}

// TestRotatingFileRenameAndReopenBothFail 极端恢复路径：改名失败（钩子注入）且
// 回退重开原文件也失败（路径指向不存在的目录）。此时必须「显式返回错误 + 绝不
// panic」：旧日志文件原样保留（改名从未发生），Write 以 ErrClosed 暴露，而不是
// 解引用 nil 句柄崩溃（审计 item 7 的失败安全底线）。
func TestRotatingFileRenameAndReopenBothFail(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "lanet.log")
	// 种子内容小于阈值，避免 newRotatingFile 启动时抢先轮转（否则原内容会被改名走）。
	if err := os.WriteFile(oldPath, []byte("seed-log\n"), 0o600); err != nil {
		t.Fatalf("预置日志: %v", err)
	}
	r, err := newRotatingFile(oldPath, 16, 3)
	if err != nil {
		t.Fatalf("newRotatingFile: %v", err)
	}
	// 把 r.path 指向不存在的子目录，使「改名失败后回退重开原文件」也必然失败。
	r.path = filepath.Join(dir, "gone-subdir", "lanet.log")
	var warned int
	r.warn = func(string) { warned++ }
	r.renameHook = func(string, string) error { return fmt.Errorf("injected rename failure") }
	defer r.Close()

	// 触发轮转：现有内容(9B) + 新内容 > 16B 阈值，且此时 r.path 已是无效目录。
	_, werr := r.Write([]byte("trigger-rotation-now\n"))
	// 改名与回退重开都失败：必须显式错误暴露，绝不可 panic / 解引用 nil。
	if werr == nil {
		t.Fatal("改名与重开都失败时应返回显式错误（而非静默丢日志）")
	}
	if !errors.Is(werr, os.ErrClosed) {
		t.Fatalf("失败路径应返回 ErrClosed，实际: %v", werr)
	}
	// 原日志文件未被截断、内容仍在（改名从未发生）。
	got, rerr := os.ReadFile(oldPath)
	if rerr != nil {
		t.Fatalf("原日志文件应仍在: %v", rerr)
	}
	if string(got) != "seed-log\n" {
		t.Fatalf("原日志被截断/篡改: %q", string(got))
	}
	if _, stErr := os.Stat(oldPath + ".1"); stErr == nil {
		t.Fatal("双重失败不应产生 .1 备份")
	}
	if warned == 0 {
		t.Fatal("双重失败未触发告警")
	}
}
