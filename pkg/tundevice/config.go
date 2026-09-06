package tundevice

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ConfigureTUN 给 TUN 网卡配置虚拟 IP：Windows 走 netsh，Linux 走 ip，macOS 走 ifconfig。
// 需要相应系统权限：Windows 管理员、Linux CAP_NET_ADMIN、macOS root。
// 掩码固定 /16：Standalone 模式的虚拟 IP 由（群密钥, PeerID）在 10.7.0.0/16
// 全池内确定性派生（见 serverless.DeriveVirtualIP），同群成员的 IP 几乎必然
// 散布在不同 /24 里——若按 /24 配置，跨 /24 成员的包会被系统路由送到物理
// 网关（表现为 ping 不通但探测正常）。/16 让整个派生域都指向 TUN。
func ConfigureTUN(name, ip string, prefixBits int) error {
	_ = prefixBits // 历史参数：掩码语义已固定为 /16（见上）
	switch runtime.GOOS {
	case "windows":
		return runCmd("netsh", "interface", "ip", "set", "address",
			"name="+name, "source=static", "addr="+ip, "mask=255.255.0.0")
	case "linux":
		return runCmd("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, 16), "dev", name)
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
