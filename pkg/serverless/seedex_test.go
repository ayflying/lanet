package serverless

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
)

type seedLifecycleHost struct {
	host.Host
	mu       sync.Mutex
	handlers map[protocol.ID]network.StreamHandler
}

func (h *seedLifecycleHost) SetStreamHandler(p protocol.ID, f network.StreamHandler) {
	h.mu.Lock()
	h.handlers[p] = f
	h.mu.Unlock()
}
func (h *seedLifecycleHost) RemoveStreamHandler(p protocol.ID) {
	h.mu.Lock()
	delete(h.handlers, p)
	h.mu.Unlock()
}
func (h *seedLifecycleHost) handler(p protocol.ID) network.StreamHandler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handlers[p]
}

func TestSeedEnableBeforeStartCloseRemovesHandlers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &seedLifecycleHost{Host: testHost(t, false), handlers: map[protocol.ID]network.StreamHandler{}}
	d, err := New(ctx, h, Config{NetworkKey: "seed-before-start", Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	d.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, GlobalEnabled: true})
	for _, p := range []protocol.ID{d.protoSeedsGroup, ProtocolSeedsGlobal} {
		if h.handler(p) == nil {
			t.Fatalf("Start前未注册种子协议: %s", p)
		}
	}
	if d.protocolsStarted {
		t.Fatal("测试要求未调用Start")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []protocol.ID{d.protoSeedsGroup, ProtocolSeedsGlobal} {
		if h.handler(p) != nil {
			t.Fatalf("Start前启用后Close仍残留种子协议: %s", p)
		}
	}
}

func TestSeedHandlersClosedOnHostReuse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &seedLifecycleHost{Host: testHost(t, false), handlers: map[protocol.ID]network.StreamHandler{}}
	d, err := New(ctx, h, Config{NetworkKey: "seed-old", Quiet: true, GroupSeedsEnabled: true, GlobalSeedsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oldGroup := d.protoSeedsGroup
	oldHandler := h.handler(oldGroup)
	if oldHandler == nil || h.handler(ProtocolSeedsGlobal) == nil {
		t.Fatal("种子入口未注册")
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []protocol.ID{oldGroup, ProtocolSeedsGlobal} {
		if h.handler(p) != nil {
			t.Fatalf("Close 后旧handler仍注册: %s", p)
		}
	}
	var merged, sourced atomic.Int32
	d.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, GlobalEnabled: true, Merge: func(string, string, []SeedRecord) { merged.Add(1) }, Source: func(string, int) []SeedRecord { sourced.Add(1); return nil }})
	if h.handler(oldGroup) != nil || h.handler(ProtocolSeedsGlobal) != nil {
		t.Fatal("关闭后重注册")
	}
	stale := &controlTestStream{reader: strings.NewReader(`{}`)}
	oldHandler(stale)
	if stale.resets.Load() == 0 || stale.read != 0 || merged.Load() != 0 || sourced.Load() != 0 {
		t.Fatal("旧在途handler在Close后仍处理")
	}
	newer, err := New(ctx, h, Config{NetworkKey: "seed-new", Quiet: true, GroupSeedsEnabled: true, GlobalSeedsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	if err = newer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if h.handler(oldGroup) != nil || h.handler(newer.protoSeedsGroup) == nil || h.handler(ProtocolSeedsGlobal) == nil {
		t.Fatal("复用host后协议入口归属错误")
	}
}

func TestSeedHandlerCloseDuringReadBlocksCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "seed-inflight", Quiet: true, GroupSeedsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var merged, sourced atomic.Int32
	d.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, Merge: func(string, string, []SeedRecord) { merged.Add(1) }, Source: func(string, int) []SeedRecord { sourced.Add(1); return nil }})
	reader, writer := io.Pipe()
	s := &controlTestStream{reader: reader}
	done := make(chan struct{})
	go func() { d.handleSeeds(s, SeedScopeGroup); close(done) }()
	payload, _ := json.Marshal(seedPayload{Scope: SeedScopeGroup, Fingerprint: d.seedFingerprint(SeedScopeGroup)})
	if _, err = writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("读完成后处理未退出")
	}
	if s.resets.Load() == 0 || merged.Load() != 0 || sourced.Load() != 0 {
		t.Fatalf("Close后回调仍执行 reset=%d merge=%d source=%d", s.resets.Load(), merged.Load(), sourced.Load())
	}
}

