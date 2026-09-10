// Package serverless 提供无控制面的群组成员发现：
//
//   - DHT（kad-dht，ModeAutoServer）：跨网段发现。每个节点把
//     「本网络 rendezvous key」作为 provider 记录发布到 DHT 网络
//     （默认公共 IPFS DHT），同网络成员通过 FindProviders 互相找到。
//     key 由网络密钥（NetworkKey）派生，不知道密钥就无法定位网络（弱隐私边界）。
//   - mDNS：局域网零配置发现（service tag 派生自网络密钥，同网络才互见）。
//   - 节点即服务端：每个节点默认运行 relay service 与 DHT server 模式，
//     公网可达的成员自然成为网络内的引导与中继节点。
//
// 发现到同网络成员后主动建连并交换信息（/lanet/info/1.0.0），
// 本地维护成员表；对外实现 tunnel.GroupNetMap（按虚拟 IP 解析）
// 与 tunnel.RelaySource（中继候选），SDK 的 Dial/OnStream 语义不变。
package serverless

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// PublicNetworkKey 历史公共网络密钥：早期版本 NetworkKey 留空时使用的固定值。
// 保留仅为兼容老网络派生（GroupKey(channel, "") 的历史语义）与迁移识别，
// 新部署的「留空」不再使用此值——见 DeriveDefaultNetworkKey。
const PublicNetworkKey = "lanet/public"

// DefaultNetworkKeyPrefix 自动派生的默认网络密钥前缀。
// 留空密钥时使用，保证「每个部署默认自成一张网」：不填密钥 = 不与他人
// 自动同网（避免所有零配置节点挤在一张超大网络里，导致发现流量与
// 成员表膨胀）；要互通必须显式设置相同密钥。
const DefaultNetworkKeyPrefix = "lanet/auto/"

// DeriveDefaultNetworkKey 按节点身份（PeerID）派生本机专属默认网络密钥。
//
// 设计意图（为什么不是固定的 lanet/public）：
//   - 固定值会让所有「没填密钥」的节点落到同一张网，网内节点越多，
//     DHT 发现、成员表、探测流量越大（实测过全局同网时的流量与幽灵成员问题）；
//   - 按 PeerID 派生后，每台机器开箱即用且天然独立，零配置不再等于
//     「和全世界同网」；想互通需显式约定相同 NetworkKey（或互发连接种子）。
//
// 稳定性：PeerID 由 node.key（持久化身份文件）派生，重启/升级不变；
// 更换身份文件即更换默认网络，语义上等于换了一个身份，符合预期。
func DeriveDefaultNetworkKey(peerID string) string {
	if peerID == "" {
		return PublicNetworkKey // 极端兜底：拿不到身份时退回历史默认值
	}
	h := sha256.Sum256([]byte("lanet-default-network-v1:" + peerID))
	return DefaultNetworkKeyPrefix + hex.EncodeToString(h[:8])
}

// 分发渠道（Channel）：参与群组密钥派生，用于把不同分发途径的程序
// 隔离在不同的网络里——即使双方使用完全相同的 NetworkKey 也不互通
// （DHT rendezvous、mDNS 标签、虚拟 IP 派生全部随群组密钥隔离）。
const (
	// ChannelOfficial 官方发行渠道：官方打包发布的程序（pvn-node 等）。
	// 派生时使用空渠道前缀，与历史版本派生结果完全一致（老网络零迁移）。
	ChannelOfficial = ""
	// ChannelSDK 第三方 SDK 渠道：通过 sdk/go/lanet（及各语言 SDK 封装）
	// 构建的程序默认归属此渠道，与官方渠道网络互相隔离。
	ChannelSDK = "sdk"
)

// GroupKey 由（渠道, 网络密钥）派生群组密钥（32 字节）。
// 网络密钥留空时按历史公共网络密钥处理（仅供内部兜底调用；
// 正常路径下 New() 已把留空替换为 DeriveDefaultNetworkKey 的派生值）；
// 渠道留空时与历史版本派生一致。
func GroupKey(channel, networkKey string) []byte {
	if networkKey == "" {
		networkKey = PublicNetworkKey
	}
	msg := "lanet-group-v1:" + channel
	if channel != "" {
		msg += ":"
	}
	h := sha256.Sum256([]byte(msg + networkKey))
	return h[:]
}

// RendezvousKey DHT provider 记录的键（含群指纹，不含明文群名）。
func RendezvousKey(groupKey []byte) string {
	return "/lanet/group/" + hex.EncodeToString(groupKey[:12])
}

// MdnsTag 局域网 mDNS service tag（同网络节点才互相可见）。
func MdnsTag(groupKey []byte) string {
	return "_lanet-" + hex.EncodeToString(groupKey[:4])
}

// DeriveVirtualIP 无控制面模式下按（群密钥, PeerID）确定性派生虚拟 IP。
// 地址空间 10.7.1.1 ~ 10.7.254.254（约 6.4 万），冲突概率随群规模缓慢上升，
// 冲突时表现为两个成员互相 Dial 打到对方（SDK NetMap 会同时列出，可人工发现）。
// 注意：不使用 100.64.0.0/10（CGNAT 段），避免与 Tailscale 等同类工具的虚拟网卡冲突。
func DeriveVirtualIP(groupKey []byte, peerID string) string {
	buf := make([]byte, 0, len(groupKey)+len(peerID))
	buf = append(buf, groupKey...)
	buf = append(buf, peerID...)
	h := sha256.Sum256(buf)
	return fmt.Sprintf("10.7.%d.%d", int(h[0])%254+1, int(h[1])%254+1)
}

// GroupFingerprint 群组指纹短串（展示/日志用，8 hex）。
func GroupFingerprint(groupKey []byte) string {
	return hex.EncodeToString(groupKey[:4])
}
