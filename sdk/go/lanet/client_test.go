package lanet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFakeCTL 启动一个最小化的 ctl 假服务：支持 create/join/netmap/announce/candidates。
func newFakeCTL(t *testing.T, invite string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "ok", "data": data})
	}

	mux.HandleFunc("/v1/groups/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PeerID    string `json:"peer_id"`
			GroupName string `json:"group_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.PeerID == "" || req.GroupName == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "message": "peer_id and group_name are required"})
			return
		}
		write(w, map[string]any{
			"group":       map[string]any{"id": "grp-1", "name": req.GroupName},
			"creator":     map[string]any{"peer_id": req.PeerID, "virtual_ip": "10.7.0.1", "virtual_ipv6": "fd00:6c61:6e65::2"},
			"invite_code": "grp-test-invite-code",
		})
	})

	mux.HandleFunc("/v1/groups/join", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InviteCode string `json:"invite_code"`
			PeerID     string `json:"peer_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.InviteCode != invite {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "message": "invalid invite code"})
			return
		}
		write(w, map[string]any{
			"group":  map[string]any{"id": "grp-1", "name": "fake-group"},
			"member": map[string]any{"peer_id": req.PeerID, "virtual_ip": "10.7.0.2", "virtual_ipv6": "fd00:6c61:6e65::3"},
		})
	})

	mux.HandleFunc("/v1/groups/netmap", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"group_id":   "grp-1",
			"group_name": "fake-group",
			"cidr":       "10.7.0.0/24",
			"version":    1,
			"members":    []any{},
		})
	})

	mux.HandleFunc("/v1/groups/announce", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"status": "announced"})
	})

	mux.HandleFunc("/v1/relays/candidates", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"candidates": []any{}})
	})

	return httptest.NewServer(mux)
}

func TestNewCreatesGroup(t *testing.T) {
	srv := newFakeCTL(t, "")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-test", GroupName: "fake-group",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err != nil {
		t.Fatalf("New(create): %v", err)
	}
	defer client.Close()

	info := client.Info()
	if info.GroupID != "grp-1" {
		t.Errorf("group id = %q, want grp-1", info.GroupID)
	}
	if info.VirtualIP != "10.7.0.1" {
		t.Errorf("virtual ip = %q, want 10.7.0.1", info.VirtualIP)
	}
	if info.VirtualIPv6 != "fd00:6c61:6e65::2" {
		t.Errorf("virtual ipv6 = %q, want fd00:6c61:6e65::2（应采用控制面分配值）", info.VirtualIPv6)
	}
	if !info.Created || info.InviteCode == "" {
		t.Errorf("created = %v, invite = %q; want created with invite", info.Created, info.InviteCode)
	}
	if client.peerID == "" {
		t.Error("peer id should not be empty")
	}
}

func TestNewJoinsGroupWithInvite(t *testing.T) {
	srv := newFakeCTL(t, "grp-test-invite-code")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-join", InviteCode: "grp-test-invite-code",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err != nil {
		t.Fatalf("New(join): %v", err)
	}
	defer client.Close()

	info := client.Info()
	if info.VirtualIP != "10.7.0.2" {
		t.Errorf("virtual ip = %q, want 10.7.0.2", info.VirtualIP)
	}
	if info.VirtualIPv6 != "fd00:6c61:6e65::3" {
		t.Errorf("virtual ipv6 = %q, want fd00:6c61:6e65::3（应采用控制面分配值）", info.VirtualIPv6)
	}
	if info.Created {
		t.Error("join mode should not be marked as created")
	}
}

