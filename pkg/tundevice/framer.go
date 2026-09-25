package tundevice

import "encoding/binary"

// ipFramer 把隧道字节流切成一个个完整 IPv4/IPv6 包。
//
// libp2p stream 是字节流（yamux/mplex），对端连续 Write 多个 IP 包时会被
// 粘包：一次 Read 可能拿到 N 个包拼成的整块。直接把它写进 TUN，等于向内核
// 提交一个「IP 头声明 84 字节、实际 420 字节」的畸形包，协议栈直接丢弃
// （表现为 ping 100% 丢包，且日志里能看到 inbound wrote 420 bytes 这类
// 非正常长度）。必须按 IPv4 头的 total_length 逐包切分后再写入。
//
// 典型症状：5 个 ping 包（84B）被粘成 420B，Windows 端一个都不回。
type ipFramer struct {
	buf []byte
}

// feed 追加新读到的字节，返回其中已完整的 IP 包。
// 返回的切片指向内部缓冲，调用方必须在下一次 feed 之前使用完毕（写入 TUN）。
func (f *ipFramer) feed(b []byte) [][]byte {
	if len(b) > 0 {
		f.buf = append(f.buf, b...)
	}
	var out [][]byte
	for len(f.buf) > 0 {
		version := f.buf[0] >> 4
		headerLen := 0
		total := 0
		switch version {
		case 4:
			if len(f.buf) < 20 {
				break
			}
			headerLen = int(f.buf[0]&0x0f) * 4
			if headerLen < 20 {
				f.buf = f.buf[:0]
				return out
			}
			if len(f.buf) < headerLen {
				break
			}
			total = int(binary.BigEndian.Uint16(f.buf[2:4]))
			if total < headerLen || total > maxPacketSize {
				f.buf = f.buf[:0]
				return out
			}
		case 6:
			if len(f.buf) < 40 {
				break
			}
			headerLen = 40
			payloadLen := int(binary.BigEndian.Uint16(f.buf[4:6]))
			if payloadLen == 0 && f.buf[6] == 0 {
				// Hop-by-Hop Options 可能包含 Jumbo Payload option；本实现不支持
				// 大于 65535 字节的 jumbogram，无法安全确定帧边界，故拒绝该流。
				f.buf = f.buf[:0]
				return out
			}
			total = headerLen + payloadLen
			if total > maxPacketSize {
				f.buf = f.buf[:0]
				return out
			}
		default:
			f.buf = f.buf[:0]
			return out
		}
		if total == 0 || len(f.buf) < total {
			break
		}
		out = append(out, f.buf[:total])
		f.buf = f.buf[total:]
	}
	if len(f.buf) > maxPacketSize {
		f.buf = f.buf[:0]
	}
	return out
}
