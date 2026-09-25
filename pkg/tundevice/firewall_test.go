package tundevice

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/ayflying/pvn/pkg/firewall"
)

func ipv6TCPPacket(src, dst net.IP, port uint16) []byte {
	packet := make([]byte, 42)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], 2)
	packet[6] = 6
	packet[7] = 64
	copy(packet[8:24], src.To16())
	copy(packet[24:40], dst.To16())
	binary.BigEndian.PutUint16(packet[40:42], port)
	return packet
}

func TestCheckPacketIPv6AllowAndReject(t *testing.T) {
	src := net.ParseIP("fd00::2")
	dst := net.ParseIP("fd00::1")
	packet := ipv6TCPPacket(src, dst, 443)

	f := firewall.New()
	f.Set(firewall.ModeAllowList, []firewall.Rule{{Source: "fd00::/8", Proto: firewall.ProtoTCP, Port: "443"}})
	if !CheckPacket(f, packet) {
		t.Fatal("IPv6 CIDR rule should allow matching TCP packet")
	}

	f.Set(firewall.ModeAllowList, []firewall.Rule{{Source: "fd01::/16", Proto: firewall.ProtoTCP, Port: "443"}})
	if CheckPacket(f, packet) {
		t.Fatal("non-matching IPv6 CIDR rule must reject packet")
	}

	f.Set(firewall.ModeAllowList, []firewall.Rule{{Source: "10.7.0.0/16", Proto: firewall.ProtoTCP, Port: "443"}})
	if CheckPacket(f, packet) {
		t.Fatal("IPv4-only rule must not allow IPv6 source")
	}
}

func TestCheckPacketIPv6ShortAndNonIP(t *testing.T) {
	f := firewall.New()
	f.Set(firewall.ModeAllowAll, nil)
	jumbogram := make([]byte, 40)
	jumbogram[0] = 0x60
	jumbogram[6] = 0 // Hop-by-Hop Options may carry Jumbo Payload option.
	if CheckPacket(f, jumbogram) {
		t.Fatal("IPv6 jumbogram option chain must be rejected")
	}
	for _, packet := range [][]byte{{}, {0x60, 0, 0}, append(make([]byte, 40), 0x60)} {
		if CheckPacket(f, packet) {
			t.Fatalf("malformed or non-IP packet must be rejected: %x", packet)
		}
	}

	shortTransport := ipv6TCPPacket(net.ParseIP("fd00::2"), net.ParseIP("fd00::1"), 0)
	shortTransport = shortTransport[:41]
	if CheckPacket(f, shortTransport) {
		t.Fatal("IPv6 TCP packet without full port header must be rejected")
	}
}

func TestFirewallPacketLogFields(t *testing.T) {
	ipv4 := make([]byte, 22)
	ipv4[0] = 0x45
	ipv4[9] = 17
	copy(ipv4[12:16], net.ParseIP("10.7.0.2").To4())
	binary.BigEndian.PutUint16(ipv4[20:22], 53)
	if src, proto, port := firewallPacketLogFields(ipv4); src != "10.7.0.2" || proto != firewall.ProtoUDP || port != 53 {
		t.Fatalf("IPv4 fields = (%q, %q, %d)", src, proto, port)
	}

	ipv6 := ipv6TCPPacket(net.ParseIP("fd00::2"), net.ParseIP("fd00::1"), 443)
	if src, proto, port := firewallPacketLogFields(ipv6); src != "fd00::2" || proto != firewall.ProtoTCP || port != 443 {
		t.Fatalf("IPv6 fields = (%q, %q, %d)", src, proto, port)
	}

	for _, packet := range [][]byte{nil, {0x60}, make([]byte, 20)} {
		src, proto, port := firewallPacketLogFields(packet)
		if src != "" || proto != "other" || port != 0 {
			t.Fatalf("malformed packet fields = (%q, %q, %d)", src, proto, port)
		}
	}
}

func TestCheckPacketIPv6ExtensionHeader(t *testing.T) {
	packet := make([]byte, 50)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], 10)
	packet[6] = 0
	copy(packet[8:24], net.ParseIP("fd00::2").To16())
	copy(packet[24:40], net.ParseIP("fd00::1").To16())
	packet[40] = 6
	packet[41] = 0
	binary.BigEndian.PutUint16(packet[48:50], 443)

	f := firewall.New()
	f.Set(firewall.ModeAllowList, []firewall.Rule{{Source: "fd00::2", Proto: firewall.ProtoTCP, Port: "443"}})
	if !CheckPacket(f, packet) {
		t.Fatal("IPv6 packet with hop-by-hop extension header should match")
	}
	if src, proto, port := firewallPacketLogFields(packet); src != "fd00::2" || proto != firewall.ProtoTCP || port != 443 {
		t.Fatalf("extension fields = (%q, %q, %d)", src, proto, port)
	}
}

func TestCheckPacketIPv6ExtensionBoundsAndFragments(t *testing.T) {
	f := firewall.New()
	f.Set(firewall.ModeAllowList, []firewall.Rule{{Source: "*", Proto: firewall.ProtoTCP, Port: "443"}})
	base := make([]byte, 40)
	base[0] = 0x60
	copy(base[8:24], net.ParseIP("fd00::2").To16())

	// Payload says 8 bytes, but the backing buffer is truncated.
	shortPayload := append([]byte(nil), base...)
	binary.BigEndian.PutUint16(shortPayload[4:6], 8)
	shortPayload[6] = 0
	if CheckPacket(f, shortPayload) {
		t.Fatal("truncated IPv6 payload must be rejected")
	}

	// Nine minimal extension headers exceed the parsing limit.
	chain := make([]byte, 40+9*8+2)
	copy(chain, base)
	binary.BigEndian.PutUint16(chain[4:6], uint16(len(chain)-40))
	chain[6] = 0
	for i := 0; i < 9; i++ {
		chain[40+i*8] = 0
	}
	chain[40+8*8] = 6
	binary.BigEndian.PutUint16(chain[len(chain)-2:], 443)
	if CheckPacket(f, chain) {
		t.Fatal("excessive IPv6 extension chain must be rejected")
	}
	if _, proto, port := firewallPacketLogFields(chain); proto != "other" || port != 0 {
		t.Fatalf("excessive-chain fields = (%q, %d)", proto, port)
	}

	fragment := make([]byte, 50)
	copy(fragment, base)
	binary.BigEndian.PutUint16(fragment[4:6], 10)
	fragment[6] = 44
	fragment[40] = 6
	binary.BigEndian.PutUint16(fragment[42:44], 0x40) // non-zero fragment offset (8 units)
	binary.BigEndian.PutUint16(fragment[48:50], 443)
	if CheckPacket(f, fragment) {
		t.Fatal("non-initial fragment must be rejected without a usable port")
	}
	if _, proto, port := firewallPacketLogFields(fragment); proto != "tcp" || port != 0 {
		t.Fatalf("non-initial-fragment fields = (%q, %d)", proto, port)
	}

	binary.BigEndian.PutUint16(fragment[42:44], 1) // first fragment, offset zero
	if !CheckPacket(f, fragment) {
		t.Fatal("first fragment with available TCP port should follow allow-all mode")
	}
}
