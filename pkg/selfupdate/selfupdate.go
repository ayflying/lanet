// Package selfupdate 实现去中心化的 P2P 自动更新：
// 节点通过成员发现感知全网版本分布，当「更高版本 + 同平台」的成员达到
// 足够数量（默认 3 个）时，向随机成员征询版本清单（版本/大小/sha256/
// Ed25519 签名），三票一致且验签通过后随机选一个成员流式下载新程序，
// 校验后交由宿主完成自替换与重启。
//
// 信任模型（重要）：
//   - 「多数节点特征码一致」只能证明它们跑的是同一个文件，不能证明文件
//     可信——任何攻击者都可串通 3 个节点投毒假更新。
//   - 因此唯一信任锚是「发布签名」：CI 用私钥对每个平台的二进制签名
//     （签名随清单在节点间自由传播，公开数据伪造不了），本包内置发布
//     公钥逐一验签，验签不过一律拒绝。GitHub 只是发版与首种子来源，
//     之后全网自传播，不依赖任何单一渠道。
//   - 无签名版本（dev 构建 / 未配置签名密钥的 CI）不参与 P2P 分发。
//
// 容器环境禁用：容器内替换二进制会在重启时被镜像回滚，自更新只对
// 裸机二进制形态（pvn-node 单程序）有意义，由调用方检测并禁用。
package selfupdate

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// 节点间更新协议：manifest = 版本清单征询；file = 文件分发。
const (
	ProtocolManifest protocol.ID = "/lanet/update-manifest/1.0.0"
	ProtocolFile     protocol.ID = "/lanet/update-file/1.0.0"
)

// signPrefix 签名域分隔：签名内容 = prefix + version + ":" + platform + ":" + size + ":" + sha256hex。
// 所有字段（含 Size）都入签——测试曾发现仅漏签 Size 时可被篡改。
const signPrefix = "lanet-update-v1:"

// ReleasePublicKey 发布签名公钥（base64 Ed25519）。私钥只存在于
// GitHub Actions Secrets（SELFUPDATE_SIGNING_KEY），用于 CI 发版时签名；
// 换钥 = 全网失去旧版本分发能力，需随代码更新重新发版。
const ReleasePublicKey = "2e2tr45GeyFbu2dG6isVh4nwUF2sG58iM4oaAWDBtmI="

// Manifest 单平台更新条目：一个「已签名的新版本」的完整描述。
// 在节点间经 manifest 协议自由传播，接收方验签后才能作为下载依据。
type Manifest struct {
	Version   string `json:"version"`             // 三段式版本号
	Platform  string `json:"platform"`            // GOOS/GOARCH，如 windows/amd64
	Size      int64  `json:"size"`                // 二进制字节数
	SHA256    string `json:"sha256"`              // 二进制 sha256（小写 hex）
	Signature string `json:"signature,omitempty"` // Ed25519 签名（base64）
}

// SignMessage 生成待签名消息（CI 签名工具与运行时验签共用）。
func SignMessage(m Manifest) []byte {
	return []byte(fmt.Sprintf("%s%s:%s:%d:%s", signPrefix, m.Version, m.Platform, m.Size, m.SHA256))
}

// SignManifest 用私钥补全 m.Signature（CI 签名工具用）。
func SignManifest(priv ed25519.PrivateKey, m *Manifest) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("selfupdate: 私钥长度非法")
	}
	sig := ed25519.Sign(priv, SignMessage(*m))
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}

// Verify 用给定公钥验签（base64 公钥，通常传 ReleasePublicKey）。
func (m Manifest) Verify(pubB64 string) bool {
	if m.Signature == "" || m.SHA256 == "" || m.Version == "" || m.Platform == "" {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), SignMessage(m), sig)
}

// CompareVersions 三段式版本比较：-1/0/1。非法段按 0 处理。
func CompareVersions(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var out [3]int
	seg := 0
	num := 0
	got := false
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == '.' {
			if got && seg < 3 {
				out[seg] = num
			}
			seg++
			num, got = 0, false
			continue
		}
		if v[i] >= '0' && v[i] <= '9' {
			num = num*10 + int(v[i]-'0')
			got = true
		}
	}
	return out
}

// PeerInfo 决策所需的成员版本视图（由成员发现层提供）。
type PeerInfo struct {
	ID       string // libp2p peer.ID 字符串
	Version  string
	Platform string
}

// PeerSource 成员版本视图来源（serverless.Peers 的适配）。
type PeerSource interface {
	Peers() []PeerInfo
}

