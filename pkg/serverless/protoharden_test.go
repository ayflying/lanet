package serverless

// 私有协议加固（0.5.33）测试：控制面协议 ID 与私有 DHT 前缀按群密钥派生。
//
// 验证三层语义：
//  1. 同群新对等互通（派生 ID 一致，握手正常）；
//  2. 异群节点即便直连端口也协商不上协议（噪音在传输层归零）；
//  3. 新老版本混跑过渡：新→老出向有固定 ID 兜底；老→新靠新节点出向收敛。

import (
	"context"
	"strings"
	"testing"
	"time"

	lproto "github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// TestDerivedProtoSameGroup 同群两节点（默认派生协议）应正常完成 info 握手。
func TestDerivedProtoSameGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha, hb := testHost(t, false), testHost(t, false)
	da, err := New(ctx, ha, Config{NetworkKey: "grp-derived", Name: "node-a"})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	db, err := New(ctx, hb, Config{NetworkKey: "grp-derived", Name: "node-b"})
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err = db.Start(ctx); err != nil {
		t.Fatalf("start B: %v", err)
	}
	// 派生 ID 必须形如 /lanet/<指纹>/<base>/1.0.0 且不等于历史固定 ID。
	if string(da.protoInfo) == ProtocolInfo || !strings.HasPrefix(string(da.protoInfo), "/lanet/") {
		t.Fatalf("protoInfo 未按群派生: %s", da.protoInfo)
	}
	if da.protoInfo != db.protoInfo || da.protoUnfriend != db.protoUnfriend {
		t.Fatalf("同群派生 ID 不一致: %s/%s vs %s/%s", da.protoInfo, da.protoUnfriend, db.protoInfo, db.protoUnfriend)
	}
	if da.dhtPrivate == nil {
		t.Fatal("私有 DHT 未初始化")
	}
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("fetchInfo: %v", err)
	}
	if info.Name != "node-b" {
		t.Fatalf("info.Name = %q, want node-b", info.Name)
	}
}

// TestDerivedProtoCrossGroupRejected 异群节点直连端口后：
// 拨派生协议必须协商失败（对端没注册这个 ID），握手根本进不了 handler。
func TestDerivedProtoCrossGroupRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha, hb := testHost(t, false), testHost(t, false)
	da, err := New(ctx, ha, Config{NetworkKey: "grp-x-a", Name: "node-a"})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if _, err = New(ctx, hb, Config{NetworkKey: "grp-x-b", Name: "node-b"}); err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	// B 注册自己的派生 handler（模拟异群老客户端探测本机端口）。
	if err = hb.Connect(ctx, peer.AddrInfo{ID: ha.ID(), Addrs: ha.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	dprotoB := lproto.GroupProtoID(lproto.BaseInfo, GroupKey(ChannelOfficial, "grp-x-b"))
	s, err := hb.NewStream(ctx, ha.ID(), dprotoB)
	if err == nil {
		_ = s.Reset()
		t.Fatalf("异群派生协议 ID 不应能建立流（协商应失败）")
	}
	if !strings.Contains(err.Error(), "protocol not supported") &&
		!strings.Contains(err.Error(), "no protocols available") {
		t.Logf("协商失败（错误形态可接受）: %v", err)
	}
}

// TestLegacyOutboundFallbackNewToOld 「新→老」过渡：B 模拟未升级老版本
// （只注册固定 ProtocolInfo handler），A（派生模式）出向 fetchInfo 应经
// 候选列表里的固定 ID 兜底成功——已保存的好友升级节奏不一致也不断连。
func TestLegacyOutboundFallbackNewToOld(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ha, hb := testHost(t, false), testHost(t, false)
	da, err := New(ctx, ha, Config{NetworkKey: "grp-mix", Name: "node-a"})
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err = da.Start(ctx); err != nil {
		t.Fatalf("start A: %v", err)
	}
	// B：不启动派生 Discovery，仅注册历史固定协议（完全等同 0.5.32 老节点）。
	fakeOld := &Discovery{host: hb, cfg: Config{NetworkKey: "grp-mix", Name: "node-b"}, groupKey: GroupKey("", "grp-mix"), members: make(map[string]*Member)}
	hb.SetStreamHandler(ProtocolInfo, fakeOld.handleInfo)
	if err = ha.Connect(ctx, peer.AddrInfo{ID: hb.ID(), Addrs: hb.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, err := da.fetchInfo(ctx, hb.ID())
	if err != nil {
		t.Fatalf("新→老出向兜底应成功: %v", err)
	}
	if info.Name != "node-b" {
		t.Fatalf("info.Name = %q, want node-b", info.Name)
	}
}

// TestLegacyFlagUsesFixedIDs 逃生开关：LegacyProtocols=true 时注册与出向
// 都退回历史固定 ID（与全网老版本完全一致）。
func TestLegacyFlagUsesFixedIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h := testHost(t, false)
	d, err := New(ctx, h, Config{NetworkKey: "grp-legacy", Name: "n", LegacyProtocols: true})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if d.protoInfo != libprotocol.ID(ProtocolInfo) || d.protoUnfriend != libprotocol.ID(ProtocolUnfriend) {
		t.Fatalf("LegacyProtocols 应使用固定 ID，实际 %s/%s", d.protoInfo, d.protoUnfriend)
	}
	if d.protoInfoAlt != "" || d.protoUnfriendAlt != "" {
		t.Fatalf("固定模式下不应再有兜底 ID: %s/%s", d.protoInfoAlt, d.protoUnfriendAlt)
	}
}
