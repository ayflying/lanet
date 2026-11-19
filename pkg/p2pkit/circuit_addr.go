package p2pkit

import (
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	// maxCircuitUnderlayScan 单个候选最多扫描多少条承载地址。
	// 一台装了 WSL / VMware 的机器会通告几十条（多网卡 × 多端口 × 多传输），
	// 全试一遍会把拨号预算耗光，还得不到更多成功率。
	maxCircuitUnderlayScan = 12
	// maxCircuitUnderlayBytes 单条承载地址的长度上限：超过它的基本是畸形地址。
	maxCircuitUnderlayBytes = 2048
)

// CircuitAddrs 拼出「经 candidate 这个中继访问 target」的 circuit 路径地址。
//
// 这是中继兜底拨号的唯一地址构造入口，供数据面（pkg/tunnel）与控制面
// （pkg/serverless 成员握手）共用：两处如果各写一份，中继地址格式一变就会
// 出现「一条路能通、另一条路永远通不了」的半死状态。
//
// 只接受能承载 libp2p 的 underlay 地址，四类一律跳过：
//   - 已带 /p2p-circuit：经中继再预约中继会成环，永远拨不通；
//   - lanet overlay（10.7.0.0/16）：那是隧道内的虚拟地址，拨它会递归拨隧道；
//   - 空地址 / 超长地址：畸形输入，交给拨号侧只会白等；
//   - /p2p/<别人> 结尾且与候选不符：地址里的节点不是这个候选，拼出来必然错。
//
// 每条候选最多返回 maxAddrs 条（调用方一般传 2）：候选的中继之间可以切换，
// 但同一个中继上试太多地址没有收益，只会拖长单轮恢复时间。
func CircuitAddrs(candidate peer.AddrInfo, target peer.ID, maxAddrs int) []ma.Multiaddr {
	if candidate.ID == "" || target == "" || candidate.ID == target || maxAddrs <= 0 {
		return nil
	}
	out := make([]ma.Multiaddr, 0, maxAddrs)
	for i, addr := range candidate.Addrs {
		if i >= maxCircuitUnderlayScan || len(out) >= maxAddrs {
			break
		}
		if addr == nil || len(addr.Bytes()) > maxCircuitUnderlayBytes {
			continue
		}
		if isCircuitAddr(addr) || IsLanetOverlayAddr(addr) {
			continue
		}
		base := addr
		// 承载地址自带 /p2p/<候选> 时剥掉再拼，保证最终路径里的中继身份唯一。
		if _, err := base.ValueForProtocol(ma.P_P2P); err == nil {
			ai, err := peer.AddrInfoFromP2pAddr(base)
			if err != nil || ai.ID != candidate.ID || len(ai.Addrs) == 0 {
				continue
			}
			base = ai.Addrs[0]
		}
		out = append(out, base.
			Encapsulate(ma.StringCast("/p2p/"+candidate.ID.String())).
			Encapsulate(ma.StringCast("/p2p-circuit/p2p/"+target.String())))
	}
	return out
}
