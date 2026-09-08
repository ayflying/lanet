package tundevice

import "encoding/binary"

// ipFramer 把隧道字节流切成一个个完整 IPv4 包。
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
	for len(f.buf) >= 20 {
		if f.buf[0]>>4 != 4 {
			// 非 IPv4（隧道里混入了别的东西）：丢弃缓冲，避免整条流错乱
			f.buf = f.buf[:0]
			break
		}
		total := int(binary.BigEndian.Uint16(f.buf[2:4]))
		if total < 20 || total > maxPacketSize {
			// 非法长度：流已错乱，丢弃缓冲重新同步
			f.buf = f.buf[:0]
			break
		}
		if len(f.buf) < total {
			break // 包未收全，等下一批数据
		}
		out = append(out, f.buf[:total])
		f.buf = f.buf[total:]
	}
	// 长期凑不出一个完整包说明流已错乱，防止缓冲无限增长
	if len(f.buf) > maxPacketSize {
		f.buf = f.buf[:0]
	}
	return out
}
