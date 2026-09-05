package main

// pvn-agent：PVN 客户端入口。
// 流程：向控制面入网（建群或凭邀请加入）→ 拉取群组 NetMap → 创建 TUN 虚拟网卡
// → 启动路由器（TUN ↔ libp2p 隧道双向转发）→ 周期刷新 NetMap 与地址通告。
//
// 用法：
//
//	创建群组：pvn-agent -ctl http://ctl:8000 -mode create  -name my-pc  -os windows -group "我的局域网"
//	加入群组：pvn-agent -ctl http://ctl:8000 -mode join -invite XXXXXXXXXX -name pc2 -os windows
//
// Windows 需管理员权限运行（Wintun 网卡）；Linux 需要 /dev/net/tun 与 CAP_NET_ADMIN。

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gogf/gf/v2/os/gcmd"
	"github.com/gogf/gf/v2/os/gctx"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/pkg/netmapclient"
	p2pkit "github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ayflying/pvn/pkg/peersource"
	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tundevice"
	tunnelsvc "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
)

// version 由 CI 经 -ldflags "-X main.version=<VERSION 文件内容>" 注入。
var version = "dev"

func main() {
	command := gcmd.Command{
		Name:  "pvn-agent",
		Usage: "pvn-agent",
		Brief: "Lanet 客户端：入网（建群/凭邀请加入）→ TUN 虚拟网卡 → P2P 隧道转发",
		Arguments: []gcmd.Argument{
			{Name: "ctl", Short: "c", Default: "http://127.0.0.1:8000", Brief: "控制面地址"},
			{Name: "mode", Short: "m", Default: "join", Brief: "create=创建群组; join=凭邀请加入"},
			{Name: "invite", Short: "i", Default: "", Brief: "邀请码（join 模式必填）"},
			{Name: "name", Short: "n", Default: "", Brief: "节点名称（必填）"},
			{Name: "os", Default: defaultOS(), Brief: "操作系统标识"},
			{Name: "group", Short: "g", Default: "default", Brief: "群组名称（create 模式）"},
			{Name: "tun", Short: "t", Default: "pvn0", Brief: "TUN 网卡名"},
			{Name: "mtu", Default: "1400", Brief: "TUN MTU"},
			{Name: "real-tun", Default: "false", Brief: "创建真实 TUN 网卡（需管理员权限）；默认内存 TUN 仅用于联调"},
			{Name: "fw", Default: "deny-all", Brief: "入向防火墙模式：deny-all（默认，拒绝一切入向）/ allow-all（全开）"},
		},
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			runAgent(ctx, parser)
			return nil
		},
	}
	command.Run(gctx.GetInitCtx())
}

