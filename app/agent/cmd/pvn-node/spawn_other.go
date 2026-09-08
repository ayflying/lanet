//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// spawnSysProcAttr 非 Windows：新进程独立进程组，不随父进程退出。
var spawnSysProcAttr = &syscall.SysProcAttr{Setpgid: true}

// isElevationRequired 仅 Windows 有意义（提权清单机制）。
func isElevationRequired(err error) bool { return false }

// spawnSelfWindows 在非 Windows 构建中承担同一平台钩子：按原参数启动
// 脱离当前进程组的新实例。旧实现只返回 nil，导致 /api/restart 退出后没有
// 新进程接管。
func spawnSelfWindows() (bool, error) {
	exe := selfExe()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = spawnSysProcAttr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return false, cmd.Process.Release()
}
