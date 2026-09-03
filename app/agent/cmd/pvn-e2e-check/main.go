package main

// pvn-e2e-check：本地端到端验证。
// 拓扑：本地控制面 HTTP 服务 + Relay + AgentA（建群）+ AgentB（凭邀请入群）。
// 验证链路：创建群组 → 邀请加入 → 通告地址 → NetMap 路由 → 直连隧道 → 流上双向收发。
//
// 控制面通过独立进程启动（go run ./app/ctl），本程序按配置端口访问其真实 HTTP 接口。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayflying/pvn/app/agent/internal/service/peersource"
	"github.com/ayflying/pvn/pkg/netmapclient"
	p2pkit "github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/ayflying/pvn/pkg/protocol"
	tunnelsvc "github.com/ayflying/pvn/pkg/tunnel"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. 启动真实控制面子进程（随机空闲端口 + 独立临时 SQLite 库）。
	stopCtl, ctlBaseURL, err := startControlPlane()
	if err != nil {
		log.Fatalf("start control plane: %v", err)
	}
	defer stopCtl()
	if err = waitHealthy(ctx, ctlBaseURL, 20*time.Second); err != nil {
		log.Fatalf("control plane health: %v", err)
	}
	log.Printf("control-plane ready at %s", ctlBaseURL)

	// 2. 启动 Relay 并注册到控制面。
	relay, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs:  []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
		RelayService: true,
		UserAgent:    "pvn-e2e-relay/0.1.0",
	})
	if err != nil {
		log.Fatalf("start relay: %v", err)
	}
	defer relay.Close()
	relayInfo := p2pkit.AddrInfo(relay)
	log.Printf("relay started peer=%s", relay.ID())

	mustPostJSON(ctlBaseURL+"/v1/relays/register", map[string]any{
		"peer_id": relay.ID().String(), "addrs": multiaddrStrings(relay), "region": "local", "score": 100,
	})

	// 3. AgentA 创建群组（创建者自动入组）。
	hostA := newAgentHost(ctx, "agent-a")
	defer hostA.Close()

	createResp := mustPostJSON(ctlBaseURL+"/v1/groups/create", map[string]any{
		"peer_id": hostA.ID().String(), "name": "agent-a", "os": "windows", "group_name": "e2e",
	})
	inviteCode := createResp["invite_code"].(string)
	ipA := createResp["creator"].(map[string]any)["virtual_ip"].(string)
	log.Printf("group created invite=%s ipA=%s", inviteCode, ipA)

	// 4. AgentB 凭邀请码加入。
	hostB := newAgentHost(ctx, "agent-b")
	defer hostB.Close()

	joinResp := mustPostJSON(ctlBaseURL+"/v1/groups/join", map[string]any{
		"invite_code": inviteCode, "peer_id": hostB.ID().String(), "name": "agent-b", "os": "linux",
	})
	ipB := joinResp["member"].(map[string]any)["virtual_ip"].(string)
	log.Printf("agent-b joined ipB=%s", ipB)

	// 5. 双方通告可达地址，并连接 Relay 作为中继/打洞备用路径。
	mustPostJSON(ctlBaseURL+"/v1/groups/announce", map[string]any{"peer_id": hostA.ID().String(), "addrs": multiaddrStrings(hostA)})
	mustPostJSON(ctlBaseURL+"/v1/groups/announce", map[string]any{"peer_id": hostB.ID().String(), "addrs": multiaddrStrings(hostB)})
	if err = hostA.Connect(ctx, relayInfo); err != nil {
		log.Fatalf("agent-a connect relay: %v", err)
	}
	if err = hostB.Connect(ctx, relayInfo); err != nil {
		log.Fatalf("agent-b connect relay: %v", err)
	}

	// 6. AgentB 作为被叫方：监听隧道流，回显数据。
	// 注意：先读固定长度再回写，避免 ReadAll 阻塞在等发送端关闭上。
	hostB.SetStreamHandler(protocol.Tunnel, func(stream network.Stream) {
		data := make([]byte, 8) // ping-e2e
		if _, err := io.ReadFull(stream, data); err != nil {
			_ = stream.Reset()
			return
		}
		if _, err := stream.Write([]byte("echo:" + string(data))); err != nil {
			_ = stream.Reset()
			return
		}
		_ = stream.Close()
	})

	// 7. AgentA：拉取群组 NetMap → 按虚拟 IP 通过隧道服务建流（直连优先，失败走中继）。
	netmapA := netmapclient.NewClient(ctlBaseURL, hostA.ID().String())
	snapshot, err := netmapA.Refresh(ctx)
	if err != nil {
		log.Fatalf("agent-a refresh netmap: %v", err)
	}
	if len(snapshot.Members) != 2 {
		log.Fatalf("netmap members = %d, want 2", len(snapshot.Members))
	}
	log.Printf("netmap ok group=%s cidr=%s members=%d", snapshot.GroupID, snapshot.CIDR, len(snapshot.Members))

	tunnelA := tunnelsvc.New(hostA, netmapA, peersource.NewClient(ctlBaseURL))
	stream, viaRelay, err := tunnelA.OpenStreamToVirtualIP(ctx, ipB)
	if err != nil {
		log.Fatalf("open tunnel to %s: %v", ipB, err)
	}
	path := "direct"
	if viaRelay {
		path = "relay"
	}
	log.Printf("tunnel opened to=%s path=%s", ipB, path)

	if _, err = stream.Write([]byte("ping-e2e")); err != nil {
		log.Fatalf("tunnel write: %v", err)
	}
	// 回显长度固定：echo: + ping-e2e = 14 字节，读满即算成功。
	reply := make([]byte, len("echo:ping-e2e"))
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.ReadFull(stream, reply); err != nil {
		log.Fatalf("tunnel read: %v", err)
	}
	if string(reply) != "echo:ping-e2e" {
		log.Fatalf("unexpected reply %q", string(reply))
	}
	_ = stream.Close()
	log.Printf("payload roundtrip ok reply=%q", string(reply))

	fmt.Printf("e2e-ok group=%s ipA=%s ipB=%s path=%s\n", snapshot.GroupID, ipA, ipB, path)
}

