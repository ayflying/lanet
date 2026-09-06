package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestSignVerify 签名/验签往返：正确签名通过，篡改任一字段验签失败。
func TestSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	m := Manifest{Version: "0.5.0", Platform: "windows/amd64", Size: 123, SHA256: "abc123"}
	if err = SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}
	if !m.Verify(pubB64) {
		t.Fatal("正确签名验签失败")
	}
	// 篡改各字段逐一验失败。
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Version = "0.5.1" },
		func(m *Manifest) { m.Platform = "linux/amd64" },
		func(m *Manifest) { m.SHA256 = "deadbeef" },
		func(m *Manifest) { m.Size = 456 },
	} {
		bad := m
		mutate(&bad)
		if bad.Verify(pubB64) {
			t.Fatalf("篡改后验签竟通过: %+v", bad)
		}
	}
	// 错误公钥。
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrong := m
	_ = SignManifest(otherPriv, &wrong)
	if wrong.Verify(pubB64) {
		t.Fatal("他钥签名验签竟通过")
	}
}

// TestCompareVersions 版本比较。
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.2", "0.4.3", -1},
		{"0.10.0", "0.9.9", 1},     // 数字比较而非字典序
		{"1.0.0", "1.0.0", 0},      // 相等
		{"dev", "0.1.0", -1},       // 非法段按 0
		{"0.4", "0.4.0", 0},        // 缺段按 0
		{"", "0.0.1", -1},          // 空版本
		{"0.4.3-beta", "0.4.3", 0}, // 后缀忽略
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestConsensus 共识决策：验签通过 + 票数达标才可信（need=1 单票制）。
func TestConsensus(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	good := Manifest{Version: "0.5.0", Platform: "windows/amd64", Size: 1, SHA256: "aa"}
	if err := SignManifest(priv, &good); err != nil {
		t.Fatal(err)
	}
	// need=1：单份验签通过即可信（签名信任锚）。
	if m, ok := consensus([]Manifest{good}, pubB64, 1); !ok || m.SHA256 != "aa" {
		t.Fatalf("单份验签通过未通过 need=1: %v %v", m, ok)
	}
	// 3 份一致 → 通过（兼容多票制）。
	if m, ok := consensus([]Manifest{good, good, good}, pubB64, 3); !ok || m.SHA256 != "aa" {
		t.Fatalf("3 份一致未通过: %v %v", m, ok)
	}
	// 2 份 → 票数不足（多票制下）。
	if _, ok := consensus([]Manifest{good, good}, pubB64, 3); ok {
		t.Fatal("2 份不应通过 need=3 的共识")
	}
	// 单票制下同版本 sha256 冲突 → 拒绝（分发被污染，宁可不动）。
	conflict := good
	conflict.SHA256 = "bb"
	if err := SignManifest(priv, &conflict); err != nil {
		t.Fatal(err)
	}
	if _, ok := consensus([]Manifest{good, conflict}, pubB64, 1); ok {
		t.Fatal("同版本 sha256 冲突不应通过 need=1")
	}
	// 单票制下取验签通过的最高版本。
	better := Manifest{Version: "0.6.0", Platform: "windows/amd64", Size: 1, SHA256: "cc"}
	if err := SignManifest(priv, &better); err != nil {
		t.Fatal(err)
	}
	if m, ok := consensus([]Manifest{good, better}, pubB64, 1); !ok || m.Version != "0.6.0" {
		t.Fatalf("应取最高版本 0.6.0: %v %v", m, ok)
	}
	// 混入假签名 → 拒绝。
	fake := good
	fake.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
	if _, ok := consensus([]Manifest{good, good, fake}, pubB64, 3); ok {
		t.Fatal("含假签名的清单不应通过共识")
	}
	// sha256 分裂 → 拒绝。
	other := Manifest{Version: "0.5.0", Platform: "windows/amd64", Size: 1, SHA256: "bb"}
	_ = SignManifest(priv, &other)
	if _, ok := consensus([]Manifest{good, good, other}, pubB64, 3); ok {
		t.Fatal("sha256 分裂不应通过共识")
	}
}

// ---- 双 host 真实传输测试 ----

type staticPeers struct{ peers []PeerInfo }

func (s staticPeers) Peers() []PeerInfo { return s.peers }

func newTestHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestP2PFileTransfer 提供方（持有效 head）与请求方真实走一遍
// manifest 征询 + 文件下载 + sha256 校验的全流程。
func TestP2PFileTransfer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	// 模拟"新版本二进制"。
	bin := make([]byte, 256*1024)
	if _, err := rand.Read(bin); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	shaHex := hex.EncodeToString(sum[:])

	dir := t.TempDir()
	srcExe := filepath.Join(dir, "lanet-new.exe")
	if err := os.WriteFile(srcExe, bin, 0o755); err != nil {
		t.Fatal(err)
	}

	// 提供方：持有 head（签名 + 本地 exe 一致）。
	m := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: int64(len(bin)), SHA256: shaHex}
	if err := SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "update-manifest.json")
	if err := saveManifest(manifestPath, m); err != nil {
		t.Fatal(err)
	}

	hProvider := newTestHost(t)
	cfgP := Config{
		CurrentVersion: "9.9.9",
		Platform:       "windows/amd64",
		ExePath:        srcExe,
		ManifestPath:   manifestPath,
		PublicKey:      pubB64,
		CheckInterval:  time.Hour, // 不触发自动巡检
		Quiet:          true,
	}
	cProvider := New(hProvider, staticPeers{}, cfgP, nil)
	if _, ok := cProvider.SelfManifest(); !ok {
		t.Fatal("提供方 head 加载失败（签名或 sha256 校验未过）")
	}

	// 请求方。
	hRequester := newTestHost(t)
	cfgR := Config{
		CurrentVersion: "0.1.0",
		Platform:       "windows/amd64",
		ExePath:        filepath.Join(dir, "lanet-old.exe"),
		PublicKey:      pubB64,
		Quiet:          true,
	}
	cRequester := New(hRequester, staticPeers{}, cfgR, nil)

	// 建连。
	if err := hRequester.Connect(context.Background(), peer.AddrInfo{ID: hProvider.ID(), Addrs: hProvider.Addrs()}); err != nil {
		t.Fatal(err)
	}

	// 1) 征询清单。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := cRequester.requestManifest(ctx, hProvider.ID().String())
	if err != nil {
		t.Fatalf("征询清单失败: %v", err)
	}
	if got.SHA256 != shaHex || got.Version != "9.9.9" {
		t.Fatalf("清单内容不符: %+v", got)
	}

	// 2) 下载文件。
	dest := filepath.Join(dir, "downloaded.tmp")
	if err = cRequester.requestFile(ctx, hProvider.ID().String(), got, dest); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	gotBin, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(gotBin)
	if hex.EncodeToString(gotSum[:]) != shaHex {
		t.Fatal("下载文件 sha256 与源不符")
	}
	if len(gotBin) != len(bin) {
		t.Fatalf("文件大小不符: %d != %d", len(gotBin), len(bin))
	}

	// 3) 拒绝假请求：下载方请求一个不存在的 sha256，提供方应拒绝（超时/空响应）。
	badCtx, badCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer badCancel()
	bad := Manifest{Version: "9.9.9", Platform: "windows/amd64", Size: int64(len(bin)), SHA256: "f000"}
	err = cRequester.requestFile(badCtx, hProvider.ID().String(), bad, filepath.Join(dir, "should-not-exist.tmp"))
	if err == nil {
		t.Fatal("伪造 sha256 的下载请求不应成功")
	}
}
