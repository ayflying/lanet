package tundevice

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestPacketDestinationIPv4Regression(t *testing.T) {
	packet := buildIPv4([4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}, nil)
	got, err := packetDestination(packet)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.0.0.2" {
		t.Fatalf("destination = %q, want 10.0.0.2", got)
	}
}

func TestEnqueuePacketIPv6Destination(t *testing.T) {
	r := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  2,
		writeBudgetMax: outboundQueueBudget,
		outboundIdle:   time.Hour,
	}
	forwarded := make(chan []byte, 1)
	r.forwardImpl = func(_ context.Context, packet []byte) error {
		forwarded <- packet
		return nil
	}
	packet := make([]byte, 40)
	packet[0] = 0x60
	copy(packet[24:40], netip.MustParseAddr("fd00:6c61:6e65::1234").AsSlice())
	if err := r.enqueuePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.outbound["fd00:6c61:6e65::1234"]; !ok {
		t.Fatal("IPv6 destination was not used as outbound map key")
	}
	select {
	case got := <-forwarded:
		if len(got) != len(packet) {
			t.Fatalf("forwarded length = %d, want %d", len(got), len(packet))
		}
	case <-time.After(time.Second):
		t.Fatal("IPv6 packet was not dispatched")
	}
	r.Close()
}

func TestEnqueuePacketOtherULAIsDropped(t *testing.T) {
	r := &Router{
		outbound:       make(map[string]*outboundWorker),
		outboundLimit:  2,
		writeBudgetMax: outboundQueueBudget,
		outboundIdle:   time.Hour,
	}
	forwarded := make(chan []byte, 1)
	r.forwardImpl = func(_ context.Context, packet []byte) error {
		forwarded <- packet
		return nil
	}
	packet := make([]byte, 40)
	packet[0] = 0x60
	copy(packet[24:40], netip.MustParseAddr("fd12:3456::1").AsSlice())
	if err := r.enqueuePacket(context.Background(), packet); err != nil {
		t.Fatal(err)
	}
	if len(r.outbound) != 0 {
		t.Fatalf("outbound workers = %d, want 0", len(r.outbound))
	}
	select {
	case <-forwarded:
		t.Fatal("IPv6 packet with unrelated ULA destination was forwarded")
	case <-time.After(50 * time.Millisecond):
	}
	r.Close()
}

func TestPacketDestinationMalformedIPv6(t *testing.T) {
	cases := [][]byte{
		{0x60},
		make([]byte, 39),
	}
	cases[1][0] = 0x60
	truncated := make([]byte, 40)
	truncated[0] = 0x60
	truncated[5] = 1
	cases = append(cases, truncated)
	jumbogramMarker := make([]byte, 40)
	jumbogramMarker[0] = 0x60
	jumbogramMarker[6] = 0
	cases = append(cases, jumbogramMarker)
	for i, packet := range cases {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("case %d panicked: %v", i, recovered)
				}
			}()
			got, err := packetDestination(packet)
			if i < 3 {
				if err == nil {
					t.Errorf("case %d: expected malformed packet error", i)
				}
				return
			}
			if err != nil {
				t.Errorf("case %d: expected no error for empty IPv6 packet: %v", i, err)
			}
			if got != "::" {
				t.Errorf("case %d: IPv6 destination=%q, want ::", i, got)
			}
		}()
	}
}

func TestForwardPacketUnsupportedVersionDrops(t *testing.T) {
	r := &Router{}
	if err := r.forwardPacket(context.Background(), []byte{0x70}); err != nil {
		t.Fatalf("unsupported version returned error: %v", err)
	}
}