// Config 更新协调器配置。
type Config struct {
	// CurrentVersion 当前程序版本（ldflags 注入）。
	CurrentVersion string
	// Platform 本程序平台，默认 runtime.GOOS+"/"+runtime.GOARCH。
	Platform string
	// ExePath 本地主程序路径（既是分发源也是替换目标）。
	ExePath string
	// ManifestPath 本地版本清单文件（保存最近一次验证过的 head，
	// 重启后作为新版本的分发凭证）。默认 exe 同目录 update-manifest.json。
	ManifestPath string
	// PublicKey 发布公钥 base64，默认 ReleasePublicKey。
	PublicKey string
	// CheckInterval 版本巡检周期，默认 30 分钟。
	CheckInterval time.Duration
	// MinNewPeers 触发征询所需的更高版本同平台成员数，默认 3。
	// 同时是清单共识票数（3 份一致才可信）。
	MinNewPeers int
	// Quiet 关闭日志。
	Quiet bool
}

func (c *Config) fillDefaults() {
	if c.Platform == "" {
		c.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if c.PublicKey == "" {
		c.PublicKey = ReleasePublicKey
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 30 * time.Minute
	}
	if c.MinNewPeers <= 0 {
		c.MinNewPeers = 3
	}
	if c.ManifestPath == "" {
		c.ManifestPath = filepath.Join(filepath.Dir(c.ExePath), "update-manifest.json")
	}
}

// Coordinator P2P 更新协调器：既是分发源（持有有效 head 后对外提供
// manifest 与文件），也是升级决策者（巡检成员版本、征询、下载）。
type Coordinator struct {
	host host.Host
	src  PeerSource
	cfg  Config

	onUpdate func(path string, m Manifest) // 下载校验成功回调（宿主替换+重启）

	mu   sync.Mutex
	self *Manifest // 本地有效 head：验签通过且与本地 exe sha256 一致

	attempts map[string]bool // 已尝试过下载的 sha256（进程内防重）
}

// New 创建协调器并注册流协议 handler。启动巡检用 Start。
// selfHead 加载失败不视为错误（首装/无签名版本仅不参与分发）。
func New(h host.Host, src PeerSource, cfg Config, onUpdate func(path string, m Manifest)) *Coordinator {
	cfg.fillDefaults()
	c := &Coordinator{
		host:     h,
		src:      src,
		cfg:      cfg,
		onUpdate: onUpdate,
		attempts: make(map[string]bool),
	}
	c.self = c.loadSelfManifest()
	h.SetStreamHandler(ProtocolManifest, c.handleManifest)
	h.SetStreamHandler(ProtocolFile, c.handleFile)
	if c.self != nil {
		c.logf("分发源就绪：v%s %s sha256=%s…", c.self.Version, c.self.Platform, c.self.SHA256[:12])
	} else {
		c.logf("本地无有效分发凭证（首装或无签名版本），仅作升级请求方")
	}
	return c
}

// SelfManifest 当前生效的分发 head（测试与状态展示用）。
func (c *Coordinator) SelfManifest() (Manifest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self == nil {
		return Manifest{}, false
	}
	return *c.self, true
}

// Start 启动周期巡检，直到 ctx 取消。
func (c *Coordinator) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Coordinator) loop(ctx context.Context) {
	t := time.NewTicker(c.cfg.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.round(ctx)
		}
	}
}

