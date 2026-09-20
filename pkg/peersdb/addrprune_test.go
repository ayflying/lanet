package peersdb

import (
	"context"
	"fmt"
	"testing"
)

// seedRawAddrs 绕过写入侧过滤直接写库，用于模拟「老版本对端积累下来的存量脏数据」。
func seedRawAddrs(t *testing.T, d *DB, peerID string, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		if _, err := d.db.ExecContext(context.Background(),
			`INSERT OR IGNORE INTO peer_addrs (peer_id, addr) VALUES (?, ?)`, peerID, a); err != nil {
			t.Fatalf("seed addr %s: %v", a, err)
		}
	}
}

// 存量脏地址（回环 / 链路本地 / overlay / circuit）必须能被一遍清掉——
// 真机实测某节点地址簿 404 条里有 169 条是回环/链路本地、130 条是 circuit。
func TestPruneUnreachableAddrs(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a", Name: "a"}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	dirty := []string{
		"/ip4/127.0.0.1/tcp/52854",
		"/ip4/169.254.153.138/tcp/52854",
		"/ip4/10.7.207.102/tcp/4001",
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",
	}
	keep := "/ip4/43.136.124.167/tcp/4001"
	seedRawAddrs(t, d, "peer-a", append(append([]string{}, dirty...), keep)...)

	n, err := d.PruneUnreachableAddrs(ctx)
	if err != nil {
		t.Fatalf("prune unreachable: %v", err)
	}
	if n != int64(len(dirty)) {
		t.Fatalf("清理条数 = %d, want %d", n, len(dirty))
	}
	got, err := d.KnownAddrs(ctx, "peer-a")
	if err != nil {
		t.Fatalf("known addrs: %v", err)
	}
	if len(got) != 1 || got[0] != keep {
		t.Fatalf("剩余地址 = %v, want 仅 %s", got, keep)
	}
	// 幂等：再清一次不应再删。
	if n, err = d.PruneUnreachableAddrs(ctx); err != nil || n != 0 {
		t.Fatalf("重复清理 = (%d, %v), want (0, nil)", n, err)
	}
}

// 写入侧闸门：拨号失败结果里的不可达地址连记录都不该留（它们永远拨不通，
// 留着只会让每次「按 ID 连接」多试一条）。
func TestNoteDialResultRejectsUnreachable(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a"}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, a := range []string{
		"/ip4/127.0.0.1/tcp/4001",
		"/ip4/169.254.1.1/tcp/4001",
		"/ip4/10.7.1.1/tcp/4001",
		"/ip4/3.3.3.3/tcp/4001/p2p-circuit",
	} {
		if err := d.NoteDialResult(ctx, "peer-a", a, false); err != nil {
			t.Fatalf("note dial %s: %v", a, err)
		}
	}
	if got, _ := d.KnownAddrs(ctx, "peer-a"); len(got) != 0 {
		t.Fatalf("不可达地址不应入库，实际 %v", got)
	}
	// 可达地址照常记。
	if err := d.NoteDialResult(ctx, "peer-a", "/ip4/43.136.124.167/tcp/4001", false); err != nil {
		t.Fatalf("note dial ok: %v", err)
	}
	if got, _ := d.KnownAddrs(ctx, "peer-a"); len(got) != 1 {
		t.Fatalf("可达地址应入库，实际 %v", got)
	}
}

// 运行期也要收敛：一次 upsert 灌入 40 条合法地址，库内条数不得超过上限。
// （历史缺陷：修剪只在开库时跑一次，运行期无上限，单节点涨到 110 条。）
func TestUpsertPeerPrunesAtRuntime(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	addrs := make([]string, 0, 40)
	for i := 1; i <= 40; i++ {
		addrs = append(addrs, fmt.Sprintf("/ip4/8.8.%d.%d/tcp/4001", i/256, i%256))
	}
	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a"}, addrs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := d.KnownAddrs(ctx, "peer-a")
	if err != nil {
		t.Fatalf("known addrs: %v", err)
	}
	if len(got) > prunePeerAddrsKeep {
		t.Fatalf("地址数 %d 超过上限 %d", len(got), prunePeerAddrsKeep)
	}
	if len(got) == 0 {
		t.Fatal("修剪过度：不应清空全部地址")
	}
}

// 回归：修剪 SQL 曾经少一个右括号而失败，错误又被调用方静默吞掉，导致
// 地址簿从未被真正修剪。这里直接对 PrunePeerAddrs 断言「不报错且生效」。
func TestPrunePeerAddrsActuallyWorks(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.UpsertPeer(ctx, Peer{PeerID: "peer-a"}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	addrs := make([]string, 0, 40)
	for i := 1; i <= 40; i++ {
		addrs = append(addrs, fmt.Sprintf("/ip4/8.8.%d.%d/tcp/4001", i/256, i%256))
	}
	seedRawAddrs(t, d, "peer-a", addrs...)

	n, err := d.PrunePeerAddrs(ctx, 4)
	if err != nil {
		t.Fatalf("prune peer addrs: %v", err)
	}
	if n != 36 {
		t.Fatalf("修剪条数 = %d, want 36", n)
	}
	if got, _ := d.KnownAddrs(ctx, "peer-a"); len(got) != 4 {
		t.Fatalf("剩余地址数 = %d, want 4", len(got))
	}
}
