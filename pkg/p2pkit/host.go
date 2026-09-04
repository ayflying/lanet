package p2pkit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

type HostSpec struct {
	ListenAddrs  []string
	RelayService bool
	RelaySource  autorelay.PeerSource
	UserAgent    string
	// WebRTC 是否启用 webrtc-direct 传输层（浏览器 js-libp2p 可直连）。
	// 启用后自动在 ListenAddrs 基础上追加 /udp/<同端口+1>/webrtc-direct，
	// 或由 WebRTCAddrs 显式指定监听地址。
	WebRTC      bool
	WebRTCAddrs []string
	// HolePunching 启用 DCUtR 打洞（独立于 RelaySource；无服务器模式用）。
	HolePunching bool
	// RelayServiceDedicated 专用中继模式：无限配额 + 强制声明公网可达。
	// 普通 P2P 节点（客户端即服务端）不要开启，用 RelayService 即可。
	RelayServiceDedicated bool
	// RelayServiceAlways 无条件启动 Circuit Relay v2 hop 服务。
	// libp2p 内建 EnableRelayService 只在判定「公网可达」后才启动，
	// NAT 后节点永远等不到该事件；「节点即服务端」语义下需要
	// 打洞成功后立即具备中继能力，故用底层 relayv2.New 直接注册。
	RelayServiceAlways bool
}

// NewHost 创建 libp2p Host。
//
// 注意 AutoRelay 的行为边界：EnableAutoRelayWithPeerSource 只在节点
// 「认为自身不可达（private/NAT 后）」时才会向 relay 预约。公网可达的
// 节点不会预约，导致其他节点经中继找不到它（NO_RESERVATION）。
// 因此 agent 主流程在入网后还须调用 EnsureRelayReservation 主动预约，
// 保证「任意成员都能被经中继访问」这一兜底语义。
func NewHost(ctx context.Context, spec HostSpec) (host.Host, error) {
	options := []libp2p.Option{
		libp2p.UserAgent(spec.UserAgent),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
	}

	if len(spec.ListenAddrs) > 0 {
		options = append(options, libp2p.ListenAddrStrings(spec.ListenAddrs...))
	}
	if spec.WebRTC {
		addrs := spec.WebRTCAddrs
		if len(addrs) == 0 {
			addrs = defaultWebRTCAddrs(spec.ListenAddrs)
		}
		if len(addrs) > 0 {
			options = append(options, libp2p.ListenAddrStrings(addrs...))
		}
	}
	if spec.RelayService {
		if spec.RelayServiceDedicated {
			options = append(options,
				libp2p.ForceReachabilityPublic(),
				libp2p.EnableRelayService(relayv2.WithInfiniteLimits()),
			)
		} else {
			// 节点即服务端：默认配额，可达性交给 AutoNAT 真实探测。
			options = append(options, libp2p.EnableRelayService())
		}
	}
	if spec.RelaySource != nil {
		options = append(options,
			libp2p.ForceReachabilityPrivate(),
			libp2p.EnableHolePunching(),
			libp2p.EnableAutoRelayWithPeerSource(spec.RelaySource),
		)
	} else if spec.HolePunching {
		options = append(options, libp2p.EnableHolePunching())
	}

	h, err := libp2p.New(options...)
	if err != nil {
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}
	if spec.RelayServiceAlways {
		// 底层 hop 服务：默认资源配额，不受 reachability 事件约束。
		if _, err = relayv2.New(h); err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("start relay service: %w", err)
		}
	}
	return h, nil
}

// defaultWebRTCAddrs 依据 TCP/QUIC 监听地址推导 webrtc-direct 监听地址：
// 端口 = 原 UDP 端口 + 101（避开 quic 端口），同网段监听。
// 仅识别 /ip4 与 /ip6 的 udp quic 地址；无匹配时返回空（不额外监听）。
func defaultWebRTCAddrs(listenAddrs []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range listenAddrs {
		addr, err := ma.NewMultiaddr(raw)
		if err != nil {
			continue
		}
		ipComponent, ipErr := addr.ValueForProtocol(ma.P_IP4)
		if ipErr != nil {
			ipComponent, ipErr = addr.ValueForProtocol(ma.P_IP6)
		}
		if ipErr != nil {
			continue
		}
		udpComponent, udpErr := addr.ValueForProtocol(ma.P_UDP)
		if udpErr != nil {
			continue
		}
		port := 101
		if udpComponent != "0" {
			if parsed, parseErr := strconv.Atoi(udpComponent); parseErr == nil {
				port = parsed + 101
			}
		}
		target := fmt.Sprintf("/ip%s/%s/udp/%d/webrtc-direct",
			map[bool]string{true: "4", false: "6"}[strings.Contains(ipComponent, ".")],
			ipComponent, port)
		if !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
	}
	return out
}

func AddrInfo(h host.Host) peer.AddrInfo {
	return peer.AddrInfo{
		ID:    h.ID(),
		Addrs: h.Addrs(),
	}
}

// EnsureRelayReservation 向候选中继逐个发起预约，成功一次即返回。
// 用途：agent 入网后主动在 relay 上留下预约（Reservation），
// 使任意其他成员都能经中继访问本节点——AutoRelay 只在节点
// 自认不可达时才预约，公网可达节点必须靠这里兜底。
// source 为 nil 或全部候选预约失败时返回错误（不阻塞主流程，可周期重试）。
func EnsureRelayReservation(ctx context.Context, h host.Host, source autorelay.PeerSource, number int) error {
	if source == nil {
		return fmt.Errorf("nil relay source")
	}
	if number < 1 {
		number = 2
	}
	for candidate := range source(ctx, number) {
		reserveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := relayclient.Reserve(reserveCtx, h, candidate)
		cancel()
		if err == nil {
			return nil
		}
		// 记录但不中断：继续尝试下一个候选。
	}
	return fmt.Errorf("no relay reservation succeeded")
}
