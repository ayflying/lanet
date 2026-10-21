package serverless

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// 自动审批不能由被动发现或后台出向触发。
func TestAutoAcceptDoesNotInitiateRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ha, hb := testHost(t, false), testHost(t, false)
	var accepted, pending atomic.Int32
	d, err := New(ctx, ha, Config{NetworkKey: "direction", IsTrusted: func(string) bool { return false }, AutoAccept: func(string, []string, string) bool { accepted.Add(1); return true }, OnPending: func(string, []string, string) { pending.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	d.addMember(hb.ID(), hb.Addrs(), "test")
	if len(d.Peers()) != 0 {
		t.Fatal("被动发现不应自动加入成员")
	}
	if err := d.connectAndIdentifyContext(ctx, hb.ID()); err == nil {
		t.Fatal("后台出向不得自动信任陌生节点")
	}
	if accepted.Load() != 0 || pending.Load() != 0 {
		t.Fatalf("发现或出向触发了审批: accepted=%d pending=%d", accepted.Load(), pending.Load())
	}
}

// 删除后的普通重连不能复活；显式申请可自动审批，且申请标记必须从 RequestConnect 传到对端。
func TestAutoAcceptDeletedPeerExplicitRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ha, hb := testHost(t, false), testHost(t, false)
	var deleted atomic.Bool
	deleted.Store(true)
	var accepted atomic.Int32
	da, err := New(ctx, ha, Config{NetworkKey: "reapply", Name: "central", IsTrusted: func(string) bool { return !deleted.Load() }, IsUnfriended: func(string) bool { return deleted.Load() }, AutoReaccept: func(string, []string, string) bool { accepted.Add(1); deleted.Store(false); return true }})
	if err != nil {
		t.Fatal(err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "reapply", Name: "applicant"})
	if err != nil {
		t.Fatal(err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ai := peer.AddrInfo{ID: ha.ID(), Addrs: ha.Addrs()}
	if err = hb.Connect(ctx, ai); err != nil {
		t.Fatal(err)
	}
	resp, err := db.fetchInfo(ctx, ha.ID())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rejected != rejectionUnfriended || accepted.Load() != 0 {
		t.Fatalf("后台握手越过删除保护: %+v accepted=%d", resp, accepted.Load())
	}
	if _, err = db.RequestConnect(ctx, ai); err != nil {
		t.Fatalf("重新申请应自动同意: %v", err)
	}
	if deleted.Load() || accepted.Load() != 1 {
		t.Fatalf("未正确自动恢复: deleted=%v accepted=%d", deleted.Load(), accepted.Load())
	}
	found := false
	for _, m := range da.Peers() {
		if m.PeerID == hb.ID().String() {
			found = true
		}
	}
	if !found {
		t.Fatal("自动同意后未恢复成员")
	}
}

// 地址簿预热不依赖发现预先创建成员，也不能把普通重连变成重新申请。
func TestReconnectRestoresMemberWithoutConnectRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ha, hb := testHost(t, false), testHost(t, false)
	var deleted atomic.Bool
	var reaccepted atomic.Int32
	da, err := New(ctx, ha, Config{NetworkKey: "reconnect-direction", Name: "warmup", IsTrusted: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	db, err := New(ctx, hb, Config{NetworkKey: "reconnect-direction", Name: "friend", IsTrusted: func(string) bool { return !deleted.Load() }, IsUnfriended: func(string) bool { return deleted.Load() }, AutoReaccept: func(string, []string, string) bool { reaccepted.Add(1); deleted.Store(false); return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := da.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ai := peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}
	if len(da.Peers()) != 0 {
		t.Fatal("测试起点不应已有成员")
	}
	if _, err := da.Reconnect(ctx, ai); err != nil {
		t.Fatalf("好友预热失败: %v", err)
	}
	members := da.Peers()
	if len(members) != 1 || members[0].PeerID != hb.ID().String() || members[0].Name != "friend" {
		t.Fatalf("预热未恢复成员及身份: %+v", members)
	}
	// 模拟对端之后删除本机；同一好友的再次后台重连必须保留墓碑。
	deleted.Store(true)
	// Keep transport alive to isolate the marker contract from close races;
	// the persistent tombstone alone must reject this background handshake.
	if _, err := da.Reconnect(ctx, ai); !errors.Is(err, ErrUnfriended) {
		t.Fatalf("后台重连应收到删除拒绝，实际: %v", err)
	}
	if !deleted.Load() || reaccepted.Load() != 0 {
		t.Fatalf("后台重连误带重新申请: deleted=%v reaccepted=%d", deleted.Load(), reaccepted.Load())
	}
}

func TestSchedulerConnectRequestDoesNotLeakToBackground(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	modes := make(chan bool, 3)
	s := newDiscoveryScheduler(ctx, 0, func(ctx context.Context, _ peer.ID) error {
		request, _ := ctx.Value(connectRequestKey{}).(bool)
		modes <- request
		return nil
	})
	defer s.close()
	id := peer.ID("request-mode")
	if err := s.request(context.WithValue(ctx, connectRequestKey{}, true), id); err != nil {
		t.Fatal(err)
	}
	call, err := s.submit(id, false)
	if err != nil || call == nil {
		t.Fatalf("后台任务未提交: call=%v err=%v", call, err)
	}
	select {
	case <-call.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if first, second := <-modes, <-modes; !first || second {
		t.Fatalf("显式/后台标记错误: %v, %v", first, second)
	}
}

// Done is evaluated after request has submitted/joined the running call.
// This barrier makes the background-first overlap deterministic without sleeps.
type requestJoinedContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *requestJoinedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestSchedulerConnectRequestSurvivesRunningBackground(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	modes := make(chan bool, 3)
	var runs atomic.Int32
	s := newDiscoveryScheduler(ctx, 0, func(ctx context.Context, _ peer.ID) error {
		request, _ := ctx.Value(connectRequestKey{}).(bool)
		modes <- request
		if runs.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	defer s.close()
	id := peer.ID("background-first")
	if _, err := s.submit(id, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	done := make(chan error, 1)
	requestCtx := &requestJoinedContext{Context: context.WithValue(ctx, connectRequestKey{}, true), joined: make(chan struct{})}
	go func() { done <- s.request(requestCtx, id) }()
	select {
	case <-requestCtx.joined:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// 后台仍被阻塞时，显式请求不得提前把后台任务当成申请成功。
	select {
	case err := <-done:
		t.Fatalf("后台未结束显式请求就返回: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if runs.Load() != 2 {
		t.Fatalf("显式申请被后台单飞吞掉: runs=%d", runs.Load())
	}
	if first, second := <-modes, <-modes; first || !second {
		t.Fatalf("后台/显式标记错误: %v, %v", first, second)
	}
}
