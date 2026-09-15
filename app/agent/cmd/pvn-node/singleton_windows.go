//go:build windows

package main

import (
	"os"
	"runtime"

	"golang.org/x/sys/windows"
)

// singletonLockOffset 锁区起点，刻意放在数据区（文件头部几百字节）之外。
//
// Windows 的字节区间锁是**强制锁**：别的句柄连**读**落在锁区里的字节都会被拒
// （ERROR_LOCK_VIOLATION）。而锁文件的内容正是要给被拒绝的第二份进程读出来做
// 提示的，若锁在 [0,1)，对方连第 0 字节都读不到，只能拿到一个空提示
//（实测如此）。放到 1MB 处、且 Windows 允许锁到文件末尾之外，双方就互不干扰。
const singletonLockOffset = 1 << 20

// tryLockFile 非阻塞地独占锁定锁文件的一个字节（见上：锁区必须避开数据区）。
// 锁的持有范围与进程生命周期绑定：句柄关闭（含进程被强杀）即由内核释放，
// 不会像「文件存在即占用」那样留下需要人工清理的残留。
func tryLockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	ol.Offset = singletonLockOffset
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol)
	runtime.KeepAlive(ol)
	return err
}

// isLockBusy 判断错误是否为「已被别的进程锁住」。
// ERROR_LOCK_VIOLATION 是非阻塞加锁失败的常规返回；ERROR_SHARING_VIOLATION
// 出现在锁文件被别的进程以独占方式打开时，语义上同样是「有人在用」。
func isLockBusy(err error) bool {
	return err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_SHARING_VIOLATION
}

// unlockFile 释放锁；失败也无所谓，关句柄时会一并释放。
func unlockFile(f *os.File) {
	ol := new(windows.Overlapped)
	ol.Offset = singletonLockOffset
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
	runtime.KeepAlive(ol)
}