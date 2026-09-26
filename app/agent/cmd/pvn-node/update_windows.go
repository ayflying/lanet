//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// replaceLockedBinary 替换「可能正被映像锁定的」exe。
//
// 实测（2026-09-26，Windows 11 + go1.26）：
//   - MoveFileEx(REPLACE_EXISTING) 覆盖一个正被映射的 exe 一律返回 Access is denied，
//     与调用方是否自己映射它无关（另一个进程映射也一样拒）；
//   - 纯改名（MoveFile，目标名不存在）对正被映射的 exe **允许**。
//
// 所以「原子换名」在自更新场景走不通，只能改名腾位 + 写入：把运行中的旧程序改成
// 另一个名字，腾出原路径后把新程序写进去。正在跑的实例继续用旧映像，新程序下次
// 启动生效。
//
// 腾位名优先 lanet.exe.old（启动时清理）；若旧名仍被别的进程占用删不掉，就退到
// 带时间戳的唯一名字——否则 rename 的目标已存在又会触发 REPLACE_EXISTING 被拒。
func replaceLockedBinary(newExe, exePath string) error {
	old := exePath + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		old = fmt.Sprintf("%s.old-%d", exePath, time.Now().Unix())
	}
	if err := os.Rename(exePath, old); err != nil {
		return fmt.Errorf("旧程序改名腾位失败: %w", err)
	}
	// copyFile 写的是 exePath.part 再改名，同目录内原子落位，不会留下写了一半的程序。
	if err := copyFile(newExe, exePath); err != nil {
		_ = os.Rename(old, exePath) // 回滚：旧程序改回原路径
		return fmt.Errorf("写入新程序失败（已回滚）: %w", err)
	}
	return nil
}
