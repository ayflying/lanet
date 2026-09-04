// Package gatewayproto 定义 ws-gateway 的 WebSocket 帧协议。
//
// 一条二进制 WebSocket 消息即一个帧，布局固定：
//
//	[type:1 字节][streamID:4 字节大端][payload 长度:4 字节大端][payload]
//
// 各端（Go / C# / uniapp）按同一布局实现编解码，保持字节级兼容。
package gatewayproto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 帧类型。
const (
	TypeAuth    byte = 0x01 // c→g  鉴权：JSON {"invite_code","name","mode"}
	TypeAuthOk  byte = 0x02 // g→c  鉴权通过：JSON {"virtual_ip","peer_id","group"}
	TypeAuthErr byte = 0x03 // g→c  鉴权失败：JSON {"error"}
	TypeDial    byte = 0x04 // c→g  开流：JSON {"ip","port","protocol"}；protocol 空=端口转发(ip:port)，非空=按协议直开流
	TypeDialOk  byte = 0x05 // g→c  开流成功：streamID 生效，payload JSON {"via_relay":bool}
	TypeDialErr byte = 0x06 // g→c  开流失败：streamID 生效，payload 为错误文本
	TypeData    byte = 0x07 // 双向  流数据
	TypeClose   byte = 0x08 // 双向  半关闭写端（对端读到 EOF）
	TypeReset   byte = 0x09 // 双向  强制中止流
	TypePing    byte = 0x0A // c→g  心跳（payload 原样回 Pong）
	TypePong    byte = 0x0B // g→c  心跳应答
	// TypeStreamOpen g→c  网关把网格内入向流推给 service 模式连接：
	// streamID 生效，payload JSON {"protocol","remote_peer"}。
	TypeStreamOpen byte = 0x0C
)

// 连接模式。
const (
	ModeClient  = "client"  // 主动开流访问网格内服务
	ModeService = "service" // 接收网格内入向流（网关同一时刻仅接受一个）
)

const headerSize = 9

// ErrTruncated 帧不完整。
var ErrTruncated = errors.New("gatewayproto: 帧不完整")

// Frame 一个协议帧。
type Frame struct {
	Type     byte
	StreamID uint32
	Payload  []byte
}

// Marshal 编码为二进制（每次调用分配独立缓冲，可直接作为 WS 消息发送）。
func Marshal(f Frame) []byte {
	buf := make([]byte, headerSize+len(f.Payload))
	buf[0] = f.Type
	binary.BigEndian.PutUint32(buf[1:5], f.StreamID)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(f.Payload)))
	copy(buf[headerSize:], f.Payload)
	return buf
}

// Unmarshal 从一条完整的 WS 消息解码帧。
func Unmarshal(data []byte) (Frame, error) {
	if len(data) < headerSize {
		return Frame{}, ErrTruncated
	}
	n := binary.BigEndian.Uint32(data[5:9])
	if uint64(len(data)-headerSize) < uint64(n) {
		return Frame{}, fmt.Errorf("%w: 需要 %d 字节，实际 %d", ErrTruncated, n, len(data)-headerSize)
	}
	return Frame{
		Type:     data[0],
		StreamID: binary.BigEndian.Uint32(data[1:5]),
		Payload:  data[headerSize : headerSize+int(n)],
	}, nil
}
