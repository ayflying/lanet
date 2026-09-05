//go:build !windows

package main

// startTray 非 Windows 平台暂无系统托盘实现（服务器/容器无桌面）。
// 保持与 Windows 版一致的签名，调用方无需区分平台。
func startTray(consoleURL func() string, onExit func()) {}

// openBrowser 非 Windows 平台不自动打开浏览器（容器/无头环境）。
func openBrowser(url string) {}
