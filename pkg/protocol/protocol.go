package protocol

import "github.com/libp2p/go-libp2p/core/protocol"

const (
	Tunnel  protocol.ID = "/pvn/tunnel/1.0.0"
	Probe   protocol.ID = "/pvn/probe/1.0.0"
	Control protocol.ID = "/pvn/ctrl/1.0.0"
	// PortFWD 端口转发：流上首条消息为 2 字节大端长度 + 目标地址
	// （"ip:port" 文本），之后为双向原始字节。接收端 net.Dial
	// 目标地址后做透明搬运。
	PortFWD protocol.ID = "/pvn/portfwd/1.0.0"
)
