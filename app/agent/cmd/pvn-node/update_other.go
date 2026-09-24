//go:build !windows

package main

// replaceLockedBinary 非 Windows 平台不会走到（installNewBinary 已按平台分流）：
// POSIX 允许改名/覆盖运行中的可执行文件，走 rename 腾位 + copyFile 路径。
func replaceLockedBinary(newExe, exePath string) error {
	return nil
}
