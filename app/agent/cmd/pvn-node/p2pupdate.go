package main

// p2pupdate.go P2P 自更新接线：
//   - 容器环境自动禁用（容器内替换二进制会被镜像回滚，更新走编排层）；
//   - 候选来源 = 成员表 ∪ 私有 DHT 路由表，覆盖**整个 lanet 私有 DHT 网络**
//     ——不同网络密钥、没加过好友、甚至只是被 DHT 路由过的节点都算，发现更高
//     版本即征询；
//   - 验签通过后 P2P 下载，由本文件完成自替换与随机抖动重启。
//
// 关于「附近」：0.5.109 起「附近在线节点」功能整体删除（协议、观察表、控制台
// 卡片、SDK 探测状态机全部移除），候选来源因此从三类收敛为两类。新语义是：
// 同一网络密钥下的节点在 **P2P 层自动连接**用于控制同步（互相通知组件成员与
// 地址线索、PEX 种子交换、更新分发），但**不参与用户路由**——不可 ping、
// 不可访问端口、不进成员列表。也就是说「自动连上」与「能不能用」被彻底分开了：
// 连接不再等价于成员授权，所以不再需要一个「附近列表」把未审批节点暴露给用户。
//
// 信任链：CI 私钥签名（GitHub Secrets）→ 节点内置公钥验签 → sha256 下载
// 校验。验签通过即可信（1 票制），GitHub 仅是发版与首种子来源。
//
// 更新锁（0.5.49）：GitHub 在线更新与 P2P 自动更新共用一把闸门；替换成功后
// 闸门一直占住到进程重启为止。否则「已替换、待重启」的那 1~8 分钟窗口里，
// 下一轮巡检仍会把本进程判成落后（CurrentVersion 是编译期常量，不会随磁盘
// 上的新 exe 变化），于是再次下载、再次替换、再排一次重启——反复更新。

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
	"github.com/ayflying/pvn/sdk/go/lanet"
)

// runningInContainer 检测容器环境（Docker/Podman 常见标记文件）。
func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	return false
}

// ---- 更新闸门 ----

// updateGate 更新闸门：GitHub 在线更新（控制台点击）与 P2P 自动更新共用。
//
// busy 为 true 的含义是「一轮更新已经开始并已落地/正在落地」：
//   - 正在下载/解压/替换：另一条路径必须让路，否则两个流程会同时替换同一个 exe；
//   - 已替换成功、等待重启：本进程版本号不会变，绝不能开始第二轮。
//
// 只有「整轮更新彻底失败、没有替换成」才释放，允许下轮巡检重试。
var updateGate struct {
	mu   sync.Mutex
	busy bool
}

// errUpdateInFlight 已有更新在途时的错误（控制台据此回 409 而不是 500）。
var errUpdateInFlight = errors.New("已有更新在进行中，请等待本次更新完成（程序会自动重启）")

// acquireUpdate 抢占更新闸门；返回 false 表示已有更新在途，调用方必须放弃。
func acquireUpdate() bool {
	updateGate.mu.Lock()
	defer updateGate.mu.Unlock()
	if updateGate.busy {
		return false
	}
	updateGate.busy = true
	return true
}

// releaseUpdate 更新未落地（下载/校验/替换失败）时释放闸门，允许下轮重试。
func releaseUpdate() {
	updateGate.mu.Lock()
	updateGate.busy = false
	updateGate.mu.Unlock()
}

// updateInFlight 是否已有更新在途（P2P 巡检每轮开头据此跳过本轮）。
func updateInFlight() bool {
	updateGate.mu.Lock()
	defer updateGate.mu.Unlock()
	return updateGate.busy
}

// netmapPeers 把「本机能看到的 lanet 节点」适配为 selfupdate.PeerSource。
//
// 两个来源合并（按 PeerID 去重）：
//  1. 成员表：已审批的同群节点，带版本与平台 —— 巡检可纯本地比较，零开销；
//  2. 私有 DHT 路由表：整个 lanet DHT 网络的节点（含不同网络密钥、未加好友
//     的），只有 ID，版本靠征询清单获取。
//
// 2 是主力来源：用户要的是「P2P 更新不一定需要在自己的网络密钥下面，是整个
// DHT 网络，发现新版本就更新」。只盯成员表永远覆盖不到未加好友的节点。
//
// 为什么删掉「附近」观察表这一来源不会漏掉「同群但未审批」的节点：
// 私有 DHT 前缀是按群组密钥派生的（GroupProtoID），同群节点共用同一张私有
// DHT 路由网，路由表只由 DHT 协议本身维护（本文件不做信任过滤），所以只要
// 双方在同一网络密钥下并参与私有 DHT，未审批节点同样会出现在
// DHTRoutingPeers() 里 —— 它们不需要成为成员就能被当成更新候选。
// 这也是「P2P 自动连接 ≠ 成员授权」在新语义下的直接体现：能互相发现、
// 能互相征询更新，但拿不到虚拟 IP、不能 ping、进不了成员表。
type netmapPeers struct{ c *lanet.Client }

