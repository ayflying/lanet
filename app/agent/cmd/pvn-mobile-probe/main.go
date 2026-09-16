// pvn-mobile-probe 移动端配置探针：用「手机端将要采用的 SDK 配置」在桌面上
// 复现一次入网，先把移动端方案的地基验证掉，再写 Android 代码。
//
// 只回答一个问题：sdk/go/lanet 以 Standalone + 指定网络密钥 + 指定引导种子
// 启动，能否真的发现现有官方节点网络里的成员？
//
// 为什么必须先验证这一点：官方 pvn-node 构造的也是 lanet.Config
// （app/agent/cmd/pvn-node/main.go:303），而 sdk/go/lanet/client.go 会把
// 空 Channel 统一归一化为 serverless.ChannelSDK —— 即官方节点实际也运行在
// sdk 渠道。若该推断成立，移动端无需任何「渠道后门」即可与桌面节点同网；
// 若不成立，手机起来后就是一个成员都看不到的空网络，后续 Android 工作全白做。
//
// 本程序不干扰任何现有节点：关闭内置控制台、不建 TUN、独立身份与地址簿文件、
// 监听端口由 SDK 随机分配。
//
// 用法（密钥与种子取自目标节点的 lanet.json）：
//
//	go run ./app/agent/cmd/pvn-mobile-probe \
//	  -key yunloli \
//	  -bootstrap "/ip4/43.136.124.167/tcp/4001/p2p/12D3KooW…" \
//	  -seconds 90
//
// 判定标准：成员表出现现有节点（Desktop/VPS）即地基成立；若成员表恒为 0
// 但本机节点控制台的 /api/pending 出现了探针的 PeerID，说明链路通、只差审批。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	key := flag.String("key", "", "网络密钥（必须与目标网络一致）")
	seed := flag.String("bootstrap", "", "引导种子 multiaddr，多个用逗号分隔")
	seconds := flag.Int("seconds", 90, "运行时长（秒）")
	dir := flag.String("dir", "", "数据目录（留空使用临时目录）")
	name := flag.String("name", "mobile-probe", "节点名称")
	channel := flag.String("channel", "", "分发渠道；留空 = SDK 默认（归一化为 sdk）")
	autoAccept := flag.Bool("auto-accept", false, "自动同意陌生节点（默认否，走人工审批）")
	connect := flag.String("connect", "", "启动后主动拨号的对方 multiaddr（逗号分隔可多个），模拟手机点「连接」")
	connectAfter := flag.Int("connect-after", 5, "启动后等待几秒再发起主动连接")
	flag.Parse()

	if *key == "" || *seed == "" {
		fmt.Println("FAIL 必须同时提供 -key 与 -bootstrap")
		os.Exit(2)
	}

	dataDir := *dir
	if dataDir == "" {
		temp, err := os.MkdirTemp("", "pvn-mobile-probe-")
		if err != nil {
			fmt.Printf("FAIL 创建临时目录: %v\n", err)
			os.Exit(1)
		}
		dataDir = temp
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Printf("FAIL 创建数据目录: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := lanet.Config{
		Name:            *name,
		NetworkKey:      *key,
		Standalone:      true,
		Channel:         *channel,
		Bootstrap:       splitList(*seed),
		EnablePublicDHT: false,
		// 移动端同款：身份与地址簿都必须落在应用私有目录，且与桌面节点隔离，
		// 否则会与正在运行的节点抢同一个 lanet.db / node.key。
		IdentityFile: filepath.Join(dataDir, "node.key"),
		DBPath:       filepath.Join(dataDir, "probe.db"),
		StateFile:    filepath.Join(dataDir, "state.json"),
		// 移动端同款：不启控制台（手机上没有浏览器，且 8900 会与桌面节点撞端口）。
		ConsoleAddr: "-",
		// 探针不建 TUN：桌面节点的 8900 / TUN 网卡都归正在运行的实例所有。
		Tun:        false,
		AutoAccept: *autoAccept,
		Version:    "probe",
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
	}

	node, err := lanet.New(ctx, cfg)
	if err != nil {
		fmt.Printf("FAIL 入网失败: %v\n", err)
		os.Exit(1)
	}
	defer node.Close()

	info := node.Info()
	fmt.Println("=== 探针已入网（配置与手机端一致）===")
	fmt.Printf("PeerID     : %s\n", info.PeerID)
	fmt.Printf("虚拟 IP    : %s\n", info.VirtualIP)
	fmt.Printf("虚拟主机名 : %s\n", info.VirtualHost)
	fmt.Printf("网络组     : %s\n", info.Group)
	fmt.Printf("数据目录   : %s\n", dataDir)
	fmt.Printf("渠道       : %s（留空即 SDK 默认 sdk）\n", orDash(*channel))
	fmt.Printf("审批模式   : %s\n", approvalDesc(*autoAccept))
	fmt.Println("提示：本机节点的待审批列表可用下面的命令查询（--noproxy 必须带）")
	fmt.Println(`  curl -s --noproxy '*' http://127.0.0.1:8900/api/pending`)
	fmt.Printf("=== 开始观测 %d 秒 ===\n", *seconds)

	go node.Run(ctx)

	// 主动连接：模拟手机端用户点「连接」。手机不是被动等被发现，而是拿着
	// 对方的连接码/multiaddr 直接拨号。只有主动拨号才会让对方产生待审批申请
	// （被动 DHT 发现自 0.5.41 起不产生待审批），这是「手机入网」的必经步骤。
	if *connect != "" {
		go func() {
			select {
			case <-time.After(time.Duration(*connectAfter) * time.Second):
			case <-ctx.Done():
				return
			}
			peerID, err := node.ConnectSeed(*connect)
			if err != nil {
				fmt.Printf("\n>>> 主动连接失败: %v\n", err)
				return
			}
			fmt.Printf("\n>>> 主动拨号已建立：对端 %s\n", peerID)
			fmt.Println("    预期：对端控制台的待审批列表出现本探针；同意后双方成员表互见。")
		}()
	}

	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	round := 0
	sawMember := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		round++

		snap := node.NetMap()
		if len(snap.Members) > 0 {
			sawMember = true
		}
		fmt.Printf("\n--- 第 %d 轮 %s | 成员 %d 人 ---\n",
			round, time.Now().Format("15:04:05"), len(snap.Members))
		for _, m := range snap.Members {
			fmt.Printf("    %-16s %-14s %-22s %-18s %s\n",
				fallback(m.Name, "(无名)"), m.VirtualIP, shortPeer(m.PeerID), m.Platform, m.Version)
		}

		if pending := node.PendingList(); len(pending) > 0 {
			fmt.Printf("    待审批 %d 条：\n", len(pending))
			for _, p := range pending {
				fmt.Printf("      %-22s %-16s %s\n", shortPeer(p.PeerID), fallback(p.Name, "(无名)"), p.Reason)
			}
		}
		if trusted := node.TrustedPeers(); len(trusted) > 0 {
			fmt.Printf("    已信任 %d 个：\n", len(trusted))
			for _, p := range trusted {
				fmt.Printf("      %-22s %-16s %s\n", shortPeer(p.PeerID), fallback(p.Name, "(无名)"), p.LastIP)
			}
		}
	}

	fmt.Println("\n=== 观测结束 ===")
	if sawMember {
		fmt.Println("结论：地基成立 —— SDK 以 Standalone + 该密钥/种子能看到现有网络成员，")
		fmt.Println("      手机端可直接沿用同一套配置（无需渠道后门）。")
	} else {
		fmt.Println("结论：成员表为空。请检查本机节点控制台的 /api/pending 是否出现本探针 PeerID：")
		fmt.Println("      - 出现了 → 链路通，只差审批（审批后即可互见）。")
		fmt.Println("      - 没出现 → 密钥/种子/渠道不一致，需先查清再动 Android。")
	}
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func shortPeer(id string) string {
	if len(id) <= 22 {
		return id
	}
	return id[:14] + "…" + id[len(id)-5:]
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func approvalDesc(auto bool) string {
	if auto {
		return "auto-accept（自动信任陌生节点）"
	}
	return "人工审批（陌生节点进待审批列表）"
}
