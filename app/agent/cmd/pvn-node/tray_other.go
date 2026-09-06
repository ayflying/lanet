//go:build !windows

package main

// startTray 非 Windows 平台暂无系统托盘实现（服务器/容器无桌面）。
// 保持与 Windows 版一致的签名，调用方无需区分平台。
func startTray(consoleURL func() string, onExit func()) {}

// openBrowser 非 Windows 平台不自动打开浏览器（容器/无头环境）。
func openBrowser(url string) {}

// shouldOpenConsole 非 Windows 平台不自动开浏览器，恒返回 false
// （与 openBrowser 空实现语义一致；与 Windows 版保持相同签名）。
func shouldOpenConsole(url string) bool { return false }
