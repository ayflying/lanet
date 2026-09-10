// pvn-serverless-check 无服务器模式端到端验证（按节点 ID 连接 + 连接审批）。
//
// 验证链路：
//
//	[1] 启动节点 A（默认开启连接审批，需人工同意）
//	[2] 启动节点 B（以 A 为引导种子）
//	[3] 流量控制：空地址簿不发起主动 DHT 查找（只广播、不查询）
//	[4] B 按节点 ID 连接 A → A 未同意 → 申请落入 A 的待审批列表
//	[5] 控制台 HTTP 接口（/api/connect-peer、/api/pending、/api/peers、/api/state）
//	[6] A 同意 → 双向连通（成员表互见）→ 按虚拟 IP 双向 echo 往返
//
// 用法：go run ./app/agent/cmd/pvn-serverless-check
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	fail := func(format string, args ...any) {
		fmt.Printf("FAIL "+format+"\n", args...)
		os.Exit(1)
	}

	networkKey := "net-standalone-e2e-check"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 两个节点各自独立的地址簿（同进程共享一个 lanet.db 会串味）。
	dir, err := os.MkdirTemp("", "lanet-check-")
	if err != nil {
		fail("创建临时目录: %v", err)
	}
	defer os.RemoveAll(dir)

	fmt.Println("== [1/6] 启动节点 A（连接审批开启，需人工同意） ==")
	nodeA, err := lanet.New(ctx, lanet.Config{
		Name:       "node-a",
		Standalone: true,
		NetworkKey: networkKey,
		DBPath:     filepath.Join(dir, "a.db"),
		// e2e 专注发现与流互通：防火墙全开（防火墙语义由 pvn-firewall-check 专测）。
		FirewallMode: lanet.FirewallModeAllowAll,
		ConsoleAddr:  "127.0.0.1:19311", // 固定端口，便于验证控制台 HTTP 接口
	})
	if err != nil {
		fail("创建节点 A: %v", err)
	}
	defer nodeA.Close()
	go nodeA.Run(ctx)
	infoA := nodeA.Info()
	fmt.Printf("A: 节点ID=%s 虚拟IP=%s\n", infoA.PeerID, infoA.VirtualIP)

	fmt.Println("== [2/6] 启动节点 B（以 A 为引导种子） ==")
	seeds := multiaddrStrings(nodeA)
	if len(seeds) == 0 {
		fail("节点 A 无可用监听地址，无法作为引导种子")
	}
	nodeB, err := lanet.New(ctx, lanet.Config{
		Name:         "node-b",
		Standalone:   true,
		NetworkKey:   networkKey,
		Bootstrap:    seeds,
		DBPath:       filepath.Join(dir, "b.db"),
		FirewallMode: lanet.FirewallModeAllowAll,
		ConsoleAddr:  "127.0.0.1:19312",
	})
	if err != nil {
		fail("创建节点 B: %v", err)
	}
	defer nodeB.Close()
	go nodeB.Run(ctx)
	infoB := nodeB.Info()
	fmt.Printf("B: 节点ID=%s 虚拟IP=%s\n", infoB.PeerID, infoB.VirtualIP)

	// 双方都提供 echo。
	nodeB.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})
	nodeA.OnStream(func(stream lanet.Stream) {
		defer stream.Close()
		_, _ = io.Copy(stream, stream)
	})

	// [3] 流量控制：双方地址簿均为空 → 不该主动发起 DHT 查找。
	fmt.Println("== [3/6] 流量控制：空地址簿不发起主动 DHT 查找 ==")
	time.Sleep(3 * time.Second) // 至少跑过一轮广播周期
	if a, b := len(nodeA.TrustedPeers()), len(nodeB.TrustedPeers()); a != 0 || b != 0 {
		fail("空地址簿阶段不该有已信任节点（A=%d B=%d）", a, b)
	}
	if n := len(nodeA.NetMap().Members); n != 0 {
		fmt.Printf("  提示：A 成员表有 %d 个成员（被动发现不建连，成员表应为 0）\n", n)
	}
	fmt.Println("  地址簿为空：仅自身的 DHT 广播（Provide），无查询流量 ✓")
	// B 以 A 为引导种子，双方传输层连通，但审批未过 → 不得进入成员表。
	if n := len(nodeB.NetMap().Members); n != 0 {
		fail("未审批阶段 B 不应有任何成员（实际 %d 个）", n)
	}
	fmt.Println("  审批未通过：双方成员表均为空（完全隔离）✓")

	// [4] B 按节点 ID 连接 A → 应得到 Pending（A 尚未同意）。
	fmt.Println("== [4/6] B 按节点 ID 连接 A（等待 A 同意） ==")
	res, err := nodeB.ConnectPeer(ctx, infoA.PeerID)
	if err != nil {
		fail("B 按节点 ID 连接 A 失败: %v", err)
	}
	if !res.Pending {
		fail("A 尚未同意，B 应得到 Pending（申请已送达），实际 %+v", res)
	}
	fmt.Printf("  B 得到待审批提示：%s\n", res.Message)

	// A 的待审批列表应出现 B。
	waitPending(fail, nodeA)
	fmt.Printf("  A 的待审批列表：%d 条（来自 %s...）✓\n", len(nodeA.PendingList()), short(nodeA.PendingList()[0].PeerID))

	// [5] 控制台 HTTP 接口：前后端字段一致性（页面就是调这几个接口）。
	// A 是收到申请的一方（待审批非空），B 已添加过 A（用它的控制台做连接调用）。
	fmt.Println("== [5/6] 控制台 HTTP 接口验证 ==")
	checkConsoleAPIs(fail, nodeA, nodeB, infoA.PeerID)

	// [6] A 同意 → 双向连通 → echo。
	fmt.Println("== [6/6] A 同意连接申请 → 双向连通 → 按虚拟 IP echo ==")
	if err = nodeA.ApprovePeer(infoB.PeerID); err != nil {
		fail("A 同意 B 的连接申请失败: %v", err)
	}
	if n := len(nodeA.PendingList()); n != 0 {
		fail("同意后 A 的待审批列表应清空（实际剩 %d 条）", n)
	}
	fmt.Println("  A 已同意（永久信任，待审批列表清空）✓")

	// 双向成员表互见（名称由 info 协议交换补齐）。
	waitMembers(fail, "A 看到 B", nodeA, infoB.PeerID, "node-b")
	waitMembers(fail, "B 看到 A", nodeB, infoA.PeerID, "node-a")
	fmt.Println("  双方成员表互见（含名称）✓")

	if err := echoRound(nodeB, infoA.VirtualIP, "hello-from-b"); err != nil {
		fail("B→A echo: %v", err)
	}
	fmt.Println("B→A echo 往返 OK")
	if err := echoRound(nodeA, infoB.VirtualIP, "hello-from-a"); err != nil {
		fail("A→B echo: %v", err)
	}
	fmt.Println("A→B echo 往返 OK")

	fmt.Println("\nPASS 无服务器组网端到端验证（流量控制 + 按 ID 连接 + 审批 + 双向 echo 全通）")
}

