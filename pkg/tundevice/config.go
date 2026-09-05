package tundevice

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ConfigureTUN 给 TUN 网卡配置虚拟 IP：Windows 走 netsh，Linux 走 ip，macOS 走 ifconfig。
// 需要相应系统权限：Windows 管理员、Linux CAP_NET_ADMIN、macOS root。
// 注意：子网掩码统一 /24（Windows netsh 形式），与组内每群独立 /24 的分配语义一致。
func ConfigureTUN(name, ip string, prefixBits int) error {
	switch runtime.GOOS {
	case "windows":
		return runCmd("netsh", "interface", "ip", "set", "address",
			"name="+name, "source=static", "addr="+ip, "mask=255.255.255.0")
	case "linux":
		if err := runCmd("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, prefixBits), "dev", name); err != nil {
			return err
		}
		return runCmd("ip", "link", "set", "dev", name, "up")
	case "darwin":
		return runCmd("ifconfig", name, ip, ip, "up")
	default:
		return fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// runCmd 执行系统命令并聚合输出，失败时带出命令与输出便于排障。
func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