// runAgent 客户端主流程（参数由 gcmd Parser 提供）。
func runAgent(ctx context.Context, parser *gcmd.Parser) {
	var (
		ctlURL     = parser.GetOpt("ctl").String()
		mode       = parser.GetOpt("mode").String()
		inviteCode = parser.GetOpt("invite").String()
		name       = parser.GetOpt("name").String()
		osName     = parser.GetOpt("os").String()
		groupName  = parser.GetOpt("group").String()
		tunName    = parser.GetOpt("tun").String()
		mtu        = parser.GetOpt("mtu").Int()
		realTUN    = parser.GetOpt("real-tun").Bool()
		fwMode     = parser.GetOpt("fw").String()
	)
	if name == "" {
		log.Fatalf("必须指定 -name 节点名称")
	}
	log.Printf("[agent] pvn-agent version=%s", version)
	if mode == "join" && inviteCode == "" {
		log.Fatalf("join 模式必须提供 -invite 邀请码")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. 创建 libp2p Host（含打洞与自动中继能力）。
	// RelaySource 提供 relay 候选：AutoRelay 会据此尽早连接 relay 并完成预约，
	// 使本节点可被其他成员经中继访问（否则对端走中继时会被 NO_RESERVATION 拒绝）。
	// relay 候选地址来自控制面 /v1/relays/candidates，先建轻量 client 再建 host。
	peerSource := peersource.NewClient(ctlURL).AutoRelayPeerSource()
	hostSpec := p2pkit.HostSpec{
		UserAgent:   "pvn-agent/" + version,
		RelaySource: peerSource,
	}
	if realTUN {
		hostSpec.ListenAddrs = []string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"}
	} else {
		hostSpec.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"}
	}
	node, err := p2pkit.NewHost(ctx, hostSpec)
	if err != nil {
		log.Fatalf("创建 libp2p Host: %v", err)
	}
	defer node.Close()
	peerID := node.ID().String()
	log.Printf("节点 PeerID=%s", peerID)

	// 2. 入网：建群或加入。
	netmapCli := netmapclient.NewClient(ctlURL, peerID)
	var myIP string
	if mode == "create" {
		resp, err := postJSON(ctlURL+"/v1/groups/create", map[string]any{
			"peer_id": peerID, "name": name, "os": osName, "group_name": groupName,
		})
		if err != nil {
			log.Fatalf("创建群组: %v", err)
		}
		invite := resp["invite_code"].(string)
		myIP = resp["creator"].(map[string]any)["virtual_ip"].(string)
		log.Printf("群组已创建，邀请码=%s（请分享给要加入的成员）", invite)
	} else {
		resp, err := postJSON(ctlURL+"/v1/groups/join", map[string]any{
			"invite_code": inviteCode, "peer_id": peerID, "name": name, "os": osName,
		})
		if err != nil {
			log.Fatalf("加入群组: %v", err)
		}
		myIP = resp["member"].(map[string]any)["virtual_ip"].(string)
		log.Printf("已加入群组，虚拟 IP=%s", myIP)
	}

	// 3. 创建 TUN 网卡并配置虚拟 IP。
	var device tundevice.Device
	if realTUN {
		device, err = tundevice.NewNative(tunName, mtu)
		if err != nil {
			log.Fatalf("创建 TUN 网卡: %v", err)
		}
		if err = configureTUN(tunName, myIP, 24); err != nil {
			_ = device.Close()
			log.Fatalf("配置 TUN 网卡地址: %v", err)
		}
		log.Printf("TUN 网卡 %s 已创建，虚拟 IP=%s", tunName, myIP)
	} else {
		device, _, err = tundevice.NewMemory(mtu)
		if err != nil {
			log.Fatalf("创建内存 TUN: %v", err)
		}
		log.Printf("（联调模式）使用内存 TUN，虚拟 IP=%s", myIP)
	}
	defer device.Close()

	// 4.1 统一入向防火墙：管控 TUN 入向（IP 层）暴露面。
	// 规则模式：deny-all（默认，拒绝一切入向包）/ allow-all。
	fw := firewall.New()
	switch fwMode {
	case "allow-all":
		fw.Set(firewall.ModeAllowAll, nil)
		log.Printf("入向防火墙：allow-all（全开）")
	default:
		log.Printf("入向防火墙：deny-all（默认拒绝；组内成员暂时无法访问本机，如需开放用 -fw allow-all）")
	}

	// 4. 被叫方：接收组内隧道流，逐包过防火墙后写进本机 TUN（交给本机协议栈）。
	node.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		buf := make([]byte, 65535)
		for {
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			packet := buf[:n]
			// 统一入向防火墙：源虚拟 IP + 协议 + 目标端口，拒绝即丢包。
			if !tundevice.CheckPacket(fw, packet) {
				if n >= 20 {
					log.Printf("[firewall] TUN 入向包被拒绝：来源=%d.%d.%d.%d 协议=%d",
						packet[12], packet[13], packet[14], packet[15], packet[9])
				}
				continue
			}
			if _, err := device.Write([][]byte{packet}, 0); err != nil {
				return
			}
		}
	})

	// 5. 首次拉取 NetMap 并通告可达地址；隧道与路由器启动。
	if _, err = netmapCli.Refresh(ctx); err != nil {
		log.Fatalf("拉取 NetMap: %v", err)
	}
	addrs := make([]string, 0, len(node.Addrs()))
	for _, addr := range node.Addrs() {
		addrs = append(addrs, addr.String())
	}
	if err = netmapCli.Announce(ctx, addrs); err != nil {
		log.Printf("通告地址失败（不影响运行，将周期重试）: %v", err)
	}

	// 5.1 主动向 relay 预约（兜底链路的关键一步）：
	// AutoRelay 只在节点自认不可达时才预约，公网可达节点不会预约，
	// 会导致其他成员经中继访问本节点时报 NO_RESERVATION。
	// 因此入网后立即主动预约一次，失败不阻塞（周期任务会重试）。
	if err := p2pkit.EnsureRelayReservation(ctx, node, peerSource, 2); err != nil {
		log.Printf("relay 预约暂未成功（将周期重试）: %v", err)
	} else {
		log.Printf("relay 预约完成，可被组内成员经中继访问")
	}

	tunnelSvc := tunnelsvc.New(node, netmapCli, newRelayCandidates(ctlURL))
	router := tundevice.New(device, tunnelSvc)
	router.SetFirewall(fw)
	go router.Run(ctx)

	// 6. 周期任务：刷新 NetMap + 重新通告地址 + 补充 relay 预约（预约会过期）。
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := netmapCli.Refresh(ctx); err != nil {
					log.Printf("刷新 NetMap: %v", err)
					continue
				}
				addrs := make([]string, 0, len(node.Addrs()))
				for _, addr := range node.Addrs() {
					addrs = append(addrs, addr.String())
				}
				_ = netmapCli.Announce(ctx, addrs)
				// 预约有有效期，周期性补充（已有时 Reserve 也会刷新有效期）。
				if err := p2pkit.EnsureRelayReservation(ctx, node, peerSource, 2); err != nil {
					log.Printf("relay 预约补充失败（下个周期重试）: %v", err)
				}
			}
		}
	}()

	log.Printf("PVN Agent 已就绪：虚拟 IP=%s，入向防火墙=%s", myIP, fwMode)
	if fwMode != "allow-all" {
		log.Printf("提示：当前 deny-all，组内成员无法访问本机；如需开放用 -fw allow-all")
	}
	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭")
}
