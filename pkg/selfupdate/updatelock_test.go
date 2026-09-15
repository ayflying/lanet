package selfupdate

// 更新锁与候选筛选测试（0.5.49）。

import (
	"context"
	"path/filepath"
	"testing"
)

// countingPeers 记录候选视图被查询的次数（用来证明「更新在途时整轮跳过」）。
type countingPeers struct {
	peers []PeerInfo
	calls int
}

func (c *countingPeers) Peers() []PeerInfo {
	c.calls++
	return c.peers
}

// TestRoundSkipsWhenUpdateInFlight 更新锁：已有一轮更新在途（下载完成待重启、
// 或另一条更新路径正在跑）时，巡检必须整轮跳过 —— 连候选都不查，更不征询、
// 不下载。否则「已替换、待重启」的窗口里会反复更新、反复排重启。
func TestRoundSkipsWhenUpdateInFlight(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t)
	inflight := true
	// 候选列表故意放一个「比自己新」的节点：若门失效，它会被征询。
	src := &countingPeers{peers: []PeerInfo{
		{ID: "not-a-valid-peer-id", Version: "9.9.9", Platform: "windows/amd64"},
	}}
	updated := false
	c := New(h, src, Config{
		CurrentVersion: "0.5.0",
		Platform:       "windows/amd64",
		ExePath:        filepath.Join(dir, "lanet.exe"),
		ManifestPath:   filepath.Join(dir, "update-manifest.json"),
		Quiet:          true,
		UpdateInFlight: func() bool { return inflight },
	}, func(string, Manifest) { updated = true })

	c.round(context.Background())
	if src.calls != 0 {
		t.Fatalf("更新在途时不应查询候选，实际查询 %d 次", src.calls)
	}
	if updated {
		t.Fatal("更新在途时不应触发更新回调")
	}

	// 对照：锁放开后候选会被查询（证明上一断言不是「什么都没跑」）。
	inflight = false
	c.round(context.Background())
	if src.calls == 0 {
		t.Fatal("锁放开后应查询候选")
	}
	if updated {
		t.Fatal("候选 ID 非法（征询必失败）时不应触发更新回调")
	}
}

// TestRoundWithoutGateStillRuns 未配置 UpdateInFlight（单测/旧集成）时门不生效，
// 巡检照常查询候选 —— 保证这个门是「可选增强」而不是把更新链整条掐死。
func TestRoundWithoutGateStillRuns(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t)
	src := &countingPeers{peers: []PeerInfo{
		{ID: "not-a-valid-peer-id", Version: "9.9.9", Platform: "windows/amd64"},
	}}
	c := New(h, src, Config{
		CurrentVersion: "0.5.0",
		Platform:       "windows/amd64",
		ExePath:        filepath.Join(dir, "lanet.exe"),
		ManifestPath:   filepath.Join(dir, "update-manifest.json"),
		Quiet:          true,
	}, nil)
	c.round(context.Background())
	if src.calls == 0 {
		t.Fatal("未配置更新锁时巡检应照常查询候选")
	}
}

// TestSelectCandidates 候选分层：版本已知更高的成员优先且全取；一个都没有
// 时才退而征询「版本未知」的节点（私有 DHT 网络里未加好友、未建连的节点），
// 且一轮最多 3 个。平台不同的候选一律丢弃。
func TestSelectCandidates(t *testing.T) {
	c := &Coordinator{cfg: Config{Platform: "windows/amd64", MinNewPeers: 1}}

	// 场景一：有更高的成员 → 只取它们，版本未知的候选不动（省流量）。
	got := c.selectCandidates("0.5.0", []PeerInfo{
		{ID: "p1", Version: "0.5.9", Platform: "windows/amd64"},
		{ID: "p2", Version: "0.5.0", Platform: "windows/amd64"}, // 同版本：排除
		{ID: "p3", Version: "0.4.9", Platform: "windows/amd64"}, // 更旧：排除
		{ID: "p4", Version: "0.6.0", Platform: "linux/amd64"},   // 平台不同：排除
		{ID: "p5"}, // 版本未知：本场景不取
	})
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("应只取更高的同平台成员 p1，实际 %+v", got)
	}

	// 场景二：没有更高的成员 → 转向版本未知的候选（DHT 网络节点）。
	got = c.selectCandidates("0.5.0", []PeerInfo{
		{ID: "p1", Version: "0.5.0", Platform: "windows/amd64"},
		{ID: "d1"}, {ID: "d2"}, {ID: "d3"}, {ID: "d4"}, {ID: "d5"},
	})
	if len(got) != 3 {
		t.Fatalf("版本未知候选应抽样 3 个，实际 %d 个: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Version != "" {
			t.Fatalf("抽样结果应全是版本未知的候选: %+v", p)
		}
	}

	// 场景三：全空 → 空结果（round 会因此直接返回）。
	if got = c.selectCandidates("0.5.0", nil); len(got) != 0 {
		t.Fatalf("无候选应返回空，实际 %+v", got)
	}
}
