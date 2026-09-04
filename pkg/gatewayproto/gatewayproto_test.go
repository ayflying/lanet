package gatewayproto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMarshalUnmarshalRoundtrip(t *testing.T) {
	cases := []Frame{
		{Type: TypeAuth, Payload: []byte(`{"invite_code":"ABC","name":"n1","mode":"client"}`)},
		{Type: TypeData, StreamID: 42, Payload: []byte("hello lanet")},
		{Type: TypeClose, StreamID: 7},
		{Type: TypePong, Payload: []byte{1, 2, 3}},
	}
	for _, f := range cases {
		got, err := Unmarshal(Marshal(f))
		if err != nil {
			t.Fatalf("type %d: %v", f.Type, err)
		}
		if got.Type != f.Type || got.StreamID != f.StreamID || !bytes.Equal(got.Payload, f.Payload) {
			t.Fatalf("roundtrip mismatch: %+v != %+v", got, f)
		}
	}
}

func TestUnmarshalTruncated(t *testing.T) {
	if _, err := Unmarshal([]byte{0x01, 0x00}); err == nil {
		t.Fatal("短帧应报错")
	}
	full := Marshal(Frame{Type: TypeData, StreamID: 1, Payload: make([]byte, 100)})
	if _, err := Unmarshal(full[:15]); err == nil {
		t.Fatal("payload 截断应报错")
	}
}

func TestHeaderLayout(t *testing.T) {
	// 布局契约：[type:1][streamID:4][len:4]，三端 SDK 依赖此顺序。
	f := Frame{Type: 0xAB, StreamID: 0x11223344, Payload: []byte("xy")}
	b := Marshal(f)
	if b[0] != 0xAB {
		t.Fatalf("type 字节错位: %#x", b[0])
	}
	if got := binary.BigEndian.Uint32(b[1:5]); got != 0x11223344 {
		t.Fatalf("streamID 错位: %#x", got)
	}
	if got := binary.BigEndian.Uint32(b[5:9]); got != 2 {
		t.Fatalf("长度错位: %d", got)
	}
}
