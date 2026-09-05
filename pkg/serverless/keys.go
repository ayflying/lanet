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

// PublicNetworkKey 公共网络密钥：Standalone 模式下 NetworkKey 留空时使用。
// 所有未设置网络密钥的节点都加入同一张公共 P2P 网络，互相可见可连接；
// 想私有组网请各自约定相同的 NetworkKey。
const PublicNetworkKey = "lanet/public"

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
// 网络密钥留空时按公共网络密钥处理；渠道留空时与历史版本派生一致。
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
