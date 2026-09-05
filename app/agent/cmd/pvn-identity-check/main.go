// pvn-identity-check 节点身份持久化 + 虚拟域名端到端验证：
//   - 节点 A 配置 IdentityFile 后重启两次，虚拟 IP 恒定；
//   - 节点 B 按名称（虚拟域名）"node-a" 直接 Dial 成功；
//   - 按不存在名称 Dial 报错。
//
// 用法：go run ./app/agent/cmd/pvn-identity-check
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	networkKey := "net-identity-e2e"
	keyFile := "pvn-identity-check-node-a.key"
	_ = os.Remove(keyFile)
	defer func() { _ = os.Remove(keyFile) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. 节点 A 第一次启动：记录虚拟 IP。
	nodeA, err := lanet.New(ctx, lanet.Config{
		Name: "node-a", Standalone: true, NetworkKey: networkKey, IdentityFile: keyFile,
	})
	if err != nil {
		fail("创建节点 A（第一次）: %v", err)
	}
	ipFirst := nodeA.Info().VirtualIP
	_ = nodeA.Close()

	// 2. 节点 A 第二次启动：虚拟 IP 必须不变。
	nodeA, err = lanet.New(ctx, lanet.Config{
		Name: "node-a", Standalone: true, NetworkKey: networkKey, IdentityFile: keyFile,
	})
	if err != nil {
		fail("创建节点 A（第二次）: %v", err)
	}
	defer nodeA.Close()
	go nodeA.Run(ctx)
	// A 提供 echo（供 B 按名称拨号验证）。
	nodeA.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})
	ipSecond := nodeA.Info().VirtualIP
	if ipFirst != ipSecond {
		fail("虚拟 IP 跨重启变化: %s → %s（身份未持久化）", ipFirst, ipSecond)
	}
	fmt.Printf("PASS 身份持久化：两次启动虚拟 IP 恒定 %s\n", ipSecond)

	// 3. 节点 B 按名称拨号。
	nodeB, err := lanet.New(ctx, lanet.Config{
		Name: "node-b", Standalone: true, NetworkKey: networkKey,
		Bootstrap: multiaddrStrings(nodeA),
	})
	if err != nil {
		fail("创建节点 B: %v", err)
	}
	defer nodeB.Close()
	go nodeB.Run(ctx)
	waitDiscovery(nodeB, ipSecond)

	stream, _, err := nodeB.Dial(ctx, "node-a") // 虚拟域名：不用知道 IP
	if err != nil {
		fail("按名称拨号 node-a: %v", err)
	}
	payload := "hello-by-name"
	if _, err = stream.Write([]byte(payload)); err != nil {
		fail("写: %v", err)
	}
	if hc, ok := stream.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}
	reply, err := io.ReadAll(stream)
	if err != nil {
		fail("读: %v", err)
	}
	_ = stream.Close()
	if string(reply) != payload {
		fail("回程不一致: %q != %q", reply, payload)
	}
	fmt.Println("PASS 虚拟域名：B 按 \"node-a\" 拨号 echo 往返成功（未使用 IP）")

	// 4. 不存在的名称报错。
	if _, _, err = nodeB.Dial(ctx, "no-such-node"); err == nil {
		fail("不存在的名称不应拨号成功")
	}
	fmt.Println("PASS 未知名称正确报错")

	fmt.Println("\nPASS 身份持久化 + 虚拟域名端到端验证")
}

func waitDiscovery(from *lanet.Client, targetIP string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range from.NetMap().Members {
			if m.VirtualIP == targetIP {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	fail("发现超时")
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

func fail(format string, args ...any) {
	fmt.Printf("FAIL "+format+"\n", args...)
	os.Exit(1)
}
