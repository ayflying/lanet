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

	"github.com/ayflying/pvn/pkg/protocol"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
)

// 节点间更新协议：manifest = 版本清单征询；file = 文件分发。
//
// 这两个是「全域固定 ID」。协议 ID 的用法在三代里变过：
//   - ≤0.5.33：只有固定 ID。任何能拨通端口的人都能拉走完整二进制，公网
//     节点因此成为流量放大器。
//   - 0.5.34~0.5.48：默认按群密钥派生 ID（pkg/protocol.GroupProtoID），
//     异群节点与扫描器在 multistream 阶段即被拒；固定 ID 仅作老版本兜底，
//     且入向挂了「只放行同群好友」的成员门。
//   - 0.5.49 起：需求是「整个私有 DHT 网络里任何节点发现新版本都能互传，
//     不要求同网络密钥、不要求加过好友」，所以固定 ID 重新成为主力入口
//     （派生 ID 仍注册，同群走它更省一跳）。防滥用手段随之从「协议隔离 +
//     好友门」换成**分发限流**（rateLimiter：单对端最小间隔 + 全局并发上限），
//     否则会退回 0.5.33 前的老问题。二进制本身不是机密（release 公开可下载），
//     限流要保证的是「不被当成免费 CDN」，而不是保密。
const (
	ProtocolManifest libprotocol.ID = "/lanet/update-manifest/1.0.0"
	ProtocolFile     libprotocol.ID = "/lanet/update-file/1.0.0"
)

// signPrefix 签名域分隔：签名内容 = prefix + version + ":" + platform + ":" + size + ":" + sha256hex。
// 所有字段（含 Size）都入签——测试曾发现仅漏签 Size 时可被篡改。
const signPrefix = "lanet-update-v1:"

// DistManifestName 发行包内自带的签名清单文件名。CI 把单平台签名分片放进
// 压缩包（release.yml），解压即得分发凭证；运行期落盘的凭证则叫
// update-manifest.json（ManifestPath 默认值）。两个名字都要认——历史上只认
// 后者，「解压即种子」的设计意图因此完全落空（首装节点永远不是分发源）。
const DistManifestName = "manifest.json"

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
	Version  string // 空 = 版本未知（见 PeerSource 文档）
	Platform string // 空 = 平台未知
}

// PeerSource 更新候选视图来源（宿主把「成员表 ∪ 私有 DHT 网络节点」适配进来）。
//
// 候选允许带未知字段：
//   - Version 为空表示宿主拿不到对端版本（私有 DHT 路由表里的节点只有 ID，
//     被动发现的「附近」节点也没有版本字段）。这类候选不会被本地比较筛掉，
//     而是留到 round 里向它们征询清单——版本信息只能从对端清单获取。
//   - Platform 为空同理，最终以清单里的 Platform 字段为准（round 会二次把关）。
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
	// CheckInterval 版本巡检周期，默认 5 分钟。巡检本身只读本地成员表
	// （不发网络请求），只有确认自己落后时才征询清单，因此周期可以很短
	// ——历史默认 30 分钟会让「发现新版本」平均延迟一刻钟。
	CheckInterval time.Duration
	// InitialDelay 启动后首次巡检的延迟，默认 30 秒（负数 = 启动即巡）。
	// 成员表填充只需秒级，没必要等满一个周期才第一次比较版本。
	InitialDelay time.Duration
	// MinNewPeers 触发征询所需的更高版本同平台成员数，默认 1。
	// 发现 1 个更高版本成员即征询其清单；验签通过即可信（签名信任锚
	// 保证清单出自发布私钥，无需多票灰度），多份时要求完全一致。
	MinNewPeers int
	// GroupKey 本群群组密钥（serverless.Discovery.GroupKey）。非空时更新
	// 协议 ID 按群派生：只有同网络密钥的节点能协商这两个协议，异群扫描器
	// 与公网流量在 multistream 阶段即被挡（0.5.34 私有协议加固）。留空
	// （如单测）则退回历史固定 ID。
	GroupKey []byte
	// LegacyProtocols 逃生开关：置 true 强制使用历史固定 ID（与未升级老
	// 对端互通）。默认 false。无论真假，出向都会在派生 ID 协商失败后自动
	// 兜底尝试固定 ID，因此通常无需开启。
	LegacyProtocols bool
	// PerPeerMinInterval 分发限流：同一对端两次请求的最小间隔（manifest 与
	// file 合算），默认 30 秒。0.5.49 起更新协议对整个私有 DHT 网络开放
	// （不再要求同群/好友），限流就是取代旧「成员门」的防滥用手段——保证
	// 本机不会因为协议对全网可达而被当成免费 CDN。
	PerPeerMinInterval time.Duration
	// MaxInflightStreams 分发限流：同时在处理的入向请求上限，默认 4。
	// 一次文件分发可能持续数分钟（数十 MB），上限太低会让「多个节点同时
	// 升级」互相饿死；太高则失去防放大意义。
	MaxInflightStreams int
	// FileDownloadCooldown 分发限流：同一对端两次**文件下载**的最小间隔，
	// 默认 5 分钟。与 PerPeerMinInterval（清单征询）分开计数——一次正常
	// 升级需要「1 次征询 + 1 次下载」，合算会把升级链自己掐死。
	FileDownloadCooldown time.Duration
	// UpdateInFlight 宿主的更新锁查询：返回 true 表示「已有一轮更新在途」
	// （已下载校验完成、等待重启；或 GitHub 在线更新正在执行）。巡检每轮
	// 开头先查这一门：更新一旦启动就锁到进程重启为止，期间即便又发现更高
	// 版本也不再重复下载/替换/排重启。
	// 为什么必须要有它：替换完成后进程仍在跑旧代码，CurrentVersion 是编译期
	// 常量，下一轮巡检仍会把自己判成落后，于是再次下载、再次替换、再排一次
	// 重启——1~8 分钟的错峰重启窗口里可以反复发生（0.5.49 前实测存在）。
	// nil = 不做此门（单测）。
	UpdateInFlight func() bool
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
		c.CheckInterval = 5 * time.Minute
	}
	if c.InitialDelay == 0 {
		c.InitialDelay = 30 * time.Second
	}
	if c.MinNewPeers <= 0 {
		c.MinNewPeers = 1
	}
	if c.ManifestPath == "" {
		c.ManifestPath = filepath.Join(filepath.Dir(c.ExePath), "update-manifest.json")
	}
	if c.PerPeerMinInterval <= 0 {
		c.PerPeerMinInterval = 30 * time.Second
	}
	if c.MaxInflightStreams <= 0 {
		c.MaxInflightStreams = 4
	}
	if c.FileDownloadCooldown <= 0 {
		c.FileDownloadCooldown = 5 * time.Minute
	}
}

