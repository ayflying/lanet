// TUN 入向包的统一防火墙判定：解析 IP 头，按「源虚拟 IP + 传输层协议 + 目标端口」
// 过 pkg/firewall。与 PortFWD、OnStream 共用同一套规则与模式。
package tundevice

import (
	"encoding/binary"
	"log"
	"net"

	"github.com/ayflying/pvn/pkg/firewall"
)

// CheckPacket 解析 IPv4 或 IPv6 包并按防火墙判定是否放行（写回 TUN / 本机协议栈）。
// fw 为 nil 时放行（未启用防火墙）。非 TCP/UDP 包无端口可匹配：allow-all 放行，
// deny-all / allow-list 拒绝。
func CheckPacket(fw *firewall.Firewall, packet []byte) bool {
	if fw == nil {
		return true // 未启用防火墙：放行
	}
	if len(packet) == 0 {
		return false
	}

	var src net.IP
	var nextHeader byte
	var transportOffset int
	ipv6FragmentNonInitial := false
	ipv6PayloadEnd := 0
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return false
		}
		ihl := int(packet[0]&0x0f) * 4
		if ihl < 20 || len(packet) < ihl {
			return false
		}
		src = net.IP(packet[12:16])
		nextHeader = packet[9]
		transportOffset = ihl
	case 6:
		if len(packet) < 40 {
			return false
		}
		src = net.IP(packet[8:24])
		payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
		if payloadLen == 0 && packet[6] == 0 {
			return false // jumbogram option chains are not supported
		}
		ipv6PayloadEnd = 40 + payloadLen
		if ipv6PayloadEnd > len(packet) {
			return false
		}
		nextHeader = packet[6]
		transportOffset = 40
		// Bound extension-header parsing to prevent pathological chains.
		for count := 0; nextHeader == 0 || nextHeader == 43 || nextHeader == 44 || nextHeader == 60 || nextHeader == 51 || nextHeader == 135 || nextHeader == 139 || nextHeader == 140; count++ {
			if count >= 8 || ipv6PayloadEnd < transportOffset+2 {
				return false
			}
			headerLen := (int(packet[transportOffset+1]) + 1) * 8
			if nextHeader == 44 {
				headerLen = 8
				ipv6FragmentNonInitial = binary.BigEndian.Uint16(packet[transportOffset+2:transportOffset+4])&0xfff8 != 0
			} else if nextHeader == 51 {
				headerLen = (int(packet[transportOffset+1]) + 2) * 4
			}
			if ipv6PayloadEnd < transportOffset+headerLen {
				return false
			}
			nextHeader = packet[transportOffset]
			transportOffset += headerLen
		}
	default:
		return false
	}

	proto := "other"
	var hasPort bool
	switch nextHeader {
	case 6:
		proto, hasPort = firewall.ProtoTCP, true
	case 17:
		proto, hasPort = firewall.ProtoUDP, true
	}
	port := 0
	if hasPort {
		if ipv6FragmentNonInitial {
			return fw.Allow(src.String(), "other", 0)
		}
		if len(packet) < transportOffset+2 || (ipv6PayloadEnd != 0 && ipv6PayloadEnd < transportOffset+2) {
			return false
		}
		port = int(binary.BigEndian.Uint16(packet[transportOffset : transportOffset+2]))
	}
	return fw.Allow(src.String(), proto, port)
}

// firewallPacketLogFields 从 IPv4/IPv6 包安全提取防火墙拒绝日志字段。
// 畸形包返回空来源、协议 other 和端口 0，不进行越界读取。
func firewallPacketLogFields(packet []byte) (src, proto string, port int) {
	proto = "other"
	if len(packet) == 0 {
		return "", proto, 0
	}

	var nextHeader byte
	var transportOffset int
	ipv6FragmentNonInitial := false
	ipv6PayloadEnd := 0
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return "", proto, 0
		}
		ihl := int(packet[0]&0x0f) * 4
		if ihl < 20 || len(packet) < ihl {
			return "", proto, 0
		}
		src = net.IP(packet[12:16]).String()
		nextHeader = packet[9]
		transportOffset = ihl
	case 6:
		if len(packet) < 40 {
			return "", proto, 0
		}
		src = net.IP(packet[8:24]).String()
		ipv6PayloadEnd = 40 + int(binary.BigEndian.Uint16(packet[4:6]))
		if ipv6PayloadEnd > len(packet) {
			return src, proto, 0
		}
		nextHeader = packet[6]
		transportOffset = 40
		for count := 0; nextHeader == 0 || nextHeader == 43 || nextHeader == 44 || nextHeader == 60 || nextHeader == 51 || nextHeader == 135 || nextHeader == 139 || nextHeader == 140; count++ {
			if count >= 8 || ipv6PayloadEnd < transportOffset+2 {
				return src, proto, 0
			}
			headerLen := (int(packet[transportOffset+1]) + 1) * 8
			if nextHeader == 44 {
				headerLen = 8
				ipv6FragmentNonInitial = binary.BigEndian.Uint16(packet[transportOffset+2:transportOffset+4])&0xfff8 != 0
			} else if nextHeader == 51 {
				headerLen = (int(packet[transportOffset+1]) + 2) * 4
			}
			if ipv6PayloadEnd < transportOffset+headerLen {
				return src, proto, 0
			}
			nextHeader = packet[transportOffset]
			transportOffset += headerLen
		}
	default:
		return "", proto, 0
	}

	switch nextHeader {
	case 6:
		proto = firewall.ProtoTCP
	case 17:
		proto = firewall.ProtoUDP
	default:
		return src, proto, 0
	}
	if ipv6FragmentNonInitial || len(packet) < transportOffset+2 || (ipv6PayloadEnd != 0 && ipv6PayloadEnd < transportOffset+2) {
		return src, proto, 0
	}
	return src, proto, int(binary.BigEndian.Uint16(packet[transportOffset : transportOffset+2]))
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
