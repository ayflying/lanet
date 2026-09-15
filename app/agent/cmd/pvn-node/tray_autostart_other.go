//go:build !windows

package main

// 非 Windows 平台的托盘与「登录时拉起托盘」均为空实现：Linux 用 systemd、
// 容器用 restart 策略，都没有「服务跑在 Session 0、托盘在用户会话」这层
// 隔离问题，也不需要计划任务。保持与 Windows 版一致的签名，调用方无需
// 按平台分支。

// runTrayCompanion 非 Windows 平台不进入托盘伴侣模式。
func runTrayCompanion() (bool, error) { return false, nil }

// installTrayAutostart 非 Windows 平台无需注册托盘任务。
func installTrayAutostart() error { return nil }

// removeTrayAutostart 非 Windows 平台无需删除托盘任务。
func removeTrayAutostart() error { return nil }

// isTrayAutostartInstalled 非 Windows 平台恒为 false。
func isTrayAutostartInstalled() bool { return false }

// startTrayTaskNow 非 Windows 平台无操作。
func startTrayTaskNow() {}
