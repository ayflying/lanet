package serverless

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/libp2p/go-libp2p/core/peer"
	"io"
)

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
	return d.nearbyMAC("lanet-control-v1/"+string(d.protoControl)+"/"+role, requester, responder, binding)
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