func TestSeedEnableCloseDoesNotReRegister(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &seedLifecycleHost{Host: testHost(t, false), handlers: map[protocol.ID]network.StreamHandler{}}
	d, err := New(ctx, h, Config{NetworkKey: "seed-race", Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			d.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, GlobalEnabled: true})
		}
	}()
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if h.handler(d.protoSeedsGroup) != nil || h.handler(ProtocolSeedsGlobal) != nil {
		t.Fatal("Close并发重挂handler")
	}
}

// TestSeedRateLimiter 限流器：同对端冷却、全局并发上限、退避。
func TestSeedRateLimiter(t *testing.T) {
	r := newSeedRateLimiter()
	t0 := time.Now()

	if !r.allow(testPeerA, t0) {
		t.Fatal("首次应放行")
	}
	if r.allow(testPeerA, t0.Add(time.Minute)) {
		t.Error("冷却期内再请求同一对端应被拒")
	}
	if !r.allow(testPeerA, t0.Add(seedsPeerCooldown+time.Second)) {
		t.Error("冷却结束后应放行")
	}

	// 并发上限：两次放行后 active=2（= seedsMaxConcurrent），第三个应被拒。
	r2 := newSeedRateLimiter()
	if !r2.allow(testPeerA, t0) || !r2.allow(testPeerB, t0) {
		t.Fatal("前两个不同对端应放行")
	}
	if r2.allow(testPeerC, t0) {
		t.Errorf("并发已达上限 %d，第三个应被拒", seedsMaxConcurrent)
	}
	// 归还一个额度后可以放行。
	r2.release()
	if !r2.allow(testPeerC, t0) {
		t.Error("归还额度后应放行")
	}
	// 多归还不会把 active 压成负数。
	r2.release()
	r2.release()
	r2.release()
	if !r2.allow(testPeerC, t0.Add(time.Hour)) {
		t.Error("active 被压成负数后仍应能放行")
	}

	// 退避：把 last 推到未来，退避期内一律拒绝。
	r3 := newSeedRateLimiter()
	r3.backoff(testPeerA, t0.Add(seedsUnsupportedBackoff))
	if r3.allow(testPeerA, t0) {
		t.Error("退避期内应被拒")
	}
	if r3.allow(testPeerA, t0.Add(seedsUnsupportedBackoff)) {
		t.Error("退避刚到期时仍在冷却窗口内，应继续被拒")
	}
	// 退避到期 + 一个正常冷却窗口之后才放行。
	if !r3.allow(testPeerA, t0.Add(seedsUnsupportedBackoff+seedsPeerCooldown+time.Second)) {
		t.Error("退避与冷却都过完后应放行")
	}
}

// TestSeedScopeEnabled 开关语义：默认关，只有显式打开的范围才启用。
//
// 走的是运行期入口 EnableSeedExchange（控制台改设置就是这条路径），
// 而不是直接构造 Discovery——运行期字段才是唯一事实来源。
func TestSeedScopeEnabled(t *testing.T) {
	d := &Discovery{}
	if d.seedScopeEnabled(SeedScopeGroup) || d.seedScopeEnabled(SeedScopeGlobal) {
		t.Error("默认应两个范围都关")
	}
	if d.seedScopeEnabled("bogus") {
		t.Error("未知范围应为关")
	}

	// 只开群内：全域必须仍为关。
	d2 := &Discovery{}
	d2.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true})
	if !d2.seedScopeEnabled(SeedScopeGroup) || d2.seedScopeEnabled(SeedScopeGlobal) {
		t.Error("只开群内时全域应仍为关")
	}

	// 只开全域：群内必须仍为关。
	d3 := &Discovery{}
	d3.EnableSeedExchange(SeedExchangeOptions{GlobalEnabled: true})
	if d3.seedScopeEnabled(SeedScopeGroup) || !d3.seedScopeEnabled(SeedScopeGlobal) {
		t.Error("只开全域时群内应仍为关")
	}

	// 重复调用即时覆盖（控制台来回改开关不能有残留）。
	d3.EnableSeedExchange(SeedExchangeOptions{GroupEnabled: true, GlobalEnabled: false})
	if !d3.seedScopeEnabled(SeedScopeGroup) || d3.seedScopeEnabled(SeedScopeGlobal) {
		t.Error("再次调用应即时覆盖两个开关")
	}
	d3.EnableSeedExchange(SeedExchangeOptions{})
	if d3.seedAnyScopeEnabled() {
		t.Error("传空参数应把两个开关都关掉")
	}
}

