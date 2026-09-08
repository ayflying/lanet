//go:build !windows

package tundevice

import "os/exec"

// hideWindow 非 Windows 平台无控制台窗口问题，空实现。
func hideWindow(_ *exec.Cmd) {}