// Coordinator P2P 更新协调器：既是分发源（持有有效 head 后对外提供
// manifest 与文件），也是升级决策者（巡检成员版本、征询、下载）。
type Coordinator struct {
	host host.Host
	src  PeerSource
	cfg  Config

	// 更新协议 ID（0.5.34 起按群密钥派生）。Alt 为出向兜底用的历史固定
	// ID（仅主 ID 协商失败后尝试），入向只注册主 ID——不把私有协议重新敞开。
	protoManifest    libprotocol.ID
	protoFile        libprotocol.ID
	protoManifestAlt libprotocol.ID
	protoFileAlt     libprotocol.ID

	onUpdate func(path string, m Manifest) // 下载校验成功回调（宿主替换+重启）

	mu   sync.Mutex
	self *Manifest // 本地有效 head：验签通过且与本地 exe sha256 一致

	attempts map[string]bool // 已尝试过下载的 sha256（进程内防重）

	// limiter 入向分发限流（取代 0.5.34~0.5.48 的「同群好友门」）。
	limiter *rateLimiter
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
		limiter:  newRateLimiter(cfg.PerPeerMinInterval, cfg.FileDownloadCooldown, cfg.MaxInflightStreams),
	}
	// 协议 ID：有群密钥时按群派生（异群/未入网者在 multistream 协商阶段
	// 即被拒，拉不走任何字节）；出向保留固定 ID 作老版本兜底。
	derived := len(cfg.GroupKey) > 0 && !cfg.LegacyProtocols
	switch {
	case derived:
		c.protoManifest = protocol.GroupProtoID(protocol.BaseUpdManifest, cfg.GroupKey)
		c.protoFile = protocol.GroupProtoID(protocol.BaseUpdFile, cfg.GroupKey)
		c.protoManifestAlt = ProtocolManifest
		c.protoFileAlt = ProtocolFile
	default:
		c.protoManifest = ProtocolManifest
		c.protoFile = ProtocolFile
	}
	c.self = c.loadSelfManifest()
	// 固定 ID 谁都能协商 → 一律过限流器（主 ID 本身就是固定 ID 时同样如此）。
	gateFixed := func(id libprotocol.ID, bucket string, h func(network.Stream)) func(network.Stream) {
		if id == ProtocolManifest || id == ProtocolFile {
			return c.gateRate(bucket, h)
		}
		return h
	}
	h.SetStreamHandler(c.protoManifest, gateFixed(c.protoManifest, bucketManifest, c.handleManifest))
	h.SetStreamHandler(c.protoFile, gateFixed(c.protoFile, bucketFile, c.handleFile))
	if derived {
		// 全域兼容入口（0.5.49）：更新分发不再要求「同群 + 好友」——用户要的
		// 是整个私有 DHT 网络内任何节点发现新版本都能互传，所以固定 ID 对
		// 所有 lanet 节点开放。旧的 gateMember（IsMember=是否好友）因此换成
		// gateRate：单对端最小间隔 + 全局并发上限，挡的是流量放大，而不是人。
		h.SetStreamHandler(ProtocolManifest, c.gateRate(bucketManifest, c.handleManifest))
		h.SetStreamHandler(ProtocolFile, c.gateRate(bucketFile, c.handleFile))
	}
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
	// 更新锁（宿主提供）：已有一轮更新在途就别再开第二轮。替换完成后进程
	// 跑的仍是旧代码、CurrentVersion 是编译期常量不会变，不拦就会在 1~8
	// 分钟的错峰重启窗口里反复下载、反复替换、反复排重启。
	if c.cfg.UpdateInFlight != nil && c.cfg.UpdateInFlight() {
		c.logf("已有一轮更新在途（等待重启），跳过本轮巡检")
		return
	}
	peers := c.src.Peers()
	newer := c.selectCandidates(cur, peers)
	if len(newer) < c.cfg.MinNewPeers {
		return
	}
	// 随机抽样征询：MinNewPeers=1 时向所有更高版本成员征询（谁在线问谁，
	// 提高响应率）；多成员时抽样上限设为 3 份用于交叉比对。
	rand.Shuffle(len(newer), func(i, j int) { newer[i], newer[j] = newer[j], newer[i] })
	sample := newer
	maxSample := c.cfg.MinNewPeers
	if maxSample < 3 {
		maxSample = 3
	}
	if len(sample) > maxSample {
		sample = sample[:maxSample]
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

	// 共识 + 逐份验签：验签通过（出自发布私钥）即满足票数门槛；多份一致
	// 更佳，不一致时取验签通过的最高版本由 CompareVersions 二次把关。
	target, ok := consensus(heads, c.cfg.PublicKey, c.cfg.MinNewPeers)
	if !ok {
		c.logf("征询到 %d/%d 份清单但未形成可信共识，放弃本轮", len(heads), len(sample))
		return
	}
	if CompareVersions(cur, target.Version) >= 0 {
		return
	}
	// 平台二次把关：候选的平台可能未知（DHT 网络里的陌生节点），清单才是
	// 权威依据——绝不能把别的平台的二进制替换到本机 exe 上。
	if target.Platform != c.cfg.Platform {
		c.logf("发现新版本 v%s 但平台是 %s（本机 %s），跳过", target.Version, target.Platform, c.cfg.Platform)
		return
	}
	c.mu.Lock()
	if c.attempts[target.SHA256] { // 本进程内同一版本只尝试一次
		c.mu.Unlock()
		return
	}
	c.attempts[target.SHA256] = true
	c.mu.Unlock()

	c.logf("发现新版本 v%s（sha256=%s…，验签通过），开始下载",
		target.Version, target.SHA256[:12])
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

// selectCandidates 从候选视图里挑出「值得征询清单」的节点，分两层：
//
//	第一层：版本已知且比自己新的节点 —— 本地比较即可判定，零网络开销；
//	第二层：版本未知的节点 —— 私有 DHT 网络里未加好友、未建连的节点只有 ID
//	        （被动发现不建连、附近表也没有版本字段），本地无从比较，只能问。
//
// 只有第一层为空时才发第二层，且一轮抽样上限 3 个：既保证「DHT 网络里任何
// 节点有新版本都能被发现」，又不至于每 5 分钟把整张路由表问一遍。
// 平台已知但与本机不同的候选直接丢弃（二进制装不上本机）。
func (c *Coordinator) selectCandidates(cur string, peers []PeerInfo) []PeerInfo {
	var knownNewer, unknown []PeerInfo
	for _, p := range peers {
		if p.ID == "" {
			continue
		}
		if p.Platform != "" && p.Platform != c.cfg.Platform {
			continue
		}
		if p.Version == "" {
			unknown = append(unknown, p)
			continue
		}
		if CompareVersions(cur, p.Version) < 0 { // 对端比本端新
			knownNewer = append(knownNewer, p)
		}
	}
	if len(knownNewer) > 0 {
		return knownNewer
	}
	rand.Shuffle(len(unknown), func(i, j int) { unknown[i], unknown[j] = unknown[j], unknown[i] })
	maxProbe := c.cfg.MinNewPeers
	if maxProbe < 3 {
		maxProbe = 3
	}
	if len(unknown) > maxProbe {
		unknown = unknown[:maxProbe]
	}
	return unknown
}

// consensus 要求 heads 中验签通过的份数 >= need。份数达标时返回验签通过
// 清单中「版本最高」的一份（need=1 时单份验签通过即可信——签名信任锚保证
// 清单出自发布私钥；多份且同版本必须 sha256 一致，同版本不一致视为异常放弃）。
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
	if need == 1 {
		// 同版本多份但 sha256 不一致 = 分发被污染，宁可放弃。
		byVersion := make(map[string]string)
		for _, m := range valid {
			if prev, ok := byVersion[m.Version]; ok && prev != m.SHA256 {
				return Manifest{}, false
			}
			byVersion[m.Version] = m.SHA256
		}
		best := valid[0]
		for _, m := range valid[1:] {
			if CompareVersions(best.Version, m.Version) < 0 {
				best = m
			}
		}
		return best, true
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
// selfManifestPaths 本地分发凭证的候选路径。首选运行期落盘的
// update-manifest.json（P2P 下载成功或 GitHub 更新解包时写入），兜底读发行
// 包自带的 manifest.json —— CI 把签名分片打进压缩包（release.yml 的
// 「解压即有分发凭证」），但那个文件叫 manifest.json，与运行期落盘名不同；
// 只认后者会让「解压即种子」的设计意图完全落空（首装节点永远不是分发源）。
func (c *Coordinator) selfManifestPaths() []string {
	primary := c.cfg.ManifestPath
	if primary == "" {
		return nil
	}
	out := []string{primary}
	alt := filepath.Join(filepath.Dir(primary), DistManifestName)
	if alt != primary {
		out = append(out, alt)
	}
	return out
}

func (c *Coordinator) loadSelfManifest() *Manifest {
	// 本地程序指纹只算一次：凭证必须与当前 exe 逐字节对应，否则不得对外
	// 分发（替换失败/旧凭证残留时自动失效，绝不传播假凭证）。
	sum, err := FileSHA256(c.cfg.ExePath)
	if err != nil {
		return nil
	}
	for _, path := range c.selfManifestPaths() {
		m, err := readManifest(path)
		if err != nil {
			continue
		}
		if !m.Verify(c.cfg.PublicKey) {
			continue
		}
		if m.SHA256 != sum {
			continue
		}
		return &m
	}
	return nil
}

// ---- 分发源侧（handler） ----

// ---- 入向分发限流 ----
//
// 0.5.49 起更新协议对整个私有 DHT 网络可达（不再要求同群，更不要求好友），
// 于是「谁能拉」从身份判定变成了流量控制：任何能拨通的 lanet 节点都能征询
// 清单/下载二进制，不设限就成了免费 CDN。规则两条：
//   - 同一对端两次请求至少间隔 PerPeerMinInterval（默认 30s，manifest 与
//     file 合算）——一次正常升级只征询一轮、只拉一次文件，完全够用；
//   - 同时在传的请求不超过 MaxInflightStreams（默认 4）。file 传输可能持续
//     几分钟，这个上限防的是并发放大。
//
// 二进制不是机密（GitHub release 公开可下载），所以限流目标是「不被滥用」，
// 不是「防泄漏」。
const (
	// 限流桶：manifest 与 file 分开计数。一次正常升级天然需要「1 次清单征询 +
	// 1 次文件下载」，合算成一个名额会把升级链自己掐死（开发中实测：file 请求
	// 紧跟在 manifest 之后发出，被同一个窗口 Reset，升级永远走不完）。
	bucketManifest = "manifest"
	bucketFile     = "file"
)

type rateLimiter struct {
	mu       sync.Mutex
	last     map[string]time.Time // "bucket|remote" -> 上次放行时刻
	inflight int
	per      time.Duration // manifest 桶：单对端最小间隔
	filePer  time.Duration // file 桶：单对端最小间隔（文件大，间隔更长）
	maxIn    int
}

func newRateLimiter(per, filePer time.Duration, maxIn int) *rateLimiter {
	if per <= 0 {
		per = 30 * time.Second
	}
	if filePer <= 0 {
		filePer = 5 * time.Minute
	}
	if maxIn <= 0 {
		maxIn = 4
	}
	return &rateLimiter{
		last:    make(map[string]time.Time),
		per:     per,
		filePer: filePer,
		maxIn:   maxIn,
	}
}

// allow 申请一次分发名额（bucket = bucketManifest / bucketFile）；
// 返回 true 时调用方必须配对 release。
func (r *rateLimiter) allow(bucket, remote string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight >= r.maxIn {
		return false
	}
	per := r.per
	if bucket == bucketFile {
		per = r.filePer
	}
	key := bucket + "|" + remote
	now := time.Now()
	if t, ok := r.last[key]; ok && now.Sub(t) < per {
		return false
	}
	r.last[key] = now
	r.inflight++
	if len(r.last) > 4096 {
		// 防 map 无界增长：仅在超阈值时做一次惰性清理。
		for k, t := range r.last {
			if now.Sub(t) > 4*r.filePer {
				delete(r.last, k)
			}
		}
	}
	return true
}

func (r *rateLimiter) release() {
	r.mu.Lock()
	if r.inflight > 0 {
		r.inflight--
	}
	r.mu.Unlock()
}

// gateRate 给「对全网可达」的 handler 包一层限流（固定协议 ID 入口）。
func (c *Coordinator) gateRate(bucket string, next func(network.Stream)) func(network.Stream) {
	return func(s network.Stream) {
		remote := s.Conn().RemotePeer().String()
		if !c.limiter.allow(bucket, remote) {
			_ = s.Reset() // 超限：立即重置，零字节分发
			return
		}
		defer c.limiter.release()
		next(s)
	}
}

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

// newStream 出向建流：优先主协议 ID（按群派生）；传了兜底 ID 时把它一并
// 交给 multistream 协商（一次拨号同时携带两个候选，对端注册了哪个就命中
// 哪个），用于同群新老版本混跑的过渡期。全部协商失败才报错。
func (c *Coordinator) newStream(ctx context.Context, pid peer.ID, primary libprotocol.ID, fallbacks ...libprotocol.ID) (network.Stream, error) {
	protos := make([]libprotocol.ID, 0, 1+len(fallbacks))
	protos = append(protos, primary)
	for _, f := range fallbacks {
		if f != "" && f != primary {
			protos = append(protos, f)
		}
	}
	return c.host.NewStream(ctx, pid, protos...)
}

// requestManifest 向对端征询版本清单。
func (c *Coordinator) requestManifest(ctx context.Context, id string) (Manifest, error) {
	pid, err := peer.Decode(id)
	if err != nil {
		return Manifest{}, err
	}
	s, err := c.newStream(ctx, pid, c.protoManifest, c.protoManifestAlt)
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
	s, err := c.newStream(ctx, pid, c.protoFile, c.protoFileAlt)
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

// InstallDistManifest 校验并落盘发行包内自带的签名清单，使其成为运行期
// 分发凭证——本节点重启后即可作为 P2P 种子。raw 为包内 manifest.json 的
// 原始字节，exePath 是刚解出的新程序（校验清单与该程序逐字节一致）。
//
// 三道校验缺一不可：平台匹配（防止把别的平台的分片当自己的凭证）、
// 发布私钥验签（信任锚）、sha256 与本地程序一致（凭证只对得上自己才算数）。
// 任何一项不符都返回错误且不落盘——宁可不做种子，也不传播假凭证。
//
// 为什么必须在这里做：GitHub 更新路径只解出可执行文件，历史上从不落盘
// 凭证，于是每个「走 GitHub 升级过」的节点都会丢掉分发能力，P2P 升级链
// 随之整体断掉（本机实测 update-manifest.json 停在 0.5.8、程序已 0.5.42）。
func InstallDistManifest(raw []byte, manifestPath, exePath string) error {
	return InstallDistManifestWith(raw, manifestPath, exePath, ReleasePublicKey)
}

// InstallDistManifestWith 同 InstallDistManifest，但显式指定验签公钥。
// 生产一律用 ReleasePublicKey；单独暴露是为了单测能自造密钥对（私钥只在
// CI Secrets 里，测试拿不到，否则正向路径无从覆盖）。
func InstallDistManifestWith(raw []byte, manifestPath, exePath, pubB64 string) error {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("清单解析失败: %w", err)
	}
	want := runtime.GOOS + "/" + runtime.GOARCH
	if m.Platform != want {
		return fmt.Errorf("清单平台不符（凭证 %s，本机 %s）", m.Platform, want)
	}
	if !m.Verify(pubB64) {
		return errors.New("清单验签失败")
	}
	sum, err := FileSHA256(exePath)
	if err != nil {
		return err
	}
	if sum != m.SHA256 {
		return errors.New("清单 sha256 与包内程序不一致")
	}
	return saveManifest(manifestPath, m)
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