// round 一轮巡检：统计更高版本 → 达票征询 → 共识 + 验签 → 下载。
func (c *Coordinator) round(ctx context.Context) {
	cur := c.cfg.CurrentVersion
	if cur == "" || cur == "dev" {
		return // dev 构建版本号不可比，不参与
	}
	peers := c.src.Peers()
	var newer []PeerInfo
	for _, p := range peers {
		if p.Platform != c.cfg.Platform || p.ID == "" {
			continue
		}
		if CompareVersions(cur, p.Version) < 0 { // 对端比本端新
			newer = append(newer, p)
		}
	}
	if len(newer) < c.cfg.MinNewPeers {
		return
	}
	// 随机抽样征询（灰度：要下载的版本必须已在至少 N 个成员上运行）。
	rand.Shuffle(len(newer), func(i, j int) { newer[i], newer[j] = newer[j], newer[i] })
	sample := newer
	if len(sample) > c.cfg.MinNewPeers {
		sample = sample[:c.cfg.MinNewPeers]
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var heads []Manifest
	for _, p := range sample {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if m, err := c.requestManifest(rctx, id); err == nil {
				mu.Lock()
				heads = append(heads, m)
				mu.Unlock()
			}
		}(p.ID)
	}
	wg.Wait()

	// 共识 + 逐份验签：全部一致才可信（少数派可能是被篡改或陈旧记录）。
	target, ok := consensus(heads, c.cfg.PublicKey, c.cfg.MinNewPeers)
	if !ok {
		c.logf("征询到 %d/%d 份清单但未形成可信共识，放弃本轮", len(heads), len(sample))
		return
	}
	if CompareVersions(cur, target.Version) >= 0 {
		return
	}
	c.mu.Lock()
	if c.attempts[target.SHA256] { // 本进程内同一版本只尝试一次
		c.mu.Unlock()
		return
	}
	c.attempts[target.SHA256] = true
	c.mu.Unlock()

	c.logf("发现新版本 v%s（sha256=%s…，%d 成员确认 + 验签通过），开始下载",
		target.Version, target.SHA256[:12], len(sample))
	path, err := c.download(ctx, target, sample)
	if err != nil {
		c.logf("P2P 更新下载失败（下轮巡检重试）: %v", err)
		return
	}
	// head 落盘：重启后本节点即可作为新版本分发源（启动时会重新校验
	// 与本地 exe 一致，替换失败则自动失效，不会传播假凭证）。
	if err = saveManifest(c.cfg.ManifestPath, target); err != nil {
		c.logf("更新清单落盘失败: %v", err)
	}
	if c.onUpdate != nil {
		c.onUpdate(path, target)
	}
}

// consensus 要求 heads 中验签通过的份数 >= need，且 sha256 完全一致。
// 一致才返回该 head（验签保证它出自发布私钥，一致保证多数在跑）。
func consensus(heads []Manifest, pubB64 string, need int) (Manifest, bool) {
	var valid []Manifest
	for _, m := range heads {
		if m.Verify(pubB64) {
			valid = append(valid, m)
		}
	}
	if len(valid) < need {
		return Manifest{}, false
	}
	for _, m := range valid[1:] {
		if m.SHA256 != valid[0].SHA256 || m.Version != valid[0].Version {
			return Manifest{}, false
		}
	}
	return valid[0], true
}

// loadSelfManifest 启动时从 ManifestPath 加载分发凭证：必须验签通过，
// 且与本地 exe 的实际 sha256 一致（防止清单指向被篡改的二进制）。
func (c *Coordinator) loadSelfManifest() *Manifest {
	m, err := readManifest(c.cfg.ManifestPath)
	if err != nil {
		return nil
	}
	if !m.Verify(c.cfg.PublicKey) {
		return nil
	}
	sum, err := FileSHA256(c.cfg.ExePath)
	if err != nil || sum != m.SHA256 {
		return nil
	}
	return &m
}

// ---- 分发源侧（handler） ----

// handleManifest 响应版本清单征询：无有效凭证则静默关闭。
// 顺序与 info 协议一致：先读完请求（对端 CloseWrite 后 EOF）再回写。
func (c *Coordinator) handleManifest(s network.Stream) {
	defer s.Close()
	var req struct {
		Current string `json:"current"`
	}
	_ = json.NewDecoder(s).Decode(&req) // 读完（EOF）
	c.mu.Lock()
	m := c.self
	c.mu.Unlock()
	if m == nil {
		return
	}
	if req.Current != "" && CompareVersions(req.Current, m.Version) >= 0 {
		return // 对端不比自己旧，无需分发
	}
	_ = json.NewEncoder(s).Encode(*m)
}

// fileReq 文件分发请求。
type fileReq struct {
	SHA256 string `json:"sha256"`
}

// handleFile 响应文件分发：帧格式 = [4 字节大端 JSON head 长度][head JSON][裸文件字节]。
func (c *Coordinator) handleFile(s network.Stream) {
	defer s.Close()
	var req fileReq
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		return
	}
	c.mu.Lock()
	m := c.self
	c.mu.Unlock()
	if m == nil || req.SHA256 != m.SHA256 {
		return
	}
	f, err := os.Open(c.cfg.ExePath)
	if err != nil {
		return
	}
	defer f.Close()
	// 发送前复验文件 sha256（分发自愈：本地文件被改时拒绝分发）。
	sum, err := FileSHA256(c.cfg.ExePath)
	if err != nil || sum != m.SHA256 {
		return
	}
	headJSON, _ := json.Marshal(*m)
	head := make([]byte, 4+len(headJSON))
	binary.BigEndian.PutUint32(head, uint32(len(headJSON)))
	copy(head[4:], headJSON)
	if _, err = s.Write(head); err != nil {
		return
	}
	if _, err = io.Copy(s, f); err != nil {
		return
	}
	_ = s.CloseWrite() // 通知对端文件发完（EOF）
}

