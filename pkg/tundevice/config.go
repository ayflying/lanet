package tundevice

import (
	"fmt"
	"os"
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
		// 关键：接口地址配 /32，再加 on-link 大网段路由。
		// 若直接配 /16，Windows 会把发往同网段单播的包先做 ARP 邻居解析——
		// Wintun 是 L3 设备不会回 ARP，包全部卡死在 incomplete 邻居上
		// （表现为 ping 100% 丢、进程零日志、只有组播能进 TUN）。
		// 注意路由必须是 on-link（不带 nexthop）：gateway 语义的路由
		// （route add ... <网关IP>）会让内核对网关做 ARP 解析，同样卡死
		// （实测 netsh route 指向自身 IP 时邻居 10.7.x 永远 Probe/Unreachable）。
		// on-link 路由由 wintun 直收直发，不经过邻居子系统。
		// 先清掉旧地址（上次运行残留或"对象已存在"会让 set address 直接失败），
		// 失败不阻断——首次启动本来就没有旧地址。
		_ = runCmd("netsh", "interface", "ip", "delete", "address", "name="+name, "addr="+ip)
		if err := runCmd("netsh", "interface", "ip", "set", "address",
			"name="+name, "source=static", "addr="+ip, "mask=255.255.255.255"); err != nil {
			return err
		}
		// Wintun 是点对点 L3 接口。Windows 上聚合 /16 on-link 路由仍会触发邻居
		// 解析，目标最终变成 Unreachable；工作正常的 WireGuard 配置使用每目标
		// /32 on-link 路由。这里只清理历史聚合路由和错误邻居，成员 /32 路由由
		// EnsureRoute 随成员表增量写入。
		_ = runCmd("netsh", "interface", "ipv4", "delete", "neighbors", "interface="+name)
		_ = runCmd("netsh", "interface", "ipv4", "delete", "route",
			"prefix=10.7.0.0/16", "interface="+name)
		return nil
	case "linux":
		// 注意顺序：先配地址再 UP。Linux 下 `ip addr add` 不会自动拉起接口，
		// 缺少 ip link set up 会导致接口保持 DOWN——内核把发往 10.7/16 的
		// 出向包直接丢在接口上（表现为 ping 100% 丢包且进程零日志）。
		if err := runCmd("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, 16), "dev", name); err != nil {
			return err
		}
		return runCmd("ip", "link", "set", name, "up")
	case "darwin":
		return runCmd("ifconfig", name, ip, ip, "up")
	default:
		return fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// runCmd 执行系统命令并聚合输出，失败时带出命令与输出便于排障。
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	hideWindow(cmd) // GUI 进程调用控制台程序必须禁窗口，否则黑框狂闪抢焦点
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureRoute 为目的虚拟 IP 添加 per-IP /32 on-link 路由（仅 Windows 需要）。
//
// 与 NodeBabyLink（本机工作正常的 Wintun 隧道）配置完全对齐：每目的 /32
// on-link（NextHop 0.0.0.0）+ Permanent 邻居。/16 聚合 on-link 路由下
// Windows 对目的 IP 做邻居解析卡死（实测），per-IP /32 是实证可行路径。
func EnsureRoute(name, ip string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	add := exec.Command("netsh", "interface", "ipv4", "add", "route",
		"prefix="+ip+"/32", "interface="+name, "store=active", "metric=1")
	hideWindow(add)
	out, err := add.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	lower := strings.ToLower(outStr)
	// 已存在就是期望状态。绝不能周期性先删后加，否则每轮都会制造无路由窗口。
	if err != nil && !strings.Contains(outStr, "对象已存在") &&
		!strings.Contains(lower, "object already exists") && !strings.Contains(lower, "already exists") {
		return fmt.Errorf("netsh add route %s: %v: %s", ip, err, outStr)
	}
	return nil
}

// EnsureNeighbor 为目的虚拟 IP 写入 Permanent 假 MAC 邻居表项（仅 Windows 需要）。
//
// 原理：Windows 对 on-link 路由的目的 IP 仍会先做邻居（ARP）解析，而 Wintun 是
// L3 设备、没有二层、永远不会回 ARP——不处理的话单播包全部卡死在 Probe/Unreachable
// 邻居上（表现为 ping 100% 丢、进程零日志、只有组播能进 TUN）。
// 预先写入 Permanent 条目后内核视为"二层可达"，直接把包写进 Wintun 环形缓冲，
// 用户态立刻可读。Linux/macOS 内核把 TUN 当点对点链路，无需此处理。
//
// 实现优先更新现有条目，只有更新失败才删除重建：TUN 适配器重建后内核邻居
// 缓存可能失效，但不能在正常刷新期间反复制造删除/解析窗口。
func EnsureNeighbor(name, ip string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	// 排障开关：LANET_SKIP_NEIGHBOR=1 时完全不写邻居表，用于验证
	// 「不写邻居 vs 写邻居」两种情况下内核是否放行单播包。
	if os.Getenv("LANET_SKIP_NEIGHBOR") != "" {
		return nil
	}
	// netsh 对 L3 设备（Wintun）会静默丢弃链路层地址，留下"永久但无 MAC"的
	// 无效条目（包照样卡死在邻居层）。必须走原生 IP Helper API 写真实 MAC。
	if err := ensureNeighborNative(name, ip); err == nil {
		return nil
	} else {
		// 某些精简 Windows 环境没有完整的 IP Helper 行为，保留 netsh
		// 作为兼容回退。先删后加，避免把已有的 Unreachable 条目误判为成功。
		_ = runCmd("netsh", "interface", "ipv4", "delete", "neighbors",
			"interface="+name, "address="+ip)
		netshErr := runCmd("netsh", "interface", "ipv4", "add", "neighbors",
			"interface="+name, "address="+ip, "neighbor=aa-bb-cc-dd-ee-01", "store=active")
		if netshErr == nil {
			return nil
		}
		return fmt.Errorf("ip helper: %v; netsh fallback: %w", err, netshErr)
	}
}
