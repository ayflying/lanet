// TUN 入向包的统一防火墙判定：解析 IPv4 头，按「源虚拟 IP + 传输层协议 + 目标端口」
// 过 pkg/firewall。与 PortFWD、OnStream 共用同一套规则与模式。
package tundevice

import (
	"encoding/binary"
	"log"
	"net"

	"github.com/ayflying/pvn/pkg/firewall"
)

// CheckPacket 解析 IPv4 包并按防火墙判定是否放行（写回 TUN / 本机协议栈）。
// fw 为 nil 时放行（未启用防火墙）。判定依据取自包内字段：
//
//   - 源地址 = IP 头 bytes 12:16（即来源成员的虚拟 IP）；
//   - 协议 = IP 头 bytes 9（TCP=6 / UDP=17，其他协议视为 "other"）；
//   - 目标端口 = 传输层头前 2 字节（按 IHL 跳过 IP 选项）。
//
// 非 TCP/UDP 包（ICMP 等）无端口可匹配：allow-all 放行，
// deny-all / allow-list 拒绝（可用 allow-list + Port "*" 但无法区分协议）。
func CheckPacket(fw *firewall.Firewall, packet []byte) bool {
	if fw == nil {
		return true // 未启用防火墙：放行
	}
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return false
	}
	src := net.IP(packet[12:16]).String()
	proto := "other"
	var hasPort bool
	switch packet[9] {
	case 6:
		proto, hasPort = firewall.ProtoTCP, true
	case 17:
		proto, hasPort = firewall.ProtoUDP, true
	}
	port := 0
	if hasPort {
		ihl := int(packet[0]&0x0f) * 4
		if len(packet) < ihl+2 {
			return false
		}
		port = int(binary.BigEndian.Uint16(packet[ihl : ihl+2]))
	}
	return fw.Allow(src, proto, port)
}

// dropLog 限频打印拒绝日志（避免洪水刷屏）。
var dropLog = newDropLogger()

func newDropLogger() func(src, proto string, port int) {
	var (
		count int
		last  string
	)
	return func(src, proto string, port int) {
		count++
		key := src + proto
		if key != last || count%100 == 1 {
			log.Printf("[firewall] TUN 入向包被拒绝：来源=%s 协议=%s 端口=%d（累计 %d）", src, proto, port, count)
			last = key
		}
	}
}
