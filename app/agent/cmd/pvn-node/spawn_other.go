//go:build !windows

package main

import "syscall"

// spawnSysProcAttr 非 Windows：新进程独立进程组，不随父进程退出。
var spawnSysProcAttr = &syscall.SysProcAttr{Setpgid: true}
