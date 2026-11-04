package serverless

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/libp2p/go-libp2p/core/peer"
	"io"
)

// groupMAC 用群密钥对「域 + 双方身份 + 载荷」做 HMAC-SHA256。
//
// 双向证明均绑定经过 libp2p 连接认证的双方 ID；长度前缀使可变字段的编码无
// 歧义，域分隔保证不同用途（控制协作的不同角色/方向）互不通用。
// 原本与「附近」探测共用（叫 nearbyMAC），附近功能删除后只剩控制协作使用。
func (d *Discovery) groupMAC(domain string, requester, responder peer.ID, payload []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, d.groupKey)
	for _, field := range [][]byte{[]byte(domain), []byte(requester), []byte(responder), payload} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write(field)
	}
	var out [sha256.Size]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// 控制协作帧有严格长度上限；证明绑定协议、双方身份、双向挑战及完整地址载荷。
func controlWrite(w io.Writer, body []byte) error {
	if len(body) > controlCollabMessage {
		return errors.New("control: message too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	for _, b := range [][]byte{header[:], body} {
		for len(b) > 0 {
			n, err := w.Write(b)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
	}
	return nil
}
func controlRead(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > controlCollabMessage {
		return nil, errors.New("control: message too large")
	}
	body := make([]byte, int(n))
	_, err := io.ReadFull(r, body)
	return body, err
}
func (d *Discovery) controlMAC(role string, requester, responder peer.ID, transcript, payload []byte) [sha256.Size]byte {
	binding := make([]byte, 0, len(transcript)+len(payload))
	binding = append(binding, transcript...)
	binding = append(binding, payload...)
	return d.groupMAC("lanet-control-v1/"+string(d.protoControl)+"/"+role, requester, responder, binding)
}
func (d *Discovery) controlPayload(role string, requester, responder peer.ID, transcript []byte) ([]byte, error) {
	raw, err := json.Marshal(d.controlHints())
	if err != nil {
		return nil, err
	}
	proof := d.controlMAC(role, requester, responder, transcript, raw)
	return append(proof[:], raw...), nil
}
func (d *Discovery) verifyControlPayload(body []byte, role string, requester, responder peer.ID, transcript []byte, expand bool) error {
	if len(body) < sha256.Size {
		return errors.New("control: missing proof")
	}
	want := d.controlMAC(role, requester, responder, transcript, body[sha256.Size:])
	if subtle.ConstantTimeCompare(body[:sha256.Size], want[:]) != 1 {
		return errors.New("control: invalid proof")
	}
	return d.acceptControlHints(body[sha256.Size:], expand)
}
