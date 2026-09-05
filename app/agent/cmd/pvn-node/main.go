// pvn-node 长驻的 Standalone（无服务器）节点，用于部署与跨网络联调：
//
//   - 以 lanet SDK standalone 模式入网（DHT + mDNS 自动发现，节点即服务端）；
//   - 对外提供 /lanet/echo/1.0.0 回显应用流；
//   - 周期探测成员表内所有成员：按虚拟 IP 开 echo 流往返一次并记录结果
//     （direct/relay、耗时），便于在容器日志里直接确认跨网连通性；
//   - 内置 Web 控制台（防火墙 / 转发映射 / 成员状态）。
//
// 用法示例（详见 build/node.Dockerfile 与 docker compose）：
//
//	pvn-node -name edge-a -key net-xnet-test -bootstrap public -console :8900
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ayflying/pvn/pkg/netmapclient"
	"github.com/ayflying/pvn/pkg/serverless"
	"github.com/ayflying/pvn/sdk/go/lanet"
	"github.com/libp2p/go-libp2p/core/network"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// echoProto 节点间探测回显协议。
const echoProto = libprotocol.ID("/lanet/echo/1.0.0")

func main() {
	var (
		name      = flag.String("name", envOr("LANET_NAME", "node"), "节点名称（成员表中的虚拟域名）")
		key       = flag.String("key", envOr("LANET_NETWORK_KEY", ""), "网络密钥（留空 = 公共网络）")
		bootstrap = flag.String("bootstrap", envOr("LANET_BOOTSTRAP", "public"),
			"逗号分隔的引导节点 multiaddr（私有网络下作为私有 DHT 种子）；public = 公共 DHT，none = 仅 mDNS")
		identity = flag.String("identity", envOr("LANET_IDENTITY", "/data/node.key"), "身份密钥文件路径")
		console  = flag.String("console", envOr("LANET_CONSOLE", "0.0.0.0:8900"), "控制台监听地址，- 关闭")
		fw       = flag.String("fw", envOr("LANET_FW", "allow-all"), "防火墙模式：deny-all / allow-list / allow-all")
		probe    = flag.Duration("probe", 20*time.Second, "成员探测间隔")
		listen   = flag.String("listen", envOr("LANET_LISTEN", ""),
			"覆盖监听地址（逗号分隔）；默认 tcp/ws/quic 全部随机端口")
		noPublic = flag.Bool("no-public-dht",
			envOr("LANET_NO_PUBLIC_DHT", "") == "1" || strings.EqualFold(envOr("LANET_NO_PUBLIC_DHT", ""), "true"),
			"私有网络下关闭公共 DHT 兜底（纯私有种子 + mDNS）")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[node] 启动 name=%s key=%q fw=%s console=%s noPublicDHT=%v", *name, *key, *fw, *console, *noPublic)

	// 引导节点列表。
	var bootstraps []string
	noPublicDHT := *noPublic
	switch strings.TrimSpace(*bootstrap) {
	case "", "none":
		// 仅 mDNS 局域网发现（完全不接入公共 DHT）。
		noPublicDHT = true
	case "public":
		bootstraps = []string{serverless.DefaultBootstrap}
	default:
		for _, a := range strings.Split(*bootstrap, ",") {
			if a = strings.TrimSpace(a); a != "" {
				bootstraps = append(bootstraps, a)
			}
		}
	}
	cfg := lanet.Config{
		Name:             *name,
		NetworkKey:       *key,
		Standalone:       true,
		Bootstrap:        bootstraps,
		DisablePublicDHT: noPublicDHT,
		IdentityFile:     *identity,
		ConsoleAddr:      *console,
	}
	switch *fw {
	case "allow-list":
		cfg.FirewallMode = lanet.FirewallModeAllowList
		// 探测与 echo 依赖应用流入向：放行全部来源的全部协议（测试语义）。
		cfg.FirewallRules = []lanet.FirewallRule{{Source: "*", Proto: lanet.FirewallProtoAny}}
	case "deny-all":
		cfg.FirewallMode = lanet.FirewallModeDenyAll
	default:
		cfg.FirewallMode = lanet.FirewallModeAllowAll
	}
	if *listen != "" {
		cfg.ListenAddrs = strings.Split(*listen, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := lanet.New(ctx, cfg)
	if err != nil {
		log.Fatalf("[node] 入网失败: %v", err)
	}
	defer node.Close()
	info := node.Info()
	log.Printf("[node] 已入网 name=%s peerID=%s virtualIP=%s network=%s",
		*name, info.PeerID, info.VirtualIP, networkLabel(*key))

	// 回显服务：收到什么回什么（供其他节点探测）。
	node.Host().SetStreamHandler(echoProto, func(s network.Stream) {
		defer s.Close()
		buf := make([]byte, 4096)
		for {
			n, err := s.Read(buf)
			if n > 0 {
				if _, werr := s.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	// 同时保留 SDK Tunnel 协议 echo（兼容既有验证工具）。
	node.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = stream.Write([]byte("echo:" + stream.Protocol()))
	})

	go node.Run(ctx)

	// 周期探测：向成员表内所有其他成员发起 echo 往返。
	lastMembers := ""
	ticker := time.NewTicker(*probe)
	defer ticker.Stop()
	first := time.After(5 * time.Second)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[node] 收到退出信号")
			return
		case <-first:
		case <-ticker.C:
		}
		members := node.NetMap().Members
		sig := membersSignature(members)
		if sig != lastMembers {
			log.Printf("[node] 成员表更新（%d 人）: %s", len(members), sig)
			lastMembers = sig
		}
		self := node.Info().VirtualIP
		for _, m := range members {
			if m.VirtualIP == self || m.VirtualIP == "" {
				continue
			}
			probeOnce(ctx, node, m.Name, m.VirtualIP)
		}
	}
}

// probeOnce 对单个成员做一次 echo 往返探测。
func probeOnce(ctx context.Context, node *lanet.Client, name, virtualIP string) {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, viaRelay, err := node.DialProtocol(pctx, virtualIP, string(echoProto))
	if err != nil {
		log.Printf("[probe] FAIL %s(%s): %v", name, virtualIP, err)
		return
	}
	defer stream.Close()
	payload := fmt.Sprintf("probe-%d", start.UnixMilli())
	if _, err = stream.Write([]byte(payload)); err != nil {
		log.Printf("[probe] FAIL %s(%s): write: %v", name, virtualIP, err)
		return
	}
	_ = stream.CloseWrite()
	buf := make([]byte, 4096)
	n, err := readAll(stream, buf)
	via := "direct"
	if viaRelay {
		via = "relay"
	}
	if err != nil || string(buf[:n]) != payload {
		log.Printf("[probe] FAIL %s(%s): 回显不匹配 (n=%d err=%v)", name, virtualIP, n, err)
		return
	}
	log.Printf("[probe] OK %s(%s) via=%s rtt=%s", name, virtualIP, via, time.Since(start).Round(time.Millisecond))
}

// readAll 读完直到 EOF 或缓冲满。
func readAll(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, nil // EOF 语义视为正常结束
		}
	}
	return total, nil
}

// membersSignature 成员表摘要（稳定顺序）。
func membersSignature(members []netmapclient.Member) string {
	type row struct{ ip, name string }
	rows := make([]row, 0, len(members))
	for _, m := range members {
		rows = append(rows, row{m.VirtualIP, m.Name})
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].ip < rows[i].ip {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = r.name + "@" + r.ip
	}
	return strings.Join(parts, ", ")
}

func networkLabel(key string) string {
	if key == "" {
		return "public(公共网络)"
	}
	return "private(" + key + ")"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
