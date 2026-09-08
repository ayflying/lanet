package tundevice

import (
	"encoding/binary"
	"errors"
)

// ARP 应答器：Windows/macOS 的 TUN 是 L3 设备（无以太网层、无真实 MAC），
// 但 Windows 内核对 on-link 目的地址仍强制邻居（ARP）解析，而 Wintun 不会
// 把应答送到内核 —— 默认必然解析失败，条目被打成 Unreachable，单播包
// 永远发不出去（组播/广播不受影响）。修复方式：用户态直接应答 ARP 请求，
// 让内核认为解析成功。
//
// 注意：Wintun Raw IP 模式下读写的是不含以太网头的纯 IP/ARP 帧。

const (
	arpHardwareEther = 1    // 硬件类型：以太网
	arpProtoIPv4     = 0x0800 // 协议类型：IPv4
	arpOpRequest     = 1
	arpOpReply       = 2
	arpFrameLen      = 28 // 纯 ARP 帧长（htype 2 + ptype 2 + hlen 1 + plen 1 + oper 2 + 双方 4+6+4+6）
)

var errNotARP = errors.New("not an arp frame")

// arpReplyFor 构造对 ARP 请求的应答帧，写入 dst 并返回应答长度。
// 本机虚拟 IP 由调用方传入（谁请求谁就是 sender，我们以本机身份应答）。
func parseARPRequest(frame []byte) (senderIP, targetIP [4]byte, err error) {
	if len(frame) < arpFrameLen {
		return senderIP, targetIP, errNotARP
	}
	if binary.BigEndian.Uint16(frame[0:2]) != arpHardwareEther ||
		binary.BigEndian.Uint16(frame[2:4]) != arpProtoIPv4 ||
		frame[4] != 6 || frame[5] != 4 {
		return senderIP, targetIP, errNotARP
	}
	if binary.BigEndian.Uint16(frame[6:8]) != arpOpRequest {
		return senderIP, targetIP, errNotARP
	}
	copy(senderIP[:], frame[14:18])
	copy(targetIP[:], frame[24:28])
	return senderIP, targetIP, nil
}

// buildARPReply 构造 ARP 应答帧写入 dst：我们是 target（被询问方），
// 请求里的 sender 就是提问者。
func buildARPReply(request []byte, dst []byte) int {
	// frame layout (raw, no ethernet):
	// [0:2]=htype [2:4]=ptype [4]=hlen [5]=plen [6:8]=oper
	// [8:14]=senderMAC(6) [14:18]=senderIP(4)
	// [18:24]=targetMAC(6) [24:28]=targetIP(4)
	reply := dst[:arpFrameLen]
	copy(reply, request[:arpFrameLen]) // 复制硬件/协议类型与双方地址
	binary.BigEndian.PutUint16(reply[6:8], arpOpReply)
	// 应答里 sender 字段=我们（被询问的 target），target 字段=提问者
	copy(reply[8:14], request[18:24]) // senderMAC = 请求里的 targetMAC（占位，通常为 0）
	copy(reply[14:18], request[24:28])
	copy(reply[18:24], request[8:14]) // targetMAC = 提问者 MAC
	copy(reply[24:28], request[14:18])
	return arpFrameLen
}

// isBroadcastMulticast 判断原始帧是否为广播/组播目的（IP 层判断）。
func isBroadcastMulticast(packet []byte) bool {
	if len(packet) < 20 {
		return false
	}
	dst := packet[16:20]
	if dst[0] == 224 || dst[0] == 239 { // 组播
		return true
	}
	// 广播：255.255.255.255 或 子网定向广播（尾字节 255 也常见）
	return dst[0] == 255 && dst[1] == 255 && dst[2] == 255 && dst[3] == 255
}
