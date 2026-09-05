package main

import "syscall"

// spawnSysProcAttr Windows：脱离当前控制台、独立进程组，
// 保证父进程退出不影响拉起的新节点。
const (
	detachedProcess     = 0x00000008
	createNewProcessGrp = 0x00000200
)

var spawnSysProcAttr = &syscall.SysProcAttr{
	CreationFlags: detachedProcess | createNewProcessGrp,
}
