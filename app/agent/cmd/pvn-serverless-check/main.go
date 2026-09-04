// pvn-serverless-check 无服务器模式端到端验证：
// 同机启动两个 SDK 节点（无 ctl、无 relay），验证
// DHT + mDNS 自动发现 → 按（派生）虚拟 IP 互开流 → echo 往返。
//
// 用法：go run ./app/agent/cmd/pvn-serverless-check
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	networkKey := "net-standalone-e2e-check"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("== [1/4] 启动节点 A（bootstrap=空，mDNS+DHT server） ==")
	nodeA, err := lanet.New(ctx, lanet.Config{
		Name:       "node-a",
		Standalone: true,
		NetworkKey: networkKey,
	})
	if err != nil {
		fmt.Printf("FAIL 创建节点 A: %v\n", err)
		os.Exit(1)
	}
	defer nodeA.Close()
	go nodeA.Run(ctx)
	infoA := nodeA.Info()

	fmt.Println("== [2/4] 启动节点 B（bootstrap=A 的地址） ==")
	bootstraps := multiaddrStrings(nodeA)
	nodeB, err := lanet.New(ctx, lanet.Config{
		Name:       "node-b",
		Standalone: true,
		NetworkKey: networkKey,
		Bootstrap:  bootstraps,
	})
	if err != nil {
		fmt.Printf("FAIL 创建节点 B: %v\n", err)
		os.Exit(1)
	}
	defer nodeB.Close()
	go nodeB.Run(ctx)

	// 双方都提供 echo。
	nodeB.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})

	// 回调注册：A 提供 echo。
	nodeA.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})

	fmt.Printf("A: 虚拟IP=%s\nB: 虚拟IP=%s\n", infoA.VirtualIP, nodeB.Info().VirtualIP)

	fmt.Println("== [3/4] 等待自动发现（DHT/mDNS，最长 60s） ==")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		aSees, bSees := false, false
		for _, m := range nodeA.NetMap().Members {
			if m.VirtualIP == nodeB.Info().VirtualIP {
				aSees = true
			}
		}
		for _, m := range nodeB.NetMap().Members {
			if m.VirtualIP == infoA.VirtualIP {
				bSees = true
			}
		}
		if aSees && bSees {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	found := false
	for _, m := range nodeB.NetMap().Members {
		if m.VirtualIP == infoA.VirtualIP {
			found = true
			fmt.Printf("B 发现 A: peer=%s... name=%s addrs=%d\n",
				short(m.PeerID), m.Name, len(m.Addrs))
		}
	}
	if !found {
		fmt.Println("FAIL 发现超时：A/B 未互相发现")
		os.Exit(1)
	}

	fmt.Println("== [4/4] 按虚拟 IP 互开流 echo ==")
	if err := echoRound(nodeB, infoA.VirtualIP, "hello-from-b"); err != nil {
		fmt.Printf("FAIL B→A echo: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("B→A echo 往返 OK")
	if err := echoRound(nodeA, nodeB.Info().VirtualIP, "hello-from-a"); err != nil {
		fmt.Printf("FAIL A→B echo: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("A→B echo 往返 OK")

	fmt.Println("\nPASS 无服务器组网端到端验证（发现 + 双向 echo 全通）")
}

// echoRound 一轮请求-响应：dial → write → CloseWrite → 读回程到 EOF。
func echoRound(from *lanet.Client, targetVirtualIP, payload string) error {
	stream, viaRelay, err := from.Dial(context.Background(), targetVirtualIP)
	if err != nil {
		return fmt.Errorf("dial %s: %w", targetVirtualIP, err)
	}
	defer stream.Close()
	if _, err = stream.Write([]byte(payload)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = stream.CloseWrite(); err != nil {
		return fmt.Errorf("closeWrite: %w", err)
	}
	reply, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(reply, []byte(payload)) {
		return fmt.Errorf("回程不一致: %q != %q", reply, payload)
	}
	fmt.Printf("  echo %q 经 %s 往返成功\n", payload, map[bool]string{true: "relay", false: "直连"}[viaRelay])
	return nil
}

func multiaddrStrings(h *lanet.Client) []string {
	out := make([]string, 0)
	for _, a := range h.Host().Addrs() {
		s := a.String()
		if strings.Contains(s, "127.0.0.1") || strings.Contains(s, "webrtc") {
			continue
		}
		out = append(out, s+"/p2p/"+h.Info().PeerID)
	}
	return out
}

func short(peerID string) string {
	if len(peerID) > 12 {
		return peerID[:12] + "..."
	}
	return peerID
}