// ---- 请求方侧 ----

// requestManifest 向对端征询版本清单。
func (c *Coordinator) requestManifest(ctx context.Context, id string) (Manifest, error) {
	pid, err := peer.Decode(id)
	if err != nil {
		return Manifest{}, err
	}
	s, err := c.host.NewStream(ctx, pid, ProtocolManifest)
	if err != nil {
		return Manifest{}, err
	}
	defer s.Close()
	// 请求体（当前版本，供对端决策；预留字段）。
	if err = json.NewEncoder(s).Encode(map[string]string{"current": c.cfg.CurrentVersion}); err != nil {
		return Manifest{}, err
	}
	if err = s.CloseWrite(); err != nil { // Windows 半关闭：对端才能读到 EOF
		return Manifest{}, err
	}
	var m Manifest
	if err = json.NewDecoder(s).Decode(&m); err != nil {
		return Manifest{}, err
	}
	if !m.Verify(c.cfg.PublicKey) {
		return Manifest{}, errors.New("selfupdate: 清单验签失败")
	}
	return m, nil
}

// download 从 sample 中随机选一个持有目标版本的成员下载文件到临时路径。
func (c *Coordinator) download(ctx context.Context, m Manifest, candidates []PeerInfo) (string, error) {
	dir := filepath.Dir(c.cfg.ExePath)
	tmp := filepath.Join(dir, fmt.Sprintf("selfupdate-%s.tmp", m.SHA256[:12]))
	// 随机起试，直到一个成功。
	order := make([]PeerInfo, len(candidates))
	copy(order, candidates)
	rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	var lastErr error
	for _, p := range order {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := c.requestFile(dctx, p.ID, m, tmp)
		cancel()
		if err == nil {
			return tmp, nil
		}
		lastErr = err
		c.logf("从 %s 下载失败，尝试下一成员: %v", shortID(p.ID), err)
		_ = os.Remove(tmp)
	}
	return "", lastErr
}

// requestFile 单对端下载：发请求 → 收 [head][file] → 边收边算 sha256 校验。
func (c *Coordinator) requestFile(ctx context.Context, id string, m Manifest, dest string) error {
	pid, err := peer.Decode(id)
	if err != nil {
		return err
	}
	s, err := c.host.NewStream(ctx, pid, ProtocolFile)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = json.NewEncoder(s).Encode(fileReq{SHA256: m.SHA256}); err != nil {
		return err
	}
	if err = s.CloseWrite(); err != nil {
		return err
	}
	br := bufio.NewReader(s)
	lenBuf := make([]byte, 4)
	if _, err = io.ReadFull(br, lenBuf); err != nil {
		return fmt.Errorf("读 head 长度: %w", err)
	}
	headJSON := make([]byte, binary.BigEndian.Uint32(lenBuf))
	if _, err = io.ReadFull(br, headJSON); err != nil {
		return fmt.Errorf("读 head: %w", err)
	}
	var head Manifest
	if err = json.Unmarshal(headJSON, &head); err != nil {
		return fmt.Errorf("解析 head: %w", err)
	}
	// head 必须与征询到的目标一致且验签有效（传输中途被换包也拦得住）。
	if head.SHA256 != m.SHA256 || head.Version != m.Version || !head.Verify(c.cfg.PublicKey) {
		return errors.New("selfupdate: 文件头与目标清单不一致或验签失败")
	}
	if head.Size <= 0 || head.Size > 512<<20 {
		return fmt.Errorf("文件大小非法: %d", head.Size)
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(br, head.Size)); err != nil {
		return fmt.Errorf("接收文件: %w", err)
	} else if n != head.Size {
		return fmt.Errorf("文件不完整: 收到 %d/%d 字节", n, head.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != m.SHA256 {
		return errors.New("selfupdate: 下载文件 sha256 校验失败")
	}
	return nil
}

// ---- 工具 ----

// FileSHA256 计算文件 sha256（小写 hex）。签名工具与运行时共用。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readManifest 读取本地清单文件。
func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	err = json.Unmarshal(data, &m)
	return m, err
}

// saveManifest 清单落盘（0600：内容可公开但没必要放宽）。
func saveManifest(path string, m Manifest) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

func (c *Coordinator) logf(format string, args ...any) {
	if c.cfg.Quiet {
		return
	}
	log.Printf("[lanet-selfupdate] "+format, args...)
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}