// waitPending 等待待审批列表非空。
func waitPending(fail func(string, ...any), c *lanet.Client) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.PendingList()) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fail("待审批列表应出现申请节点（实际为空）")
}

// checkConsoleAPIs 按前端页面的调用方式验证控制台 HTTP 接口：
// 字段名、结构与前端 JS 读取路径必须完全一致，否则页面会静默显示空白。
//
//   - pendingHost：收到了连接申请的控制台（待审批列表应非空）；
//   - caller：发起连接调用用的控制台，targetPeerID 为被连接方节点 ID
//     （caller 已添加过 target，重复调用应幂等）。
func checkConsoleAPIs(fail func(string, ...any), pendingHost, caller *lanet.Client, targetPeerID string) {
	get := func(c *lanet.Client, path string) map[string]any {
		base := c.ConsoleURL()
		if base == "" {
			fail("控制台未启动（ConsoleURL 为空）")
		}
		resp, err := http.Get(base + path)
		if err != nil {
			fail("GET %s 失败: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fail("GET %s 状态码 %d", path, resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			fail("GET %s 响应非 JSON: %v", path, err)
		}
		return out
	}
	post := func(c *lanet.Client, path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		resp, err := http.Post(c.ConsoleURL()+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			fail("POST %s 失败: %v", path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// /api/state：页面主数据源，检查审批相关字段都在。
	st := get(caller, "/api/state")
	for _, key := range []string{"info", "members", "pending", "pending_count", "auto_accept", "require_approval", "seed_addrs"} {
		if _, ok := st[key]; !ok {
			fail("/api/state 缺少字段 %q（前端会读不到）", key)
		}
	}
	if st["require_approval"] != true {
		fail("/api/state.require_approval 应为 true，实际 %v", st["require_approval"])
	}
	if st["auto_accept"] != false {
		fail("/api/state.auto_accept 应为 false，实际 %v", st["auto_accept"])
	}
	fmt.Println("  GET /api/state：审批字段齐全（pending/pending_count/auto_accept/require_approval）✓")

	// 收到申请的一方：/api/state.pending 与 /api/pending 都应含申请记录。
	hostState := get(pendingHost, "/api/state")
	pending, ok := hostState["pending"].([]any)
	if !ok || len(pending) == 0 {
		fail("/api/state.pending 应含待审批记录，实际 %v", hostState["pending"])
	}
	item, _ := pending[0].(map[string]any)
	if _, ok := item["peer_id"]; !ok {
		fail("/api/state.pending[0] 缺少 peer_id 字段")
	}
	if _, ok := item["created_at"]; !ok {
		fail("/api/state.pending[0] 缺少 created_at 字段（页面显示申请时间要用）")
	}
	if cnt, _ := hostState["pending_count"].(float64); int(cnt) != len(pending) {
		fail("/api/state.pending_count 应为 %d，实际 %v", len(pending), hostState["pending_count"])
	}
	fmt.Println("  GET /api/state.pending：待审批记录含 peer_id 与 created_at ✓")

	hostPending := get(pendingHost, "/api/pending")
	if plist, _ := hostPending["pending"].([]any); len(plist) == 0 {
		fail("/api/pending 应返回待审批记录")
	}
	fmt.Println("  GET /api/pending：待审批记录可读 ✓")

	// /api/peers：地址簿接口（caller 已添加过 target → 应非空）。
	pr := get(caller, "/api/peers")
	peers, ok := pr["peers"].([]any)
	if !ok {
		fail("/api/peers 缺少 peers 字段")
	}
	if len(peers) == 0 {
		fail("/api/peers 地址簿应有已添加的节点")
	}
	fmt.Println("  GET /api/peers：地址簿可读且含已添加节点 ✓")

	// POST /api/connect-peer：统一输入框后端。重复调用应幂等返回。
	code, out := post(caller, "/api/connect-peer", map[string]any{"address": targetPeerID})
	if code != http.StatusOK {
		fail("POST /api/connect-peer 状态码 %d：%v", code, out)
	}
	if out["peer_id"] != targetPeerID {
		fail("POST /api/connect-peer 应回带对端 peer_id，实际 %v", out["peer_id"])
	}
	if _, ok := out["message"]; !ok {
		fail("POST /api/connect-peer 应返回 message（页面展示用）")
	}
	fmt.Printf("  POST /api/connect-peer：按节点 ID 连接返回 pending=%v（幂等）✓\n", out["pending"])

	// 非法输入必须被拒（不能静默成功）。
	code, _ = post(caller, "/api/connect-peer", map[string]any{"address": "192.168.1.1"})
	if code == http.StatusOK {
		fail("POST /api/connect-peer 对非法输入应返回错误状态码")
	}
	fmt.Println("  POST /api/connect-peer：非法输入被拒 ✓")

	// POST /api/pending：非法输入不应 500 崩溃。
	code, _ = post(pendingHost, "/api/pending", map[string]any{"peer_id": "not-a-peer-id", "approve": false})
	if code == http.StatusInternalServerError {
		fail("POST /api/pending 非法输入不应 500")
	}
	fmt.Println("  POST /api/pending：非法输入不崩溃 ✓")
}

// waitMembers 等待 src 的成员表里出现 wantPeerID（且名称已由 info 交换补齐）。
func waitMembers(fail func(string, ...any), what string, src *lanet.Client, wantPeerID, wantName string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range src.NetMap().Members {
			if m.PeerID == wantPeerID && m.Name == wantName {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	fail("等待超时：%s 未互见", what)
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
		if strings.Contains(s, "webrtc") {
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
