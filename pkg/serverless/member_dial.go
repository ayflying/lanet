package serverless

import (
	"context"
	"fmt"
	"time"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	// memberDirectDialTimeout 直连预算。对端直连地址可能已经是黑洞
	// （NAT 重映射、IPv6 隐私地址滚动），必须给够时间再判失败。
	memberDirectDialTimeout = 10 * time.Second
	// memberRelayDialTimeout 中继兜底总预算（整段，不是每条路径）。经中继要
	// 多跳 Relay 转发，RTT 天然高于直连（实测跨省中继单次可达数秒）。
	memberRelayDialTimeout = 25 * time.Second
	// memberRelayAttemptTimeout 单条中继路径的预算上限。整段预算再按剩余
	// 路径数摊分取小值：没有它，4 个候选 × 2 条地址 × 25s 最坏能拖到 200s，
	// 8 个调度 worker 会被少数死成员长期占满。
	memberRelayAttemptTimeout = 6 * time.Second
	// memberRelayDialCandidates 一轮最多试几个中继候选：候选里可能混着
	// 已失去预约的中继，试太多只会拖长单轮恢复周期。
	memberRelayDialCandidates = 4
	// memberRelayAddrsPerCandidate 同一个中继上最多试两条承载地址。
	memberRelayAddrsPerCandidate = 2
)

// connectMember 建立到成员的 libp2p 连接：先直连，失败后经中继兜底。
//
// 为什么必须有中继兜底（2026-11-20 实机缺陷）：两端都在 NAT 后时，peerstore
// 里的直连地址会全部失效；而发现链会把 /p2p-circuit 地址当作「结构性有害」
// 主动清理（p2pkit.PrunePeerstoreAddrs），所以「只拨 peerstore 地址」等价于
// 「直连一失败就再无手段」。控制面（成员握手）没有这条兜底，而数据面
// （pkg/tunnel.openViaRelay）有——数据面又依赖控制面先确认成员才拿得到
// netmap 路由，于是形成死锁：成员永远离线，中继兜底永远触发不了。
//
// 这里把中继路径改成拨号时现拼：用本机已有预约的中继候选 + 目标 PeerID
// 组成 /p2p-circuit 路径，不往 peerstore 写任何 circuit 地址，
// 因此不会重新引入「地址簿被 circuit 变体刷爆」的老问题。
func (d *Discovery) connectMember(ctx context.Context, id peer.ID) error {
	directCtx, cancelDirect := context.WithTimeout(ctx, memberDirectDialTimeout)
	directErr := d.host.Connect(directCtx, peer.AddrInfo{ID: id})
	cancelDirect()
	if directErr == nil {
		return nil
	}
	relayCtx, cancelRelay := context.WithTimeout(ctx, memberRelayDialTimeout)
	defer cancelRelay()
	relayErr := d.connectMemberViaRelay(relayCtx, id)
	if relayErr != nil {
		return fmt.Errorf("直连失败（%v）；中继兜底失败: %w", directErr, relayErr)
	}
	d.logf("成员 %s 直连不通，已经中继建立连接", id.ShortString())
	return nil
}

// connectMemberViaRelay 取中继候选后逐个尝试打开到 target 的 circuit 路径。
//
// 只做连接建立，不做握手：握手由调用方在同一连接上走 info 协议完成，
// 这样连接与协议协商的失败原因能分开报，不再把链路抖动误报成「不同群」。
func (d *Discovery) connectMemberViaRelay(ctx context.Context, target peer.ID) error {
	candidates, err := d.Candidates(ctx, memberRelayDialCandidates)
	if err != nil {
		return fmt.Errorf("取中继候选失败: %w", err)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("没有可用中继候选")
	}
	return d.connectMemberViaCandidates(ctx, target, candidates)
}

// connectMemberViaCandidates 用给定的中继候选逐个尝试建立 circuit 连接。
// 与候选来源解耦：候选既可以来自成员表（Candidates），也可以来自后续新增的
// 其他可信来源；测试也能确定性地覆盖拨号逻辑，不受本机网卡形态影响。
func (d *Discovery) connectMemberViaCandidates(ctx context.Context, target peer.ID, candidates []peer.AddrInfo) error {
	var lastErr error
	attempted := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, addr := range p2pkit.CircuitAddrs(candidate, target, memberRelayAddrsPerCandidate) {
			if err := ctx.Err(); err != nil {
				return err
			}
			budget := memberRelayAttemptTimeout
			if deadline, ok := ctx.Deadline(); ok {
				// 只拿剩余时间的一半：给后续候选留出机会，避免前面的黑洞
				// 中继把整段预算吃干。
				if share := time.Until(deadline) / 2; share < budget {
					budget = share
				}
			}
			if budget <= 0 {
				return ctx.Err()
			}
			attempted++
			attemptCtx, cancel := context.WithTimeout(ctx, budget)
			err := d.host.Connect(attemptCtx, peer.AddrInfo{ID: target, Addrs: []ma.Multiaddr{addr}})
			cancel()
			if err == nil {
				d.logf("中继路径可用：经 %s 连到 %s", candidate.ID.ShortString(), target.ShortString())
				return nil
			}
			lastErr = err
		}
	}
	if lastErr == nil {
		return fmt.Errorf("中继候选都拼不出可用承载地址")
	}
	return fmt.Errorf("尝试 %d 条中继路径均失败: %w", attempted, lastErr)
}