func newAgentHost(ctx context.Context, name string) host.Host {
	h, err := p2pkit.NewHost(ctx, p2pkit.HostSpec{
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
		UserAgent:   "pvn-e2e-" + name + "/0.1.0",
	})
	if err != nil {
		log.Fatalf("start %s: %v", name, err)
	}
	return h
}

func multiaddrStrings(h host.Host) []string {
	items := make([]string, 0, len(h.Addrs()))
	for _, addr := range h.Addrs() {
		items = append(items, addr.String())
	}
	return items
}

// startControlPlane 以子进程方式启动真实 ctl 服务。
// 先本机探测一个空闲端口，再通过 PVN_CTL_ADDR 覆盖监听地址；
// 每次验证使用独立的临时 SQLite 数据库，避免多实例共享数据库文件产生锁冲突，
// 也保证 e2e 数据不会污染正式库；验证结束后随临时目录一并清理。
func startControlPlane() (func(), string, error) {
	tmpDir, err := os.MkdirTemp("", "pvn-e2e-db-*")
	if err != nil {
		return nil, "", fmt.Errorf("create temp dir for e2e db: %w", err)
	}
	port, err := freePort(18080)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, "", fmt.Errorf("find free port: %w", err)
	}
	dbPath := filepath.Join(tmpDir, "ctl.db")
	cmd := exec.Command("go", "run", "./app/ctl")
	cmd.Env = append(osEnviron(),
		fmt.Sprintf("PVN_CTL_ADDR=127.0.0.1:%d", port),
		"PVN_CTL_DB="+dbPath,
	)
	cmd.Dir = repoRoot()
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, "", fmt.Errorf("launch ctl process: %w", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(tmpDir)
	}, baseURL, nil
}

// freePort 从 start 开始找到第一个未被占用的 TCP 端口。
func freePort(start int) (int, error) {
	for port := start; port < start+200; port++ {
		conn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()
			return port, nil
		}
	}
	return 0, errors.New("no free port in range")
}

func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("control plane not healthy: %w", errors.Join(lastErr))
}

func mustPostJSON(url string, payload map[string]any) map[string]any {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Fatalf("marshal payload for %s: %v", url, err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read response from %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("post %s: status %d body %s", url, resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("decode response from %s: %v", url, err)
	}
	return out
}
