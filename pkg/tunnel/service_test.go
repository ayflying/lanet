package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	netmapclient "github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

type fakeNetmap struct {
	routes []netmapclient.Route
}

func (f *fakeNetmap) Refresh(ctx context.Context) (netmapclient.Snapshot, error) {
	return netmapclient.Snapshot{}, nil
}
func (f *fakeNetmap) Current() netmapclient.Snapshot { return netmapclient.Snapshot{} }
func (f *fakeNetmap) Routes() []netmapclient.Route   { return f.routes }
func (f *fakeNetmap) Resolve(virtualIP string) (netmapclient.Route, bool) {
	for _, route := range f.routes {
		if route.VirtualIP == virtualIP {
			return route, true
		}
	}
	return netmapclient.Route{}, false
}
func (f *fakeNetmap) Announce(ctx context.Context, addrs []string) error  { return nil }
func (f *fakeNetmap) RunLoop(ctx context.Context, interval time.Duration) {}

type fakeRelaySource struct{}

func (fakeRelaySource) Candidates(ctx context.Context, number int) ([]peer.AddrInfo, error) {
	return nil, nil
}

func newTestHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("create test host: %v", err)
	}
	return h
}

func TestOpenStreamRejectsUnknownVirtualIP(t *testing.T) {
	self := newTestHost(t)
	defer self.Close()
	service := New(self, &fakeNetmap{}, fakeRelaySource{})
	if _, _, err := service.OpenStreamToVirtualIP(context.Background(), "10.7.0.99"); err == nil {
		t.Fatal("expected unknown virtual IP to be rejected")
	}
}

func TestLastPathUsedDefaultsToUnknown(t *testing.T) {
	service := New(newTestHost(t), &fakeNetmap{}, fakeRelaySource{})
	if got := service.LastPathUsed("peer-x"); got != "unknown" {
		t.Fatalf("last path = %s, want unknown", got)
	}
}

func TestHasCircuitDetectsRelayAddress(t *testing.T) {
	direct, err := ma.NewMultiaddr("/ip4/203.0.113.5/udp/4001/quic-v1")
	if err != nil {
		t.Fatalf("parse direct address: %v", err)
	}
	if hasCircuit(direct) {
		t.Fatal("direct address should not contain circuit")
	}

	relayPeerID := newTestHost(t).ID().String()
	targetPeerID := newTestHost(t).ID().String()
	viaRelay, err := ma.NewMultiaddr("/ip4/203.0.113.5/tcp/443/p2p/" + relayPeerID + "/p2p-circuit/p2p/" + targetPeerID)
	if err != nil {
		t.Fatalf("parse circuit address: %v", err)
	}
	if !hasCircuit(viaRelay) {
		t.Fatal("circuit address should be detected")
	}
}

// TestCompactErrorFlattensDialError 回归：libp2p DialError 把每个失败地址展开为
// "  * [addr] cause" 一行，peerstore 十几个地址全部失败时单条日志十几行，probe
// 对不可达成员每轮重试会持续刷屏（本机实测 7.9 万行日志里拨号明细占绝大多数）。
// compactError 必须把它压成单行摘要，保留首行、地址总数与原因计数。
func TestCompactErrorFlattensDialError(t *testing.T) {
	multi := "failed to dial 12D3KooWTEST: all dials failed" +
		"\n  * [/ip4/220.203.161.59/udp/13113/quic-v1] dial refused because of black hole" +
		"\n  * [/ip6/240e:36f::38/tcp/56997] dial refused because of black hole" +
		"\n  * [/ip4/192.168.50.217/udp/61862] no transport for protocol" +
		"\n  * [/ip4/192.168.50.217/udp/62966] no transport for protocol"
	got := compactError(errors.New(multi))
	if strings.ContainsAny(got, "\n") {
		t.Fatalf("压缩结果必须为单行: %q", got)
	}
	if !strings.Contains(got, "4 个地址全部失败") {
		t.Fatalf("应包含地址总数 4: %q", got)
	}
	if !strings.Contains(got, "dial refused because of black hole×2") || !strings.Contains(got, "no transport for protocol×2") {
		t.Fatalf("应包含按次数聚合的原因: %q", got)
	}
	if !strings.HasPrefix(got, "failed to dial 12D3KooWTEST: all dials failed") {
		t.Fatalf("应保留首行: %q", got)
	}
}

// TestCompactErrorPlainAndLong 无多行明细的错误原样保留；超长截断。
func TestCompactErrorPlainAndLong(t *testing.T) {
	if got := compactError(nil); got != "<nil>" {
		t.Fatalf("nil 应为 <nil>: %q", got)
	}
	if got := compactError(errors.New("simple failure")); got != "simple failure" {
		t.Fatalf("单行错误应原样: %q", got)
	}
	if got := compactError(errors.New("a\nb")); strings.Contains(got, "\n") {
		t.Fatalf("非地址明细的多行应去行: %q", got)
	}
	long := strings.Repeat("x", 600)
	got := compactError(errors.New(long))
	if len(got) > 512+3 {
		t.Fatalf("超长应截断到 512: %d", len(got))
	}
}
