//go:build windows

package tundevice

import (
	"os/exec"
	"syscall"
)

// hideWindow 阻止子进程弹出控制台窗口。Windows GUI 程序（-H=windowsgui）
// 调用 netsh 等控制台程序时会闪黑框并抢占用户焦点（邻居表周期写入每 10 秒
// 触发一轮，体感就是黑框狂闪），CREATE_NO_WINDOW 让子进程无窗口静默执行。
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
