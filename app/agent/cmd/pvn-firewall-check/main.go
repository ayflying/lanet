// pvn-firewall-check 防火墙 + 局域网端口转发端到端验证：
// 节点 A（开控制台）模拟「本机服务 9911 + 局域网设备 127.0.0.2:9912」，
// 节点 B 经虚拟网 DialPortFWD 访问，验证防火墙判定链与映射转发。
//
// 用法：go run ./app/agent/cmd/pvn-firewall-check
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/firewall"
	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	networkKey := "net-firewall-e2e"
	stateFile := "pvn-firewall-check-state.json"
	_ = os.Remove(stateFile)
	defer func() { _ = os.Remove(stateFile) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 0. 模拟服务：echo 服务读全输入后回写 "echo:"+输入。
	echoSrv1 := startEcho(":9911")
	defer echoSrv1.Close()
	echoSrv2 := startEcho("127.0.0.2:9912") // loopback 别名模拟「局域网另一台设备」
	defer echoSrv2.Close()

	fmt.Println("== [1/6] 启动节点 A（开控制台 127.0.0.1:8920） ==")
	nodeA, err := lanet.New(ctx, lanet.Config{
		Name:        "node-a",
		Standalone:  true,
		NetworkKey:  networkKey,
		ConsoleAddr: "127.0.0.1:8920",
		StateFile:   stateFile,
	})
	if err != nil {
		fail("创建节点 A: %v", err)
	}
	defer nodeA.Close()
	go nodeA.Run(ctx)

	fmt.Println("== [2/6] 启动节点 B 并等待发现 A ==")
	nodeB, err := lanet.New(ctx, lanet.Config{
		Name:       "node-b",
		Standalone: true,
		NetworkKey: networkKey,
		Bootstrap:  multiaddrStrings(nodeA),
	})
	if err != nil {
		fail("创建节点 B: %v", err)
	}
	defer nodeB.Close()
	go nodeB.Run(ctx)
	ipA := waitDiscovery(nodeB, nodeA)
	ipB := nodeB.Info().VirtualIP
	fmt.Printf("B 已发现 A：虚拟 IP=%s（B=%s）\n", ipA, ipB)

	// 1. 默认 deny-all：9911 应被拒绝。
	fmt.Println("== [3/6] 防火墙默认 deny-all：B 拨 A:9911 应被拒绝 ==")
	if err = probeRejected(nodeB, ipA, 9911); err != nil {
		fail("%v", err)
	}
	fmt.Println("PASS 默认拒绝生效")

	// 2. 放行 B 的 9911（本机服务）与 9912（LAN 设备映射）。
	fmt.Println("== [4/6] 放行 B（按来源虚拟 IP）：9911 本机服务 + 9912 局域网映射 ==")
	nodeA.SetFirewall(firewall.ModeAllowList, []firewall.Rule{
		{Source: ipB, Port: "9911"},
		{Source: ipB, Port: "9912"},
	})
	nodeA.SetLANForwards([]lanet.LANForward{{Listen: 9912, Target: "127.0.0.2:9912"}})
	if err = probeEcho(nodeB, ipA, 9911, "echo:ping-9911"); err != nil {
		fail("放行后 9911 本机服务应连通: %v", err)
	}
	fmt.Println("PASS B→A:9911（本机服务）")
	if err = probeEcho(nodeB, ipA, 9912, "echo:ping-9912"); err != nil {
		fail("放行后 9912 局域网映射应连通: %v", err)
	}
	fmt.Println("PASS B→A:9912 → 127.0.0.2:9912（局域网设备）")
	if err = probeRejected(nodeB, ipA, 9999); err != nil {
		fail("%v", err)
	}
	fmt.Println("PASS 未放行端口 9999 仍被拒绝")

	// 3. 控制台 API：读状态 + 热更新回 deny-all 后应再次拒绝。
	fmt.Println("== [5/6] Web 控制台：状态接口 + 热更新回 deny-all ==")
	resp, err := http.Get("http://127.0.0.1:8920/api/state")
	if err != nil {
		fail("控制台不可达: %v", err)
	}
	var st struct {
		Mode     string             `json:"mode"`
		Forwards []lanet.LANForward `json:"forwards"`
		Members  []map[string]any   `json:"members"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&st); err != nil {
		fail("状态解析失败: %v", err)
	}
	resp.Body.Close()
	if st.Mode != string(firewall.ModeAllowList) || len(st.Forwards) != 1 || len(st.Members) == 0 {
		fail("状态异常: mode=%s forwards=%d members=%d", st.Mode, len(st.Forwards), len(st.Members))
	}
	fmt.Println("PASS GET /api/state（mode/forwards/members 正确）")

	body, _ := json.Marshal(map[string]any{"mode": "deny-all", "rules": []firewall.Rule{}})
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8920/api/firewall", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	hr, err := http.DefaultClient.Do(req)
	if err != nil || hr.StatusCode != http.StatusOK {
		fail("PUT /api/firewall 失败: err=%v status=%d", err, hrStatus(hr))
	}
	hr.Body.Close()
	if err = probeRejected(nodeB, ipA, 9911); err != nil {
		fail("%v", err)
	}
	fmt.Println("PASS 控制台热更新 deny-all 即时生效")

	if _, err = os.Stat(stateFile); err != nil {
		fail("状态文件未落盘")
	}
	fmt.Println("PASS 状态持久化文件已写入")

	// 4. 统一防火墙的应用流维度：OnStream（/pvn/tunnel/1.0.0）默认拒绝，
	// 放行协议规则后 echo 往返连通。
	fmt.Println("== [6/6] 应用流防火墙：OnStream 默认拒绝 → 放行协议后连通 ==")
	nodeB.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})
	if err = probeStreamEcho(nodeA, ipB, "stream-ping"); err == nil {
		fail("B 的应用流不应连通（默认 deny-all 未拦截 OnStream）")
	}
	fmt.Println("PASS OnStream 默认拒绝生效")
	nodeB.SetFirewall(firewall.ModeAllowList, []firewall.Rule{
		{Source: "*", Proto: "/pvn/tunnel/1.0.0"},
	})
	if err = probeStreamEcho(nodeA, ipB, "stream-ping-2"); err != nil {
		fail("放行协议规则后应用流应连通: %v", err)
	}
	fmt.Println("PASS 放行 /pvn/tunnel/1.0.0 后 OnStream echo 往返")

	fmt.Println("\nPASS 防火墙 + 局域网端口转发端到端验证（拒绝/放行/映射/控制台热更新/应用流全通）")
}

// probeStreamEcho 应用流 echo：dial → 写 payload → 断言回程。
func probeStreamEcho(from *lanet.Client, ip string, payload string) error {
	stream, _, err := from.Dial(context.Background(), ip)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer stream.Close()
	if _, err = stream.Write([]byte(payload)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if hc, ok := stream.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}
	reply, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if string(reply) != payload {
		return fmt.Errorf("expect %q got %q", payload, reply)
	}
	return nil
}

// probeEcho 拨号 → 写 payload → 断言回程。
func probeEcho(from *lanet.Client, ip string, port int, expect string) error {
	conn, err := from.DialPortFWD(context.Background(), lanet.PortFWDTarget{VirtualIP: ip, Port: port})
	if err != nil {
		return fmt.Errorf("dial %d: %w", port, err)
	}
	defer conn.Close()
	payload := fmt.Sprintf("ping-%d", port)
	if _, err = conn.Write([]byte(payload)); err != nil {
		return fmt.Errorf("write %d: %w", port, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	if hc, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read %d: %w", port, err)
	}
	if string(reply) != expect {
		return fmt.Errorf("expect %q got %q", expect, reply)
	}
	return nil
}

// probeRejected 期望被防火墙拒绝（连接建立但读不到回程）。
func probeRejected(from *lanet.Client, ip string, port int) error {
	err := probeEcho(from, ip, port, fmt.Sprintf("echo:ping-%d", port))
	if err == nil {
		return fmt.Errorf("端口 %d 不应连通（防火墙未拦截）", port)
	}
	return nil
}

// startEcho TCP echo：读全输入（EOF）后回写 "echo:"+输入。
func startEcho(addr string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fail("启动模拟服务 %s: %v", addr, err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				data, err := io.ReadAll(c)
				if err != nil {
					return
				}
				_, _ = c.Write(append([]byte("echo:"), data...))
			}(conn)
		}
	}()
	return ln
}

// waitDiscovery 等待双向互见：from 与 target 的成员表都包含对方
// （单边发现不够——对端反查来源虚拟 IP 依赖它自己的成员表同步）。
func waitDiscovery(from, target *lanet.Client) string {
	ipFrom, ipTarget := from.Info().VirtualIP, target.Info().VirtualIP
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		fSeen, tSeen := false, false
		for _, m := range from.NetMap().Members {
			if m.VirtualIP == ipTarget {
				fSeen = true
			}
		}
		for _, m := range target.NetMap().Members {
			if m.VirtualIP == ipFrom {
				tSeen = true
			}
		}
		if fSeen && tSeen {
			return ipTarget
		}
		time.Sleep(500 * time.Millisecond)
	}
	fail("发现超时")
	return ""
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

func hrStatus(hr *http.Response) int {
	if hr == nil {
		return 0
	}
	return hr.StatusCode
}

func fail(format string, args ...any) {
	fmt.Printf("FAIL "+format+"\n", args...)
	os.Exit(1)
}
