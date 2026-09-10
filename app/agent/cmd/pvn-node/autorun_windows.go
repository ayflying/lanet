//go:build windows

// Windows 开机自启使用 LocalSystem 系统服务：机器启动后即运行，不依赖用户登录。
// 本文件保留原 autorun 函数名作为控制台 API 的兼容层；旧版本遗留的 HKCU Run
// 注册项会在安装服务时清理，避免登录后再启动第二份节点。
package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

// autorunEnv 开机自启标记环境变量（main 启动时写入自身注册表命令行里带上）。
const autorunEnv = "LANET_AUTORUN"

// isAutorunLaunch 当前进程是否由开机自启拉起。
func isAutorunLaunch() bool {
	return os.Getenv(autorunEnv) == "1"
}

// autorunRegistryPath 注册表 Run 键路径（当前用户）。
const autorunRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// autorunValueName 注册表值名（固定，任务管理器「启动应用」里显示的名字）。
const autorunValueName = "Lanet"

// isAutorunEnabled 查询 LocalSystem 自动启动服务是否已安装。
func isAutorunEnabled() bool { return isWindowsServiceInstalled() }

// autorunSupported 当前平台是否支持程序内开机自启（前端据此显示/隐藏开关）。
func autorunSupported() bool { return true }

func autorunKind() string { return "windows_service" }

// setAutorunEnabled 开启/关闭 LocalSystem 自动启动服务，并清理旧版 Run 项。
func setAutorunEnabled(enable bool) error {
	if !enable {
		if err := removeWindowsService(); err != nil {
			return err
		}
		removeLegacyAutorunValue()
		return nil
	}
	if err := installWindowsService(); err != nil {
		return err
	}
	removeLegacyAutorunValue()
	return nil
}

// removeLegacyAutorunValue 清理 v0.5.23 及以前写入的 HKCU Run 项。
func removeLegacyAutorunValue() {
	k, err := registry.OpenKey(registry.CURRENT_USER, autorunRegistryPath, registry.SET_VALUE)
	if err == nil {
		defer k.Close()
		_ = k.DeleteValue(autorunValueName)
	}
}
