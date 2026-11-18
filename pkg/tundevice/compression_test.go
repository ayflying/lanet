package tundevice

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

func TestSmartCompressionFrames(t *testing.T) {
	ip := [4]byte{10, 7, 0, 2}
	random := make([]byte, 1200)
	_, _ = rand.New(rand.NewSource(1)).Read(random)
	packets := [][]byte{
		buildIPv4(ip, ip, []byte("small")),
		buildIPv4(ip, ip, bytes.Repeat([]byte("compressible text"), 60)),
		buildIPv4(ip, ip, random),
	}
	var stream bytes.Buffer
	var scratch []byte
	for i, pkt := range packets {
		frame, err := encodePacketFrame(scratch, pkt)
		if err != nil {
			t.Fatal(err)
		}
		scratch = frame
		if i == 1 && (frame[0] != 1 || len(frame) >= len(pkt)) || i != 1 && frame[0] != 0 {
			t.Fatalf("packet %d used unexpected frame: type=%d size=%d", i, frame[0], len(frame))
		}
		stream.Write(frame)
	}
	var fr packetFrameReader
	for i, pkt := range packets {
		got, err := fr.read(&stream)
		if err != nil || !bytes.Equal(got, pkt) {
			t.Fatalf("packet %d did not round-trip: %v", i, err)
		}
	}
}

func TestSmartCompressionRejectsInvalidFrames(t *testing.T) {
	tooLarge := binary.AppendUvarint(nil, maxPacketSize+1)
	oversized := append([]byte{1, 0, byte(len(tooLarge))}, tooLarge...)
	for _, data := range [][]byte{{2, 0, 1, 0}, {0, 0, 20, 0}, {1, 0, 1, 0}, oversized, append([]byte{0, 0, 20}, make([]byte, 20)...)} {
		var fr packetFrameReader
		if _, err := fr.read(bytes.NewReader(data)); err == nil {
			t.Fatalf("accepted invalid frame: %x", data)
		}
		if cap(fr.decoded) != 0 {
			t.Fatalf("invalid size caused decompression allocation: %x", data)
		}
	}
}

func TestSecureTunnelCompressionNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		aOff, bOff, legacy bool
		want               libprotocol.ID
	}{
		{"both enabled", false, false, false, protocol.TunnelSnappy},
		{"sender disabled", true, false, false, protocol.Tunnel},
		{"receiver disabled", false, true, false, protocol.Tunnel},
		{"both disabled", true, true, false, protocol.Tunnel},
		{"legacy peer", false, false, true, protocol.Tunnel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			ipA, ipB := [4]byte{10, 7, 0, 2}, [4]byte{10, 7, 0, 3}
			memA, _, err := NewMemory(1400)
			if err != nil {
				t.Fatal(err)
			}
			defer memA.Close()
			memB, _, err := NewMemory(1400)
			if err != nil {
				t.Fatal(err)
			}
			defer memB.Close()
			devA, devB := &recordingDevice{Device: memA}, &recordingDevice{Device: memB}
			ra := New(devA, tunnel.New(a, &stubNetmap{routes: map[string]peer.ID{"10.7.0.3": b.ID()}}, stubRelay{}))
			rb := New(devB, tunnel.New(b, &stubNetmap{routes: map[string]peer.ID{"10.7.0.2": a.ID()}}, stubRelay{}))
			defer ra.Close()
			defer rb.Close()
			if tc.aOff {
				ra.SetCompressionEnabled(false)
			}
			if tc.bOff {
				rb.SetCompressionEnabled(false)
			}
			ids := rb.InboundProtocols()
			if tc.legacy {
				ids = []libprotocol.ID{protocol.Tunnel}
			}
			for _, id := range ids {
				b.SetStreamHandler(id, func(s network.Stream) { rb.ServeInboundStream("10.7.0.2", s) })
			}
			if err = a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
				t.Fatal(err)
			}
			flow, err := ra.flowTo(ctx, b.ID(), "10.7.0.3")
			if err != nil {
				t.Fatal(err)
			}
			if got := flow.stream.Protocol(); got != tc.want {
				t.Fatalf("protocol=%s, want %s", got, tc.want)
			}
			switch got := flow.stream.Conn().ConnState().Security; got {
			case "/tls/1.0.0", "/noise":
			default:
				t.Fatalf("expected authenticated encryption, got %q", got)
			}
			request := buildIPv4(ipA, ipB, bytes.Repeat([]byte("request"), 80))
			reply := buildIPv4(ipB, ipA, bytes.Repeat([]byte("reply"), 80))
			if err := ra.forwardPacket(ctx, request); err != nil {
				t.Fatal(err)
			}
			waitTunnelPacket(t, ctx, devB, request)
			if err := rb.forwardPacket(ctx, reply); err != nil {
				t.Fatal(err)
			}
			waitTunnelPacket(t, ctx, devA, reply)
		})
	}
}

func waitTunnelPacket(t *testing.T, ctx context.Context, d *recordingDevice, want []byte) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, p := range d.snapshot() {
			if bytes.Equal(p, want) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("packet did not reach peer TUN")
		case <-tick.C:
		}
	}
}
