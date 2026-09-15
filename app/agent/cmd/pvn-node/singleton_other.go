//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile 非阻塞地独占锁定锁文件（flock）。
// flock 是建议锁，进程退出（含被 SIGKILL 强杀）由内核自动释放，不留残留。
// 注意：容器场景下同一配置目录本来就只跑一个进程，这里是同一套逻辑的兜底
// （例如宿主机直接跑二进制、或用 -config 指向同一个挂载目录起两份）。
func tryLockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// isLockBusy 判断错误是否为「已被别的进程锁住」。
func isLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// unlockFile 释放锁；失败也无所谓，关句柄时会一并释放。
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}