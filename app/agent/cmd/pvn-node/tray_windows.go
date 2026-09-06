//go:build windows

package main

import (
	_ "embed"
	"os/exec"
	"runtime"
	"sync"

	"github.com/getlantern/systray"
)

//go:embed icon.ico
var trayIconICO []byte

// lastOpenURL 记录本进程最近一次自动打开的控制台 URL。
// 同一 URL 已自动打开过时跳过重复弹页；托盘手动点击不受限制。
var (
	lastOpenMu  sync.Mutex
	lastOpenURL string
)

// shouldOpenConsole 自动打开前的判断：本进程已自动打开过同一 URL 则跳过
// （页签还开着，重复开只会多弹一个页签）。返回 true 时自动完成标记。
// 说明：节点重启后页签复用无法跨进程检测，但 Edge/Chrome 对相同 URL 的
// 重复打开通常聚焦已有页签，不会无限堆积；这里防的是本进程内的重复弹。
func shouldOpenConsole(url string) bool {
	if url == "" {
		return false
	}
	lastOpenMu.Lock()
	defer lastOpenMu.Unlock()
	if lastOpenURL == url {
		return false
	}
	lastOpenURL = url
	return true
}

// startTray 启动系统托盘：图标 + 右键菜单（打开控制台 / 退出）。
// consoleURL 动态返回控制台地址；onExit 在用户点「退出」时触发节点关闭。
func startTray(consoleURL func() string, onExit func()) {
	go systray.Run(func() {
		systray.SetIcon(trayIconICO)
		systray.SetTooltip("Lanet 虚拟局域网节点")
		mOpen := systray.AddMenuItem("打开控制台", "在浏览器中打开 Web 控制台")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止 Lanet 节点并退出")
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openBrowser(consoleURL())
				case <-mQuit.ClickedCh:
					systray.Quit()
					onExit()
					return
				}
			}
		}()
	}, func() {})
}

// openBrowser 用系统默认浏览器打开控制台页面（不弹额外命令行窗口）。
func openBrowser(url string) {
	if url == "" {
		return
	}
	if runtime.GOOS == "windows" {
		// rundll32 不产生新的控制台窗口，适合 windowsgui 无黑框场景。
		if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
			println("打开浏览器失败:", err.Error())
		}
		return
	}
	_ = exec.Command("xdg-open", url).Start()
}