func (n netmapPeers) Peers() []selfupdate.PeerInfo {
	out := make([]selfupdate.PeerInfo, 0, 16)
	seen := make(map[string]bool, 16)
	add := func(p selfupdate.PeerInfo) {
		if p.ID == "" || seen[p.ID] {
			return
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	for _, m := range n.c.NetMap().Members {
		add(selfupdate.PeerInfo{ID: m.PeerID, Version: m.Version, Platform: m.Platform})
	}
	for _, id := range n.c.DHTRoutingPeers() {
		add(selfupdate.PeerInfo{ID: id})
	}
	return out
}

// StartP2PUpdate 启动 P2P 自更新巡检。返回禁用原因（空 = 已启动）。
// 信任锚（Ed25519 签名）保证清单不可伪造，因此发现 1 个更高版本节点即征询
// 下载，验签通过就可信；错峰重启避免全网同时重启。
func StartP2PUpdate(ctx context.Context, c *lanet.Client, version string, exeDir string) string {
	if version == "" || version == "dev" {
		return "dev 构建（版本号不可比）"
	}
	if containerized() {
		return "容器环境（更新走编排层）"
	}
	exePath, err := os.Executable()
	if err != nil {
		return "无法定位自身程序: " + err.Error()
	}
	if real, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = real
	}
	// P2P 更新只对裸机二进制形态有意义；控制台 GitHub 更新与 P2P 共存，
	// 谁先下载完成谁触发重启（两者共用 updateGate，见文件头）。
	coord := selfupdate.New(c.Host(), netmapPeers{c}, selfupdate.Config{
		CurrentVersion: version,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		ExePath:        exePath,
		ManifestPath:   filepath.Join(exeDir, "update-manifest.json"),
		// 巡检节奏：「发现新版本立即升级」——启动 30 秒后先比一轮版本，
		// 之后每 5 分钟一轮。候选来自内存视图（成员表/路由表），
		// 真正的网络开销只在向「版本未知」的候选征询时发生（一轮最多 3 个）。
		InitialDelay:  30 * time.Second,
		CheckInterval: 5 * time.Minute,
		// GroupKey 只用于「同群快路径」：出向优先协商派生协议 ID（同网络密钥
		// 的节点少一跳），协商失败自动兜底全域固定 ID；入向两个 ID 都注册。
		GroupKey:        c.GroupKey(),
		LegacyProtocols: c.LegacyProtocols(),
		// UpdateInFlight 更新锁：巡检每轮开头查一次，已有一轮更新在途就跳过。
		UpdateInFlight: updateInFlight,
		AcquireUpdate:  acquireUpdate,
		ReleaseUpdate:  releaseUpdate,
	}, func(newPath string, m selfupdate.Manifest) {
		applyP2PUpdateLocked(newPath, m, exePath)
	})
	coord.Start(ctx)
	log.Printf("[p2p-update] 已启动：启动 30s 首轮、之后 5 分钟/轮巡检；候选覆盖整个 DHT 网络（不要求同网络密钥或好友），发现更高版本即征询下载（验签通过才升级）")
	return ""
}

var (
	installP2PBinary  = stageNewBinary
	restartP2PProcess = restartSelf
)

// applyP2PUpdateLocked 接收下载前已取得的闸门，不再次 acquire。
func applyP2PUpdateLocked(newPath string, m selfupdate.Manifest, exePath string) {
	log.Printf("[p2p-update] 新版本 v%s 下载校验完成，替换程序", m.Version)
	if err := installP2PBinary(newPath, exePath); err != nil {
		log.Printf("[p2p-update] 替换程序失败（不中断运行）: %v", err)
		releaseUpdate() // 没落地，允许下轮重试
		return
	}
	// stageNewBinary 以复制方式落盘，不删除 coordinator 管理的下载源文件。
	// 注意：此处**不释放闸门**。待重启进程不应再开始第二轮
	// 更新——否则升级窗口里再出现更新版本时会反复替换、反复排重启。
	// 随机 1~8 分钟后重启：升级节点天然错峰，DHT 网络始终有大量节点在线。
	delay := time.Duration(60+rand.Intn(7*60)) * time.Second
	log.Printf("[p2p-update] 已更新到 v%s，%s 后重启生效（更新锁已持有，重启前不再接受新的更新轮次）",
		m.Version, delay.Truncate(time.Second))
	restartP2PProcess(delay)
}
