package selfupdate

// 更新协议可达性与分发限流测试。
//
// 语义沿革：
//   - 0.5.34~0.5.48：出向默认按群密钥派生协议 ID + 固定 ID 挂「同群好友」成员门，
//     异群节点与公网扫描器拉不走任何字节。
//   - 0.5.49 起：用户要求「P2P 更新不一定需要在自己的网络密钥下面，是整个 DHT
//     网络，发现新版本就更新」，因此固定 ID 对全网 lanet 节点开放，防滥用手段
//     从「身份门（是否好友）」换成「流量门（限流）」。
//
// 本文件两侧都守：异群节点能拿到清单（全域互传），但拿不到无限次（限流）。

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

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// makeProvider 构造一个持有有效分发 head 的协调器（签名 + exe sha256 一致）。
// perPeer 为 0 时使用默认限流间隔。
func makeProvider(t *testing.T, dir string, gk []byte, perPeer time.Duration) (*Coordinator, string, string) {
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
		GroupKey: gk, PerPeerMinInterval: perPeer,
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

// fixedIDManifest 走全域固定 ID 通道征询一次清单（模拟老版本客户端 / 异群节点）。
// 被限流 Reset 或对端静默关闭时返回 error。
func fixedIDManifest(ctx context.Context, h host.Host, target peer.ID) (Manifest, error) {
	s, err := h.NewStream(ctx, target, ProtocolManifest)
	if err != nil {
		return Manifest{}, err
	}
	defer s.Close()
	if _, err = s.Write([]byte(`{"current":"0.1.0"}`)); err != nil {
		_ = s.Reset()
		return Manifest{}, err
	}
	if err = s.CloseWrite(); err != nil {
		return Manifest{}, err
	}
	s.SetDeadline(time.Now().Add(4 * time.Second))
	var m Manifest
	if err = json.NewDecoder(s).Decode(&m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// TestUpdateProtoDerivedSameGroup 同群（相同群密钥）两节点：走派生协议 ID
// 的 manifest 征询 + 文件下载全流程正常（同群快路径，少一跳）。
func TestUpdateProtoDerivedSameGroup(t *testing.T) {
	dir := t.TempDir()
	gk := []byte("0123456789abcdef0123456789abcdef")
	cProvider, shaHex, pubB64 := makeProvider(t, dir, gk, 0)
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

// TestUpdateFixedIDCrossGroupAllowed 异群节点（不同网络密钥、素未加好友）：
// 派生 ID 仍协商不上（同群快路径保持私有），但**全域固定 ID 通道必须放行**——
// 这正是用户要的「整个 DHT 网络里发现新版本就互传」。它可以拿到清单，
// 拿不到清单才叫升级链断裂（升级后跨网络密钥的节点再也拉不到包）。
func TestUpdateFixedIDCrossGroupAllowed(t *testing.T) {
	dir := t.TempDir()
	gkA := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	gkB := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	cProvider, shaHex, pubB64 := makeProvider(t, dir, gkA, 0)
	cStranger := newRequester(t, dir, gkB, pubB64)

	if cStranger.protoManifest == cProvider.protoManifest {
		t.Fatal("异群派生 ID 不应相同")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cStranger.host.Connect(ctx, peer.AddrInfo{ID: cProvider.host.ID(), Addrs: cProvider.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	// 派生 ID 通道：异群直接协商不上（同群隐私快路径保持不变）。
	if s, err := cStranger.host.NewStream(ctx, cProvider.host.ID(), cStranger.protoManifest); err == nil {
		_ = s.Reset()
		t.Fatal("异群直接协商派生协议 ID 不应成功")
	}
	if cStranger.protoManifestAlt != ProtocolManifest {
		t.Fatalf("出向应保留全域固定 ID 兜底: %s", cStranger.protoManifestAlt)
	}
	// 全域固定 ID 通道：必须能拿到清单（只此一次请求——限流下同一对端的
	// manifest 名额一轮只有一个）。
	m, err := fixedIDManifest(ctx, cStranger.host, cProvider.host.ID())
	if err != nil {
		t.Fatalf("全域固定 ID 通道应放行异群节点: %v", err)
	}
	if m.SHA256 != shaHex || m.Version != "9.9.9" {
		t.Fatalf("异群节点拿到的清单不符: %+v", m)
	}
}

// TestUpdateFixedIDRateLimited 全域通道的防滥用：同一对端在 PerPeerMinInterval
// 内只能征询一次（第一次放行、紧接着第二次被重置），超过间隔后恢复放行
// ——限流是节流，不是封禁。
func TestUpdateFixedIDRateLimited(t *testing.T) {
	dir := t.TempDir()
	gk := []byte("cccccccccccccccccccccccccccccccc")
	cProvider, shaHex, pubB64 := makeProvider(t, dir, gk, 2*time.Second)
	cReq := newRequester(t, dir, []byte("dddddddddddddddddddddddddddddddd"), pubB64)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cReq.host.Connect(ctx, peer.AddrInfo{ID: cProvider.host.ID(), Addrs: cProvider.host.Addrs()}); err != nil {
		t.Fatal(err)
	}
	// 第一次：放行。
	first, err := fixedIDManifest(ctx, cReq.host, cProvider.host.ID())
	if err != nil {
		t.Fatalf("首次征询应放行: %v", err)
	}
	if first.SHA256 != shaHex {
		t.Fatalf("首次清单不符: %+v", first)
	}
	// 第二次（同一 2s 窗口内）：必须被限流。
	if m, err := fixedIDManifest(ctx, cReq.host, cProvider.host.ID()); err == nil && m.SHA256 != "" {
		t.Fatal("同一对端在最小间隔内不应再次拿到清单（限流未生效）")
	}
	// 越过窗口：恢复放行。
	time.Sleep(2200 * time.Millisecond)
	again, err := fixedIDManifest(ctx, cReq.host, cProvider.host.ID())
	if err != nil {
		t.Fatalf("超过最小间隔后应恢复放行: %v", err)
	}
	if again.SHA256 != shaHex {
		t.Fatalf("恢复后的清单不符: %+v", again)
	}
}

// TestRateLimiterPerPeerAndInflight 限流器单元语义：同桶同对端最小间隔、
// 全局在途上限、释放归零、窗口过期恢复，以及 manifest/file 分桶互不影响。
func TestRateLimiterPerPeerAndInflight(t *testing.T) {
	r := newRateLimiter(50*time.Millisecond, 200*time.Millisecond, 3)
	if !r.allow(bucketManifest, "a") {
		t.Fatal("首次应放行")
	}
	if r.allow(bucketManifest, "a") {
		t.Fatal("同对端最小间隔内不应再放行")
	}
	// 分桶：manifest 与 file 各自独立计数，一次升级「征询 + 下载」才能都放行。
	if !r.allow(bucketFile, "a") {
		t.Fatal("file 桶应与 manifest 桶分开计数（否则升级链被自己掐死）")
	}
	if !r.allow(bucketManifest, "b") {
		t.Fatal("不同对端应放行")
	}
	if r.allow(bucketManifest, "c") {
		t.Fatal("在途达上限时不应放行")
	}
	r.release() // b 完成
	if !r.allow(bucketManifest, "c") {
		t.Fatal("释放出名额后应放行")
	}
	r.release()
	r.release()
	r.release()
	if r.inflight != 0 {
		t.Fatalf("in-flight 计数应归零，实际 %d", r.inflight)
	}
	time.Sleep(60 * time.Millisecond)
	if !r.allow(bucketManifest, "a") {
		t.Fatal("超过最小间隔后应恢复放行")
	}
	r.release()
	// file 桶冷却更长：50ms 后 manifest 恢复，200ms 内 file 仍应被挡。
	if r.allow(bucketFile, "a") {
		t.Fatal("file 桶冷却未生效")
	}
	time.Sleep(160 * time.Millisecond)
	if !r.allow(bucketFile, "a") {
		t.Fatal("超过 file 冷却后应恢复放行")
	}
	r.release()
}

// TestRateLimiterDefaults 参数归一化：0 值走默认，避免误配成「不设限」。
func TestRateLimiterDefaults(t *testing.T) {
	r := newRateLimiter(0, 0, 0)
	if r.per != 30*time.Second || r.filePer != 5*time.Minute || r.maxIn != 4 {
		t.Fatalf("默认参数应为 30s/5min/4，实际 %v/%v/%d", r.per, r.filePer, r.maxIn)
	}
}
