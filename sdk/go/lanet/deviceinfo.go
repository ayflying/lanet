package lanet

import (
	"fmt"
	"net"
)

// collectLocalIPs 枚举本机所有非回环网卡的活动 IP（含掩码位数，如
// 192.168.50.100/24）。排除回环与链路本地地址（169.254.x.x、fe80::/10）——
// 它们对识别设备无用。结果随 info 协议交换给同群成员，供对方在控制台
// 「详情」弹框里识别这是哪台机器、归属哪个网段。
func collectLocalIPs() []string {
	ips := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue // 回环网卡整块跳过
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				if ipn.IP.IsLinkLocalUnicast() {
					continue // fe80::/10 链路本地
				}
			} else if ip4.IsLinkLocalUnicast() {
				continue // 169.254.x.x 自动专用地址
			}
			mask, _ := ipn.Mask.Size()
			ips = append(ips, fmt.Sprintf("%s/%d", ipn.IP, mask))
		}
	}
	return ips
}
