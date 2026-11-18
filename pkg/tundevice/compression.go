package tundevice

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/golang/snappy"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

const (
	packetFrameHeader = 3 // 类型（0 原包 / 1 Snappy 块）+ 两字节大端载荷长度
	minCompressSize   = 256
)

// SetCompressionEnabled 在启动时、注册流处理器和运行 Router 之前调用。
// 关闭时不发起也不接受压缩协议，现有加密传输不受影响。
func (r *Router) SetCompressionEnabled(enabled bool) { r.disableCompression = !enabled }

// InboundProtocols 返回应接到同一成员校验处理器的协议列表，也用于出向协商。
// 仅双方都支持且允许压缩时才匹配新版，其他情况继续走原有加密隧道。
func (r *Router) InboundProtocols() []libprotocol.ID {
	if r.disableCompression {
		return []libprotocol.ID{protocol.Tunnel}
	}
	return []libprotocol.ID{protocol.TunnelSnappy, protocol.Tunnel}
}

// 每个包独立压缩，不跨包共享字典。小包直接走原包帧；压缩节省的字节
// 必须抵消整个帧头才采用。复用写侧缓冲，一次 Write 发完整帧。
func encodePacketFrame(buf, packet []byte) ([]byte, error) {
	if len(packet) < 20 || len(packet) > maxPacketSize {
		return nil, fmt.Errorf("invalid tunnel IP packet size: %d", len(packet))
	}
	size := len(packet)
	if size >= minCompressSize {
		size = snappy.MaxEncodedLen(size)
	}
	if cap(buf) < packetFrameHeader+size {
		buf = make([]byte, packetFrameHeader+size)
	} else {
		buf = buf[:packetFrameHeader+size]
	}
	kind := byte(0)
	if len(packet) >= minCompressSize {
		encoded := snappy.Encode(buf[packetFrameHeader:], packet)
		if len(encoded)+packetFrameHeader < len(packet) {
			kind, size = 1, len(encoded)
		}
	}
	if kind == 0 {
		size = len(packet)
		copy(buf[packetFrameHeader:], packet)
	}
	buf[0] = kind
	binary.BigEndian.PutUint16(buf[1:packetFrameHeader], uint16(size))
	return buf[:packetFrameHeader+size], nil
}

// 调用方持有 flow.writeMu，避免同一双工流上的帧交错。
func (flow *peerFlow) writePacket(packet []byte) error {
	wire := packet
	if flow.stream.Protocol() == protocol.TunnelSnappy {
		frame, err := encodePacketFrame(flow.frameWriteBuf, packet)
		if err != nil {
			return err
		}
		flow.frameWriteBuf = frame
		wire = frame
	}
	n, err := flow.stream.Write(wire)
	if err == nil && n != len(wire) {
		return io.ErrShortWrite
	}
	return err
}

type packetFrameReader struct {
	header  [packetFrameHeader]byte
	wire    []byte
	decoded []byte
}

func (f *packetFrameReader) read(r io.Reader) ([]byte, error) {
	if _, err := io.ReadFull(r, f.header[:]); err != nil {
		return nil, err
	}
	kind := f.header[0]
	size := int(binary.BigEndian.Uint16(f.header[1:]))
	if kind > 1 || size == 0 || (kind == 0 && size < 20) {
		return nil, fmt.Errorf("invalid tunnel frame type or size")
	}
	if cap(f.wire) < size {
		f.wire = make([]byte, size)
	} else {
		f.wire = f.wire[:size]
	}
	if _, err := io.ReadFull(r, f.wire); err != nil {
		return nil, err
	}
	packet := f.wire
	if kind == 1 {
		// 在任何解压分配前检查 Snappy 声明的输出大小，防止解压放大突破 IP 上限。
		decodedSize, err := snappy.DecodedLen(packet)
		if err != nil || decodedSize > maxPacketSize || decodedSize < minCompressSize || decodedSize <= size+packetFrameHeader {
			return nil, fmt.Errorf("invalid compressed tunnel packet size")
		}
		f.decoded, err = snappy.Decode(f.decoded[:cap(f.decoded)], packet)
		if err != nil {
			return nil, fmt.Errorf("decode tunnel packet: %w", err)
		}
		packet = f.decoded
	}
	if !validFramedIPPacket(packet) {
		return nil, fmt.Errorf("tunnel frame does not contain exactly one IP packet")
	}
	return packet, nil
}

func validFramedIPPacket(packet []byte) bool {
	switch packet[0] >> 4 {
	case 4:
		headerSize := int(packet[0]&15) * 4
		return headerSize >= 20 && headerSize <= len(packet) && int(binary.BigEndian.Uint16(packet[2:4])) == len(packet)
	case 6:
		return len(packet) >= 40 && !(len(packet) == 40 && packet[6] == 0) && 40+int(binary.BigEndian.Uint16(packet[4:6])) == len(packet)
	default:
		return false
	}
}