// TestSeedConfigBridge Config 初值经 initSeedRuntime 生效。
//
// 覆盖「上层提前在 Config 里配好种子参数」这条路径：没有它，
// 只有 EnableSeedExchange 被调用时才生效，配置会被静默忽略。
func TestSeedConfigBridge(t *testing.T) {
	src := func(scope string, max int) []SeedRecord { return nil }
	peers := func(scope string, max int) []string { return nil }

	d := &Discovery{}
	d.initSeedRuntime(Config{
		GroupSeedsEnabled: true,
		GlobalSeedsLimit:  7,
		SeedSource:        src,
		SeedPeers:         peers,
	})
	if !d.seedScopeEnabled(SeedScopeGroup) {
		t.Error("Config.GroupSeedsEnabled 应生效")
	}
	if d.seedScopeEnabled(SeedScopeGlobal) {
		t.Error("未配置时全域应保持关")
	}
	if !d.seedAnyScopeEnabled() {
		t.Error("群内开启后 seedAnyScopeEnabled 应为真")
	}
	if d.seedLimit != 7 {
		t.Errorf("Config.GlobalSeedsLimit 应生效，实际 %d", d.seedLimit)
	}
	if d.seedGate == nil {
		t.Error("initSeedRuntime 必须初始化限流器，否则入向请求会 panic")
	}
	if d.seedSourceFn == nil || d.seedPeersFn == nil {
		t.Error("Config 里的回调应被灌进运行期字段")
	}

	// 默认零值：全关、上限为 0（由上层回落默认值）。
	z := &Discovery{}
	z.initSeedRuntime(Config{})
	if z.seedAnyScopeEnabled() || z.seedLimit != 0 {
		t.Error("零值 Config 应保持全关且上限为 0")
	}
}

// TestSanitizeSeedRecords 发送侧规范化：去空/去重/剔脏地址/条数与地址数双上限。
func TestSanitizeSeedRecords(t *testing.T) {
	in := []SeedRecord{
		{PeerID: testPeerA, Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
		{PeerID: testPeerA, Addrs: []string{"/ip4/5.6.7.8/tcp/4001"}}, // 重复 ID：丢
		{PeerID: "", Addrs: []string{"/ip4/1.1.1.1/tcp/4001"}},        // 空 ID：丢
		{PeerID: testPeerB}, // 无地址：丢
		{
			PeerID: testPeerC,
			Addrs: []string{
				"/ip4/127.0.0.1/tcp/4001",       // 回环：丢
				"/ip4/169.254.1.2/tcp/4001",     // 链路本地：丢
				"/ip4/9.9.9.9/tcp/4001",         // 保留（1）
				"/ip4/8.8.8.8/tcp/4001",         // 保留（2）
				"/ip6/2408:824e::1/tcp/4001",    // 保留（3）
				"/ip4/7.7.7.7/udp/4001/quic-v1", // 保留（4）
				"/ip4/6.6.6.6/udp/4001/quic-v1", // 超出 seedsMaxAddrs：截断
			},
		},
	}
	got := sanitizeSeedRecords(in, seedsMaxRecords)
	if len(got) != 2 {
		t.Fatalf("应剩 2 条（A 与 C），实际 %d：%+v", len(got), got)
	}
	if len(got[1].Addrs) != seedsMaxAddrs {
		t.Errorf("单条地址数应被截到 %d，实际 %d：%v", seedsMaxAddrs, len(got[1].Addrs), got[1].Addrs)
	}
	for _, a := range got[1].Addrs {
		if a == "/ip4/127.0.0.1/tcp/4001" || a == "/ip4/169.254.1.2/tcp/4001" {
			t.Errorf("脏地址未被剔除：%s", a)
		}
	}

	// 条数上限。
	many := make([]SeedRecord, 0, seedsMaxRecords+10)
	for i := 0; i < seedsMaxRecords+10; i++ {
		many = append(many, SeedRecord{
			PeerID: testPeerA + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Addrs:  []string{"/ip4/1.2.3.4/tcp/4001"},
		})
	}
	_ = many // 上面的 ID 不保证合法 base58，这里只验证截断逻辑对条数生效
	trimmed := sanitizeSeedRecords(many, seedsMaxRecords)
	if len(trimmed) > seedsMaxRecords {
		t.Errorf("条数应被截到 %d，实际 %d", seedsMaxRecords, len(trimmed))
	}

	// maxRecords 越界时回落到硬上限，不放行超大值。
	if out := sanitizeSeedRecords(in, 1<<20); len(out) > seedsMaxRecords {
		t.Errorf("越界上限应回落到 %d，实际 %d", seedsMaxRecords, len(out))
	}
}

// TestMergeIncomingSeedsScopeIsolation 跨范围注入必须整批丢弃。
//
// 这是「群内种子与全域种子隔离」的第二道闸：协议 ID 已经隔离一次，
// 但故意在群内通道里声明 scope=global 的构造消息必须被挡住。
func TestMergeIncomingSeedsScopeIsolation(t *testing.T) {
	var merged []SeedRecord
	var mergeScope string
	d := &Discovery{cfg: Config{Quiet: true}}
	d.EnableSeedExchange(SeedExchangeOptions{
		Merge: func(scope, from string, records []SeedRecord) {
			merged = append(merged, records...)
			mergeScope = scope
		},
	})

	// scope 不匹配 → 整批丢弃，SeedMerge 不被调用。
	res := d.mergeIncomingSeeds(SeedScopeGroup, testPeerB, seedPayload{
		Scope:   SeedScopeGlobal, // 在群内通道里谎称全域
		Records: []SeedRecord{{PeerID: testPeerA, Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}}},
	})
	if res.Accepted != 0 || res.Dropped != 1 {
		t.Errorf("跨范围注入应整批丢弃：%+v", res)
	}
	if len(merged) != 0 {
		t.Error("跨范围注入不该触发合并回调")
	}

	// 正常同范围。
	res = d.mergeIncomingSeeds(SeedScopeGroup, testPeerB, seedPayload{
		Scope:   SeedScopeGroup,
		Records: []SeedRecord{{PeerID: testPeerA, Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}}},
	})
	if res.Accepted != 1 || mergeScope != SeedScopeGroup {
		t.Errorf("正常同范围应接受 1 条：res=%+v scope=%q", res, mergeScope)
	}
}

