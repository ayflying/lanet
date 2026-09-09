//go:build windows

// 开机自启：写当前用户注册表 Run 键（HKCU\...\Run），双击/UAC 授权过的
// 管理员账户登录后自动拉起 lanet.exe。
//
// 为什么用 HKCU Run 而不是计划任务 / 服务：
//   - exe 内嵌 requireAdministrator 清单，Run 键启动时同样会触发 UAC/直接提权
//     （管理员账户默认 UAC 静默提升），TUN 权限有保障；
//   - 无需额外依赖（schtasks/sc），卸载只需删键，行为对用户可见可逆；
//   - 仅当前用户登录后启动——虚拟局域网本来就是按用户会话使用的场景。
//
// 开机自启时不弹控制台网页：进程用环境变量 LANET_AUTORUN=1 标记自己，
// main 启动时检测到该变量即跳过自动打开浏览器（托盘仍正常启动）。
package main

import (
	"os"
	"strings"

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

// autorunCommand 返回写入注册表的启动命令：完整 exe 路径 + 自启标记。
// 注册表 Run 值支持带引号路径 + 参数，登录时由 explorer 解析执行。
func autorunCommand() string {
	return `"` + selfExe() + `" -autorun`
}

// isAutorunEnabled 查询开机自启是否已开启（键存在且指向本 exe）。
// 键存在但路径不一致（用户挪过目录）视为未开启，下次保存时会覆盖为新路径。
func isAutorunEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autorunRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	val, _, err := k.GetStringValue(autorunValueName)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.Trim(val, `"`), selfExe()) ||
		strings.Contains(strings.ToLower(val), strings.ToLower(selfExe()))
}

// autorunSupported 当前平台是否支持程序内开机自启（前端据此显示/隐藏开关）。
func autorunSupported() bool { return true }

// setAutorunEnabled 开启/关闭开机自启。开启时写入「exe 路径 + -autorun 参数」；
// 关闭时删除值（键本身是系统共享的，不删键）。
func setAutorunEnabled(enable bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autorunRegistryPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enable {
		return k.DeleteValue(autorunValueName)
	}
	return k.SetStringValue(autorunValueName, autorunCommand())
}
