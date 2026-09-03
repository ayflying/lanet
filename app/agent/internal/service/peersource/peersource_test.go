package peersource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestCandidates(t *testing.T) {
	privateKey, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("generate peer key: %v", err)
	}
	id, err := peer.IDFromPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("create peer id: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/relays/candidates" || request.URL.Query().Get("limit") != "2" {
			t.Fatalf("unexpected request: %s", request.URL.String())
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"candidates":[{"peer_id":"` + id.String() + `","addrs":["/ip4/127.0.0.1/tcp/4001"]}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	items, err := client.Candidates(context.Background(), 2)
	if err != nil {
		t.Fatalf("fetch candidates: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(items))
	}
	if items[0].ID != id {
		t.Fatalf("candidate peer id = %s, want %s", items[0].ID, id)
	}
	if len(items[0].Addrs) != 1 || items[0].Addrs[0].String() != "/ip4/127.0.0.1/tcp/4001" {
		t.Fatalf("unexpected candidate addresses: %v", items[0].Addrs)
	}
}

func TestCandidatesRejectsInvalidNumber(t *testing.T) {
	client := NewClient("http://127.0.0.1:8000")
	if _, err := client.Candidates(context.Background(), 0); err == nil {
		t.Fatal("expected validation error for zero candidate number")
	}
}