// TestNewAdoptsIPv6FromNetMapWhenResponseLacksIt 创建响应没带 IPv6、但 NetMap 里
// 有本节点地址时（控制面只在该字段上补数据的情况），必须采用 NetMap 的值，
// 否则 TUN 不会配 v6 地址、也不会写成员 v6 路由——IPv6 数据面会静默失效。
func TestNewAdoptsIPv6FromNetMapWhenResponseLacksIt(t *testing.T) {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "ok", "data": data})
	}
	mux.HandleFunc("/v1/groups/create", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"group":       map[string]any{"id": "grp-1", "name": "netmap-only"},
			"creator":     map[string]any{"virtual_ip": "10.7.0.1"},
			"invite_code": "grp-netmap",
		})
	})
	mux.HandleFunc("/v1/groups/netmap", func(w http.ResponseWriter, r *http.Request) {
		peerID := r.URL.Query().Get("peer_id")
		write(w, map[string]any{
			"group_id": "grp-1", "cidr": "10.7.0.0/24", "cidr_v6": "fd00:6c61:6e65::/64", "version": 1,
			"members": []any{map[string]any{
				"peer_id": peerID, "name": "netmap-only",
				"virtual_ip": "10.7.0.1", "virtual_ipv6": "fd00:6c61:6e65::2",
			}},
		})
	})
	mux.HandleFunc("/v1/groups/announce", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"status": "announced"})
	})
	mux.HandleFunc("/v1/relays/candidates", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"candidates": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-netmap", GroupName: "netmap-only",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err != nil {
		t.Fatalf("New(create): %v", err)
	}
	defer client.Close()
	if got := client.Info().VirtualIPv6; got != "fd00:6c61:6e65::2" {
		t.Errorf("virtual ipv6 = %q，期望从 NetMap 采用 fd00:6c61:6e65::2", got)
	}
}

// TestNewWithoutControlPlaneIPv6FallsBackToIPv4Only 旧版控制面不返回 virtual_ipv6
// 时必须保持旧行为：不报错、不配 IPv6（而不是把空地址当成有效地址）。
func TestNewWithoutControlPlaneIPv6FallsBackToIPv4Only(t *testing.T) {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "ok", "data": data})
	}
	mux.HandleFunc("/v1/groups/create", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"group":       map[string]any{"id": "grp-1", "name": "old-ctl"},
			"creator":     map[string]any{"virtual_ip": "10.7.0.1"},
			"invite_code": "grp-old",
		})
	})
	mux.HandleFunc("/v1/groups/netmap", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"group_id": "grp-1", "cidr": "10.7.0.0/24", "version": 1, "members": []any{}})
	})
	mux.HandleFunc("/v1/groups/announce", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"status": "announced"})
	})
	mux.HandleFunc("/v1/relays/candidates", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"candidates": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-old-ctl", GroupName: "old-ctl",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err != nil {
		t.Fatalf("New(create, 旧控制面): %v", err)
	}
	defer client.Close()
	if got := client.Info().VirtualIPv6; got != "" {
		t.Errorf("旧控制面下 virtual ipv6 = %q，期望空串（仅 IPv4）", got)
	}
	if got := client.selfVirtualIPv6(); got != "" {
		t.Errorf("selfVirtualIPv6() = %q，期望空串", got)
	}
}

func TestNewRejectsInvalidJoin(t *testing.T) {
	srv := newFakeCTL(t, "correct-code")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-bad", InviteCode: "wrong-code",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err == nil {
		t.Fatal("expected error joining with wrong invite code")
	}
	if !strings.Contains(err.Error(), "加入群组") {
		t.Errorf("error should wrap join failure, got: %v", err)
	}
}

// 初始化已创建 Host 后失败，必须释放监听，而不只是避免空指针。
func TestNewFailureReleasesListener(t *testing.T) {
	srv := newFakeCTL(t, "correct-code")
	defer srv.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "rollback-test", InviteCode: "wrong-code",
		ListenAddrs: []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)},
	})
	if err == nil || client != nil {
		t.Fatalf("预期构造失败，client=%v err=%v", client, err)
	}
	reopened, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("初始化失败未释放监听: %v", err)
	}
	_ = reopened.Close()
}

func TestNewRequiresMandatoryConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{Name: "x"}); err == nil {
		t.Error("expected error when CTLURL missing")
	}
	if _, err := New(context.Background(), Config{CTLURL: "http://x"}); err == nil {
		t.Error("expected error when Name missing")
	}
}

func TestOnStreamRegistersTunnelHandler(t *testing.T) {
	srv := newFakeCTL(t, "")
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := New(ctx, Config{
		CTLURL: srv.URL, Name: "sdk-stream", GroupName: "fake-group",
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	// 重复注册只应挂一次底层 SetStreamHandler（第二次调用不 panic 即可）。
	client.OnStream(func(s Stream) { _ = s.Close() })
	client.OnStream(func(s Stream) { _ = s.Close() })
}