// TestMergeIncomingSeedsValidation 逐条校验：非法 ID / 本机自己 / 超量批 全部处理。
func TestMergeIncomingSeedsValidation(t *testing.T) {
	var got []SeedRecord
	d := &Discovery{cfg: Config{Quiet: true}}
	d.EnableSeedExchange(SeedExchangeOptions{
		Merge: func(scope, from string, records []SeedRecord) {
			got = append(got, records...)
		},
	})

	// 非法 PeerID（占位串无法解析为 libp2p ID）与空 ID 被丢。
	res := d.mergeIncomingSeeds(SeedScopeGroup, testPeerB, seedPayload{
		Scope: SeedScopeGroup,
		Records: []SeedRecord{
			{PeerID: "not-a-peer-id", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			{PeerID: "", Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
			{PeerID: testPeerA, Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}},
		},
	})
	if res.Accepted != 1 || res.Dropped != 2 {
		t.Errorf("应接受 1 条、丢弃 2 条：%+v", res)
	}
	if len(got) != 1 || got[0].PeerID != testPeerA {
		t.Errorf("合并内容不对：%+v", got)
	}

	// 超量批：整批丢弃（不做部分接受——半接受会让越权数据难以定位）。
	got = nil
	huge := make([]SeedRecord, seedsMaxRecords+1)
	for i := range huge {
		huge[i] = SeedRecord{PeerID: testPeerA, Addrs: []string{"/ip4/1.2.3.4/tcp/4001"}}
	}
	res = d.mergeIncomingSeeds(SeedScopeGroup, testPeerB, seedPayload{Scope: SeedScopeGroup, Records: huge})
	if res.Accepted != 0 || len(got) != 0 {
		t.Errorf("超量批应整批丢弃：res=%+v got=%d", res, len(got))
	}
}

// TestSeedTrafficCeiling 流量天花板：硬上限必须是「小」的常量。
//
// 用户明确要求「不要让流量爆炸」。这个测试把每个上限钉死，防止后续改代码时
// 被无声放大（改大了这里会红）。
func TestSeedTrafficCeiling(t *testing.T) {
	if seedsMaxRecords > 64 {
		t.Errorf("单消息条数上限不得高于 64，当前 %d", seedsMaxRecords)
	}
	if seedsMaxAddrs > 4 {
		t.Errorf("单条地址数上限不得高于 4，当前 %d", seedsMaxAddrs)
	}
	if seedsMaxPayload > 64*1024 {
		t.Errorf("单消息字节上限不得高于 64KB，当前 %d", seedsMaxPayload)
	}
	if seedsMaxConcurrent > 2 {
		t.Errorf("并发上限不得高于 2，当前 %d", seedsMaxConcurrent)
	}
	if seedsPeerCooldown < 5*time.Minute {
		t.Errorf("同对端冷却不得低于 5 分钟，当前 %s", seedsPeerCooldown)
	}
	if seedsPeersPerRound > 3 {
		t.Errorf("每轮对端数不得高于 3，当前 %d", seedsPeersPerRound)
	}
	if seedsRoundInterval < 10*time.Minute {
		t.Errorf("轮间隔不得低于 10 分钟，当前 %s", seedsRoundInterval)
	}
}
