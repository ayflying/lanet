package lanet

import (
	"context"
	"encoding/json"
	"github.com/ayflying/pvn/pkg/netmapclient"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/peersdb"
	"github.com/ayflying/pvn/pkg/serverless"
)

func TestConsoleOfflineSortByVerifiedLastSeen(t *testing.T) {
	ctx := context.Background()
	ctl := newFakeCTL(t, "")
	defer ctl.Close()
	c, err := New(ctx, Config{CTLURL: ctl.URL, Name: "sort-test", GroupName: "sort-test", ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now().Truncate(time.Second)
	snapshot := netmapclient.Snapshot{Members: []netmapclient.Member{
		{PeerID: "never", VirtualIP: "10.7.0.1", FirstSeen: now},
		{PeerID: "old", VirtualIP: "10.7.0.2", FirstSeen: now, LastSeen: now.Add(-time.Hour)},
		{PeerID: "recent", VirtualIP: "10.7.0.3", FirstSeen: now.Add(-time.Hour), LastSeen: now},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": snapshot})
	}))
	defer srv.Close()
	c.netmapCli = netmapclient.NewClient(srv.URL, c.peerID)
	if _, err = c.netmapCli.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c.apiState(recorder, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var state struct {
		Members []struct {
			PeerID   string `json:"peer_id"`
			LastSeen int64  `json:"last_seen"`
			Online   bool   `json:"online"`
		} `json:"members"`
	}
	if err = json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Members) != 3 {
		t.Fatalf("members: %s", recorder.Body.String())
	}
	for i, want := range []string{"recent", "old", "never"} {
		if state.Members[i].PeerID != want || state.Members[i].Online {
			t.Fatalf("offline order: %+v", state.Members)
		}
	}
	if state.Members[2].LastSeen != 0 {
		t.Fatal("never timestamp must map to zero")
	}
}

func TestPersistVerifiedMemberSkipsPlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := peersdb.Open(ctx, filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := &Client{peers: db}
	c.persistVerifiedMember(serverless.Member{PeerID: "never", FirstSeen: time.Now()})
	peers, err := db.ListPeers(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatal("placeholder polluted persistent last_seen")
	}
	seen := time.Now().Add(-time.Hour).Truncate(time.Second)
	c.persistVerifiedMember(serverless.Member{PeerID: "verified", Name: "node", VirtualIP: "10.7.0.2", LastSeen: seen})
	peers, err = db.ListPeers(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || !peers[0].LastSeen.Equal(seen) || peers[0].Trusted {
		t.Fatalf("verified timestamp / approval mapping failed: %+v", peers)
	}
	c.persistVerifiedMember(serverless.Member{PeerID: "verified"})
	peers, err = db.ListPeers(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !peers[0].LastSeen.Equal(seen) {
		t.Fatal("later placeholder overwrote verified activity")
	}
}
