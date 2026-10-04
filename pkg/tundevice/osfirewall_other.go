//go:build !windows

package tundevice

// configureOSFirewall 非 Windows 平台没有需自动配置的系统防火墙，
// 空实现保持 ConfigureTUN 调用点跨平台一致。
func configureOSFirewall(string) {}
