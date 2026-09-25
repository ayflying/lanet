package tundevice

import "testing"

func TestIPFramerSplitsCoalescedPackets(t *testing.T) {
	first := buildIPv4ForFramer([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("one"))
	second := buildIPv4ForFramer([4]byte{10, 7, 0, 3}, [4]byte{10, 7, 0, 2}, []byte("two"))
	framer := &ipFramer{}

	packets := framer.feed(append(append([]byte{}, first...), second...))
	if len(packets) != 2 {
		t.Fatalf("got %d packets, want 2", len(packets))
	}
	if string(packets[0][20:]) != "one" || string(packets[1][20:]) != "two" {
		t.Fatalf("unexpected payloads: %q and %q", packets[0][20:], packets[1][20:])
	}
}

func TestIPFramerKeepsPartialPacket(t *testing.T) {
	packet := buildIPv4ForFramer([4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}, []byte("split"))
	framer := &ipFramer{}

	if got := framer.feed(packet[:7]); len(got) != 0 {
		t.Fatalf("partial packet emitted %d packets", len(got))
	}
	got := framer.feed(packet[7:])
	if len(got) != 1 || string(got[0][20:]) != "split" {
		t.Fatalf("partial packet was not reassembled: %#v", got)
	}
}

func TestIPFramerIPv6SplitsAndReassembles(t *testing.T) {
	first := buildIPv6ForFramer([]byte("one"))
	second := buildIPv6ForFramer([]byte("two"))
	framer := &ipFramer{}

	packets := framer.feed(append(append([]byte{}, first...), second...))
	if len(packets) != 2 || string(packets[0][40:]) != "one" || string(packets[1][40:]) != "two" {
		t.Fatalf("unexpected IPv6 packets: %#v", packets)
	}

	framer = &ipFramer{}
	if got := framer.feed(first[:39]); len(got) != 0 {
		t.Fatalf("truncated IPv6 header emitted %d packets", len(got))
	}
	if got := framer.feed(first[39:]); len(got) != 1 || string(got[0][40:]) != "one" {
		t.Fatalf("split IPv6 packet was not reassembled: %#v", got)
	}
}

func TestIPFramerRejectsInvalidIPv6PayloadLength(t *testing.T) {
	packet := buildIPv6ForFramer([]byte("x"))
	packet[4], packet[5] = 0xff, 0xff
	framer := &ipFramer{}
	if got := framer.feed(packet); len(got) != 0 {
		t.Fatalf("invalid IPv6 length emitted %d packets", len(got))
	}
	if len(framer.buf) != 0 {
		t.Fatal("invalid IPv6 length left buffered data")
	}
}

func TestIPFramerRejectsIPv6JumbogramOptions(t *testing.T) {
	packet := make([]byte, 40)
	packet[0] = 0x60
	packet[6] = 0 // Hop-by-Hop Options may carry Jumbo Payload option.
	framer := &ipFramer{}
	if got := framer.feed(packet); len(got) != 0 {
		t.Fatalf("unsupported IPv6 jumbogram marker emitted %d packets", len(got))
	}
}

func buildIPv6ForFramer(payload []byte) []byte {
	packet := make([]byte, 40+len(payload))
	packet[0] = 0x60
	packet[4] = byte(len(payload) >> 8)
	packet[5] = byte(len(payload))
	packet[6] = 17
	packet[7] = 64
	copy(packet[40:], payload)
	return packet
}

func buildIPv4ForFramer(src, dst [4]byte, payload []byte) []byte {
	totalLen := 20 + len(payload)
	packet := make([]byte, totalLen)
	packet[0] = 0x45
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], src[:])
	copy(packet[16:20], dst[:])
	copy(packet[20:], payload)
	return packet
}
