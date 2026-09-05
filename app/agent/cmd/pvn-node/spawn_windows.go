package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows：脱离当前控制台、独立进程组，
// 保证父进程退出不影响拉起的新节点。
const (
	detachedProcess     = 0x00000008
	createNewProcessGrp = 0x00000200
)

var spawnSysProcAttr = &syscall.SysProcAttr{
	CreationFlags: detachedProcess | createNewProcessGrp,
}

// isElevationRequired 判断 CreateProcess 错误是否为「需要提权」（ERROR_ELEVATION_REQUIRED 740）。
// 0.3.0 起 exe 内嵌 requireAdministrator 清单，非提权父进程直接 CreateProcess 会被拒绝。
func isElevationRequired(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == windows.ERROR_ELEVATION_REQUIRED
}

// spawnSelfRunas 通过 ShellExecute "runas" 拉起新进程：
// 当前进程已提权（双击 UAC 启动的正常场景）时不弹任何窗口直接拉起；
// 未提权时弹 UAC 由用户确认。
func spawnSelfRunas() error {
	exe := selfExe()
	quoted := make([]string, len(os.Args[1:]))
	for i, a := range os.Args[1:] {
		quoted[i] = syscall.EscapeArg(a)
	}
	return windows.ShellExecute(0,
		windows.StringToUTF16Ptr("runas"),
		windows.StringToUTF16Ptr(exe),
		windows.StringToUTF16Ptr(strings.Join(quoted, " ")),
		windows.StringToUTF16Ptr(filepath.Dir(exe)),
		1 /* SW_SHOWNORMAL */)
}

// spawnSelfWindows 先走 CreateProcess（独立进程组）；因提权清单被拒（740）时
// 降级 ShellExecute "runas"。第一个返回值表示是否走了提权路径（日志用）。
func spawnSelfWindows() (bool, error) {
	exe := selfExe()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.SysProcAttr = spawnSysProcAttr
	if err := cmd.Start(); err != nil {
		if !isElevationRequired(err) {
			return false, err
		}
		if err2 := spawnSelfRunas(); err2 != nil {
			return true, fmt.Errorf("提权拉起失败: %w", err2)
		}
		return true, nil
	}
	return false, nil
}
