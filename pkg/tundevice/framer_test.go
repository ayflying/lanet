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
