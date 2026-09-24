//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// replaceLockedBinary 用 MoveFileEx 原子替换（可能正被映像锁定的）exe。
//
// 为什么不用「改名腾位 + 写入」：lanet.exe 被运行中的节点/托盘进程映射，
// 删除目标需要 DELETE 访问权，会被 Access is denied 拒绝。MoveFileEx 的
// REPLACE_EXISTING 在目录项层面原子换名，不打开目标文件，运行中的 exe
// 也能被替换——新程序在下次进程启动时生效，正在跑的实例不受影响。
// 残留的 lanet.exe.old 不再需要（没有腾位环节），顺手清掉。
func replaceLockedBinary(newExe, exePath string) error {
	from, err := syscall.UTF16PtrFromString(newExe)
	if err != nil {
		return fmt.Errorf("新程序路径非法: %w", err)
	}
	to, err := syscall.UTF16PtrFromString(exePath)
	if err != nil {
		return fmt.Errorf("目标路径非法: %w", err)
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("MoveFileEx 替换失败: %w", err)
	}
	_ = os.Remove(exePath + ".old") // 历史版本遗留，替换成功后已无用途
	return nil
}
