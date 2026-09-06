package main

// p2pupdate.go P2P 自更新接线：
//   - 容器环境自动禁用（容器内替换二进制会被镜像回滚，更新走编排层）；
//   - Coordinator 巡检成员版本 → 发现更高版本即征询 → 验签 → P2P 下载；
//   - 下载成功后由本文件完成自替换与随机抖动重启（与 GitHub 更新互斥）。
//
// 信任链：CI 私钥签名（GitHub Secrets）→ 节点内置公钥验签 → sha256 下载
// 校验。验签通过即可信（1 票制），GitHub 仅是发版与首种子来源。

import (
	"context"
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

// netmapPeers 把 Client.NetMap 适配为 selfupdate.PeerSource。
type netmapPeers struct{ c *lanet.Client }

func (n netmapPeers) Peers() []selfupdate.PeerInfo {
	out := make([]selfupdate.PeerInfo, 0)
	for _, m := range n.c.NetMap().Members {
		out = append(out, selfupdate.PeerInfo{ID: m.PeerID, Version: m.Version, Platform: m.Platform})
	}
	return out
}

// updateBusy 全局更新互斥：GitHub 手动更新与 P2P 自动更新同一时间只跑一个。
var updateBusy sync.Mutex

// StartP2PUpdate 启动 P2P 自更新巡检。返回禁用原因（空 = 已启动）。
// 信任锚（Ed25519 签名）保证清单不可伪造，因此发现 1 个更高版本成员
// 即征询下载，验签通过就可信；错峰重启避免全网同时重启。
func StartP2PUpdate(ctx context.Context, c *lanet.Client, version string, exeDir string) string {
	if version == "" || version == "dev" {
		return "dev 构建（版本号不可比）"
	}
	if runningInContainer() {
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
	// 谁先下载完成谁触发重启。
	coord := selfupdate.New(c.Host(), netmapPeers{c}, selfupdate.Config{
		CurrentVersion: version,
		Platform:       runtime.GOOS + "/" + runtime.GOARCH,
		ExePath:        exePath,
		ManifestPath:   filepath.Join(exeDir, "update-manifest.json"),
	}, func(newPath string, m selfupdate.Manifest) {
		applyP2PUpdate(newPath, m, exePath)
	})
	coord.Start(ctx)
	log.Printf("[p2p-update] 已启动：巡检 30 分钟/轮，发现更高版本即征询下载（验签通过才升级）")
	return ""
}

// applyP2PUpdate 替换自身并随机抖动重启（错峰，避免全网同时重启瘫痪 DHT）。
func applyP2PUpdate(newPath string, m selfupdate.Manifest, exePath string) {
	updateBusy.Lock()
	defer updateBusy.Unlock()

	log.Printf("[p2p-update] 新版本 v%s 下载校验完成，替换程序", m.Version)
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(exePath, oldPath); err != nil {
		log.Printf("[p2p-update] 旧程序改名失败（不中断运行）: %v", err)
		return
	}
	if err := copyFile(newPath, exePath); err != nil {
		_ = os.Rename(oldPath, exePath) // 回滚
		log.Printf("[p2p-update] 写入新程序失败（已回滚）: %v", err)
		return
	}
	_ = os.Remove(newPath)
	// 随机 1~8 分钟后重启：升级节点天然错峰，DHT 网络始终有大量节点在线。
	delay := time.Duration(60+rand.Intn(7*60)) * time.Second
	log.Printf("[p2p-update] 已更新到 v%s，%s 后重启生效", m.Version, delay.Truncate(time.Second))
	restartSelf(delay)
}
