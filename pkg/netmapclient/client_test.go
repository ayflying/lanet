package netmapclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefreshParsesGroupSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/groups/netmap" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("peer_id") != "peer-a" {
			t.Fatalf("unexpected peer_id %q", r.URL.Query().Get("peer_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    0,
			"message": "OK",
			"data": map[string]any{
				"group_id":   "g0",
				"group_name": "alpha",
				"cidr":       "100.64.0.0/24",
				"version":    3,
				"members": []map[string]any{
					{"peer_id": "peer-a", "name": "a", "os": "windows", "virtual_ip": "100.64.0.2", "addrs": []string{}},
					{"peer_id": "peer-b", "name": "b", "os": "linux", "virtual_ip": "100.64.0.3", "addrs": []string{"/ip4/203.0.113.5/udp/4001/quic-v1"}},
				},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "peer-a")
	snapshot, err := client.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if snapshot.GroupID != "g0" || len(snapshot.Members) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}

	routes := client.Routes()
	if len(routes) != 2 || routes[0].VirtualIP != "100.64.0.2" || routes[1].VirtualIP != "100.64.0.3" {
		t.Fatalf("routes not sorted as expected: %+v", routes)
	}
	route, ok := client.Resolve("100.64.0.3")
	if !ok || route.PeerID != "peer-b" || len(route.Addrs) != 1 {
		t.Fatalf("resolve failed: %+v ok=%v", route, ok)
	}
}

func TestRefreshRejectsErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"peer has not joined any group"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "ghost")
	if _, err := client.Refresh(context.Background()); err == nil {
		t.Fatal("expected error status to fail refresh")
	}
}

func TestRefreshRejectsBusinessError(t *testing.T) {
	// gf MiddlewareHandlerResponse 语义：HTTP 200 但 code != 0 表示业务错误。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":68,"message":"peer has not joined any group","data":null}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "ghost")
	_, err := client.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "peer has not joined any group") {
		t.Fatalf("expected business error to propagate, got %v", err)
	}
}

func TestAnnounceSendsPeerAndAddrs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/groups/announce" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			PeerID string   `json:"peer_id"`
			Addrs  []string `json:"addrs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.PeerID != "peer-a" || len(payload.Addrs) != 1 || !strings.HasPrefix(payload.Addrs[0], "/ip4/") {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"OK","data":{"status":"announced"}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "peer-a")
	if err := client.Announce(context.Background(), []string{"/ip4/203.0.113.9/tcp/4001"}); err != nil {
		t.Fatalf("announce: %v", err)
	}
}
