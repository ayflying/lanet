package lanet

import (
	"context"
	"encoding/json"
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
			"creator":     map[string]any{"peer_id": req.PeerID, "virtual_ip": "10.7.0.1"},
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
			"member": map[string]any{"peer_id": req.PeerID, "virtual_ip": "10.7.0.2"},
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
	if info.Created {
		t.Error("join mode should not be marked as created")
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
