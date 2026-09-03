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
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	p2pkit "github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/ayflying/pvn/pkg/tundevice"
	tunnelsvc "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/network"
)

func main() {
	var (
		ctlURL     = flag.String("ctl", "http://127.0.0.1:8000", "控制面地址")
		mode       = flag.String("mode", "join", "create=创建群组; join=凭邀请加入")
		inviteCode = flag.String("invite", "", "邀请码（join 模式必填）")
		name       = flag.String("name", "", "节点名称（必填）")
		osName     = flag.String("os", defaultOS(), "操作系统标识")
		groupName  = flag.String("group", "default", "群组名称（create 模式）")
		tunName    = flag.String("tun", "pvn0", "TUN 网卡名")
		mtu        = flag.Int("mtu", 1400, "TUN MTU")
		realTUN    = flag.Bool("real-tun", false, "创建真实 TUN 网卡（需管理员权限）；默认内存 TUN 仅用于联调")
	)
	flag.Parse()
	if *name == "" {
		log.Fatalf("必须指定 -name 节点名称")
	}
	if *mode == "join" && *inviteCode == "" {
		log.Fatalf("join 模式必须提供 -invite 邀请码")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. 创建 libp2p Host（含打洞与自动中继能力）。
	hostSpec := p2pkit.HostSpec{
		UserAgent: "pvn-agent/0.1.0",
	}
	if *realTUN {
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
	netmapCli := netmapclient.NewClient(*ctlURL, peerID)
	var myIP string
	if *mode == "create" {
		resp, err := postJSON(*ctlURL+"/v1/groups/create", map[string]any{
			"peer_id": peerID, "name": *name, "os": *osName, "group_name": *groupName,
		})
		if err != nil {
			log.Fatalf("创建群组: %v", err)
		}
		invite := resp["invite_code"].(string)
		myIP = resp["creator"].(map[string]any)["virtual_ip"].(string)
		log.Printf("群组已创建，邀请码=%s（请分享给要加入的成员）", invite)
	} else {
		resp, err := postJSON(*ctlURL+"/v1/groups/join", map[string]any{
			"invite_code": *inviteCode, "peer_id": peerID, "name": *name, "os": *osName,
		})
		if err != nil {
			log.Fatalf("加入群组: %v", err)
		}
		myIP = resp["member"].(map[string]any)["virtual_ip"].(string)
		log.Printf("已加入群组，虚拟 IP=%s", myIP)
	}

	// 3. 创建 TUN 网卡并配置虚拟 IP。
	var device tundevice.Device
	if *realTUN {
		device, err = tundevice.NewNative(*tunName, *mtu)
		if err != nil {
			log.Fatalf("创建 TUN 网卡: %v", err)
		}
		if err = configureTUN(*tunName, myIP, 24); err != nil {
			_ = device.Close()
			log.Fatalf("配置 TUN 网卡地址: %v", err)
		}
		log.Printf("TUN 网卡 %s 已创建，虚拟 IP=%s", *tunName, myIP)
	} else {
		device, _, err = tundevice.NewMemory(*mtu)
		if err != nil {
			log.Fatalf("创建内存 TUN: %v", err)
		}
		log.Printf("（联调模式）使用内存 TUN，虚拟 IP=%s", myIP)
	}
	defer device.Close()

	// 4. 被叫方：接收组内隧道流，把包写进本机 TUN（交给本机协议栈）。
	node.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		buf := make([]byte, 65535)
		for {
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			if _, err := device.Write([][]byte{buf[:n]}, 0); err != nil {
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

	tunnelSvc := tunnelsvc.New(node, netmapCli, newRelayCandidates(*ctlURL))
	router := tundevice.New(device, tunnelSvc)
	go router.Run(ctx)

	// 6. 周期任务：刷新 NetMap + 重新通告地址（地址可能变化）。
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
			}
		}
	}()

	log.Printf("PVN Agent 已就绪：虚拟 IP=%s，组内成员可通过该 IP 访问本机", myIP)
	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭")
}
