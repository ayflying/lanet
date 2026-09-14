package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// makeSignedExe 造一份「程序 + 与之匹配的签名清单」，返回 exe 路径、清单
// 与验签公钥。私钥只在 CI Secrets 里，测试自造密钥对。
func makeSignedExe(t *testing.T, dir, exeName, version string) (string, Manifest, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bin := make([]byte, 8192)
	if _, err = rand.Read(bin); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, exeName)
	if err = os.WriteFile(exe, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	m := Manifest{
		Version:  version,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Size:     int64(len(bin)),
		SHA256:   hex.EncodeToString(sum[:]),
	}
	if err = SignManifest(priv, &m); err != nil {
		t.Fatal(err)
	}
	return exe, m, base64.StdEncoding.EncodeToString(pub)
}

func manifestJSON(t *testing.T, m Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestLoadSelfManifestFallsBackToDistManifest 发行包自带的 manifest.json
// 必须被认作有效分发凭证：CI 把签名分片打进压缩包（release.yml 的
// 「解压即有分发凭证」），文件名与运行期落盘的 update-manifest.json 不同。
// 历史上只认后者，于是「解压即种子」的设计意图完全落空——首装解压出来的
// 节点永远不是分发源，P2P 升级链在最需要种子的时候断掉。
func TestLoadSelfManifestFallsBackToDistManifest(t *testing.T) {
	dir := t.TempDir()
	exe, m, pubB64 := makeSignedExe(t, dir, "lanet.exe", "0.5.43")

	// 只放包内名，不放运行期名（模拟「解压 zip 后直接运行」）。
	if err := saveManifest(filepath.Join(dir, DistManifestName), m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{cfg: Config{
		ExePath:      exe,
		ManifestPath: filepath.Join(dir, "update-manifest.json"),
		PublicKey:    pubB64,
	}}
	got := c.loadSelfManifest()
	if got == nil {
		t.Fatal("包内 manifest.json 未被认作分发凭证（解压即种子的意图落空）")
	}
	if got.Version != "0.5.43" {
		t.Fatalf("凭证版本错误: %s", got.Version)
	}
}

// TestLoadSelfManifestRejectsStaleCredential 凭证必须与本地程序逐字节对应：
// 程序升级过而凭证停留在旧版本时不得对外分发（否则会把旧二进制当成新版本
// 传播给全网）。这正是本机踩过的坑——update-manifest.json 停在 0.5.8、
// 程序已 0.5.42，节点因此静默失去分发能力。
func TestLoadSelfManifestRejectsStaleCredential(t *testing.T) {
	dir := t.TempDir()
	exe, oldManifest, pubB64 := makeSignedExe(t, dir, "lanet.exe", "0.5.8")
	// 程序换成了新版本（内容变了，清单没跟上）。
	if err := os.WriteFile(exe, []byte("brand-new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(filepath.Join(dir, "update-manifest.json"), oldManifest); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{cfg: Config{
		ExePath:      exe,
		ManifestPath: filepath.Join(dir, "update-manifest.json"),
		PublicKey:    pubB64,
	}}
	if got := c.loadSelfManifest(); got != nil {
		t.Fatalf("陈旧的凭证被当作有效分发凭证（会把旧二进制分发给全网）: %+v", got)
	}
}

// TestSelfManifestPrefersRuntimeCredential 两个候选文件都存在时以运行期
// 凭证（update-manifest.json）为准——它是最近一次更新的产物。
func TestSelfManifestPrefersRuntimeCredential(t *testing.T) {
	dir := t.TempDir()
	exe, m, pubB64 := makeSignedExe(t, dir, "lanet.exe", "0.5.43")
	older := m
	older.Version = "0.5.42"
	// 旧凭证也保留在同一目录（模拟升级后未清理的包内分片）。
	if err := saveManifest(filepath.Join(dir, DistManifestName), older); err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(filepath.Join(dir, "update-manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{cfg: Config{
		ExePath:      exe,
		ManifestPath: filepath.Join(dir, "update-manifest.json"),
		PublicKey:    pubB64,
	}}
	got := c.loadSelfManifest()
	if got == nil || got.Version != "0.5.43" {
		t.Fatalf("未优先采用运行期凭证: %+v", got)
	}
}

// TestInstallDistManifest 正向落盘 + 三类拒绝：平台不符、验签失败、
// sha256 与程序不一致。任何一项不符都必须拒绝落盘——宁可不做种子，
// 也不传播假凭证。
func TestInstallDistManifest(t *testing.T) {
	dir := t.TempDir()
	exe, m, pubB64 := makeSignedExe(t, dir, "lanet-new.exe", "0.5.43")
	target := filepath.Join(dir, "update-manifest.json")

	// 正向：合法凭证落盘，且可被 loadSelfManifest 复读。
	if err := InstallDistManifestWith(manifestJSON(t, m), target, exe, pubB64); err != nil {
		t.Fatalf("合法凭证落盘失败: %v", err)
	}
	c := &Coordinator{cfg: Config{
		ExePath:      exe,
		ManifestPath: target,
		PublicKey:    pubB64,
	}}
	if got := c.loadSelfManifest(); got == nil || got.Version != "0.5.43" {
		t.Fatalf("落盘后未能复读为有效凭证: %+v", got)
	}

	other := filepath.Join(dir, "other.json")
	// 平台不符：别的平台的分片不得当成本机凭证。
	wrongPlatform := m
	wrongPlatform.Platform = "plan9/mips"
	if err := InstallDistManifestWith(manifestJSON(t, wrongPlatform), other, exe, pubB64); err == nil {
		t.Fatal("平台不符的清单被接受了")
	}
	// 验签失败：换一把公钥即视为伪造。
	_, _, otherPub := makeSignedExe(t, dir, "unused.bin", "9.9.9")
	if err := InstallDistManifestWith(manifestJSON(t, m), other, exe, otherPub); err == nil {
		t.Fatal("验签失败的清单被接受了")
	}
	// sha256 与程序不一致：换一份「对别的程序签名」的合法凭证，验签能过、
	// 但指纹对不上本地程序，必须拒绝（否则会把旧二进制当真凭证分发）。
	_, otherManifest, otherPub2 := makeSignedExe(t, dir, "another.exe", "0.5.44")
	if err := InstallDistManifestWith(manifestJSON(t, otherManifest), other, exe, otherPub2); err == nil {
		t.Fatal("sha256 与程序不符的清单被接受了")
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("被拒绝的凭证不应落盘")
	}
}

// TestInitialDelayDefaultsSane 首轮巡检延迟必须有默认值且远小于巡检周期：
// 否则「发现新版本立即升级」最早也只能等到一个完整周期之后。
func TestInitialDelayDefaultsSane(t *testing.T) {
	cfg := Config{}
	cfg.fillDefaults()
	if cfg.InitialDelay <= 0 {
		t.Fatal("InitialDelay 无默认值，启动后首轮巡检要等满一个周期")
	}
	if cfg.InitialDelay >= cfg.CheckInterval {
		t.Fatalf("InitialDelay(%s) 不应大于等于 CheckInterval(%s)", cfg.InitialDelay, cfg.CheckInterval)
	}
	if cfg.CheckInterval > 10*time.Minute {
		t.Fatalf("巡检周期过长（%s），发现新版本会明显延迟", cfg.CheckInterval)
	}
}
