package selfupdate

// 私有协议加固（0.5.33）测试：更新协议 ID 按群派生 + 固定 ID 成员门。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// makeProvider 构造一个持有有效分发 head 的协调器（签名 + exe sha256 一致）。
func makeProvider(t *testing.T, dir string, gk []byte, isMember func(string) bool) (*Coordinator, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	bin := make([]byte, 64*1024)
	if _, err = rand.Read(bin); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	shaHex := hex.EncodeToString(sum[:])
	srcExe := filepath.Join(dir, "lanet-new.exe")
	if err = os.WriteFile(srcExe, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: int64(len(bin)), SHA256: shaHex}
	if err = SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "update-manifest.json")
	if err = saveManifest(mp, m); err != nil {
		t.Fatal(err)
	}
	hp := newTestHost(t)
	c := New(hp, staticPeers{}, Config{
		CurrentVersion: "9.9.9", Platform: "windows/amd64", ExePath: srcExe,
		ManifestPath: mp, PublicKey: pubB64, CheckInterval: time.Hour, Quiet: true,
		GroupKey: gk, IsMember: isMember,
	}, nil)
	if _, ok := c.SelfManifest(); !ok {
		t.Fatal("提供方 head 加载失败")
	}
	return c, shaHex, pubB64
}

// newRequester 构造升级请求方（默认派生模式）。
func newRequester(t *testing.T, dir string, gk []byte, pubB64 string) *Coordinator {
	t.Helper()
	h := newTestHost(t)
	return New(h, staticPeers{}, Config{
		CurrentVersion: "0.1.0", Platform: "windows/amd64",
		ExePath:   filepath.Join(dir, "lanet-old.exe"),
		PublicKey: pubB64, Quiet: true, GroupKey: gk,
	}, nil)
}

// TestUpdateProtoDerivedSameGroup 同群（相同群密钥）两节点：派生协议 ID
// 一致，manifest 征询 + 文件下载全流程正常。
func TestUpdateProtoDerivedSameGroup(t *testing.T) {
	dir := t.TempDir()
	gk := []byte("0123456789abcdef0123456789abcdef")
	cProvider, shaHex, pubB64 := makeProvider(t, dir, gk, nil)
	cReq := newRequester(t, dir, gk, pubB64)

	if cReq.protoManifest == ProtocolManifest || !strings.HasPrefix(string(cReq.protoManifest), "/lanet/") {
		t.Fatalf("出向协议 ID 未按群派生: %s", cReq.protoManifest)
	}
	if cReq.protoManifest != cProvider.protoManifest {
		t.Fatal("同群派生 ID 不一致")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cReq.host.Connect(ctx, peer.AddrInfo{ID: cProvider.host.ID(), Addrs: cProvider.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	got, err := cReq.requestManifest(ctx, cProvider.host.ID().String())
	if err != nil {
		t.Fatalf("派生协议征询失败: %v", err)
	}
	if got.SHA256 != shaHex {
		t.Fatalf("清单不符: %+v", got)
	}
	dest := filepath.Join(dir, "dl.tmp")
	if err = cReq.requestFile(ctx, cProvider.host.ID().String(), got, dest); err != nil {
		t.Fatalf("派生协议下载失败: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if s2 := sha256.Sum256(data); hex.EncodeToString(s2[:]) != shaHex {
		t.Fatal("下载内容 sha256 不符")
	}
}

// TestUpdateProtoCrossGroupRejected 异群节点（不同群密钥）：派生 ID 不同，
// 直接拨更新协议必须协商失败——公网扫描器与陌生网络拉不走任何字节。
// 同时验证派生模式下的固定 ID 兼容入口有成员门：异群对端 IsMember
// 判定不通过（对方不是本机好友），固定通道同样被重置。
func TestUpdateProtoCrossGroupRejected(t *testing.T) {
	dir := t.TempDir()
	gkA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	gkB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	// provider 的信任名单里没有任何人。
	cProvider, _, pubB64 := makeProvider(t, dir, gkA, func(string) bool { return false })
	cStranger := newRequester(t, dir, gkB, pubB64)

	if cStranger.protoManifest == cProvider.protoManifest {
		t.Fatal("异群派生 ID 不应相同")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cStranger.host.Connect(ctx, peer.AddrInfo{ID: cProvider.host.ID(), Addrs: cProvider.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	// 派生 ID 通道：协商不上。
	if _, err := cStranger.requestManifest(ctx, cProvider.host.ID().String()); err == nil {
		t.Fatal("异群派生协议征询不应成功")
	}
	// 固定 ID 兼容通道：协商得上，但成员门必须掐流（读不到清单）。
	s, err := cStranger.host.NewStream(ctx, cProvider.host.ID(), ProtocolManifest)
	if err != nil {
		t.Fatalf("固定 ID 协商失败（兼容入口应存在）: %v", err)
	}
	defer s.Close()
	if _, werr := s.Write([]byte(`{"current":"0.1.0"}`)); werr != nil {
		_ = s.Reset()
		return // 已被门禁 Reset：符合预期
	}
	_ = s.CloseWrite()
	s.SetDeadline(time.Now().Add(3 * time.Second))
	var m Manifest
	if derr := json.NewDecoder(s).Decode(&m); derr == nil && m.SHA256 != "" {
		t.Fatal("非成员不应从固定 ID 通道拿到清单")
	}
}

// TestUpdateLegacyGateAllowsFriend 混版本过渡的正向断言：固定 ID 兼容
// 通道对「已确认好友」放行——老版本好友能正常征询到清单并升级（升级链
// 不断）；非好友（见上例）则被挡。
func TestUpdateLegacyGateAllowsFriend(t *testing.T) {
	dir := t.TempDir()
	gk := []byte("cccccccccccccccccccccccccccccccc")
	hr := newTestHost(t)
	cProvider, shaHex, _ := makeProvider(t, dir, gk, func(id string) bool { return id == hr.ID().String() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hr.Connect(ctx, peer.AddrInfo{ID: cProvider.host.ID(), Addrs: cProvider.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	// hr 在门名单里：走固定 ID 通道（模拟老版本客户端）应拿到清单。
	s, err := hr.NewStream(ctx, cProvider.host.ID(), ProtocolManifest)
	if err != nil {
		t.Fatalf("固定 ID 协商失败: %v", err)
	}
	defer s.Close()
	if _, werr := s.Write([]byte(`{"current":"0.1.0"}`)); werr != nil {
		t.Fatalf("写入请求失败: %v", werr)
	}
	if werr := s.CloseWrite(); werr != nil {
		t.Fatalf("半关闭失败: %v", werr)
	}
	s.SetDeadline(time.Now().Add(5 * time.Second))
	var m Manifest
	if derr := json.NewDecoder(s).Decode(&m); derr != nil {
		t.Fatalf("好友走固定 ID 通道应成功: %v", derr)
	}
	if m.SHA256 != shaHex || m.Version != "9.9.9" {
		t.Fatalf("好友拿到的清单不符: %+v", m)
	}
}
