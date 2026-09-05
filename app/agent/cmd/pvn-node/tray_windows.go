//go:build windows

package main

import (
	_ "embed"
	"os/exec"
	"runtime"

	"github.com/getlantern/systray"
)

//go:embed icon.ico
var trayIconICO []byte

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
