// Package serverless 提供无控制面的群组成员发现：
//
//   - DHT（kad-dht，ModeAutoServer）：跨网段发现。每个节点把
//     「本群 rendezvous key」作为 provider 记录发布到 DHT 网络
//     （默认公共 IPFS DHT），同群成员通过 FindProviders 互相找到。
//     key 由邀请码派生，不知道邀请码就无法定位群组（弱隐私边界）。
//   - mDNS：局域网零配置发现（service tag 派生自群密钥，同群才互见）。
//   - 节点即服务端：每个节点默认运行 relay service 与 DHT server 模式，
//     公网可达的成员自然成为群内的引导与中继节点。
//
// 发现到同群成员后主动建连并交换信息（/lanet/info/1.0.0），
// 本地维护成员表；对外实现 tunnel.GroupNetMap（按虚拟 IP 解析）
// 与 tunnel.RelaySource（中继候选），SDK 的 Dial/OnStream 语义不变。
package serverless

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// GroupKey 由邀请码派生群组密钥（32 字节）。
func GroupKey(inviteCode string) []byte {
	h := sha256.Sum256([]byte("lanet-group-v1:" + inviteCode))
	return h[:]
}

// RendezvousKey DHT provider 记录的键（含群指纹，不含明文群名）。
func RendezvousKey(groupKey []byte) string {
	return "/lanet/group/" + hex.EncodeToString(groupKey[:12])
}

// MdnsTag 局域网 mDNS service tag（同群节点才互相可见）。
func MdnsTag(groupKey []byte) string {
	return "_lanet-" + hex.EncodeToString(groupKey[:4])
}

// DeriveVirtualIP 无控制面模式下按（群密钥, PeerID）确定性派生虚拟 IP。
// 地址空间 100.64.1.1 ~ 100.64.254.254（约 6.4 万），冲突概率随群规模缓慢上升，
// 冲突时表现为两个成员互相 Dial 打到对方（SDK NetMap 会同时列出，可人工发现）。
func DeriveVirtualIP(groupKey []byte, peerID string) string {
	buf := make([]byte, 0, len(groupKey)+len(peerID))
	buf = append(buf, groupKey...)
	buf = append(buf, peerID...)
	h := sha256.Sum256(buf)
	return fmt.Sprintf("100.64.%d.%d", int(h[0])%254+1, int(h[1])%254+1)
}

// GroupFingerprint 群组指纹短串（展示/日志用，8 hex）。
func GroupFingerprint(groupKey []byte) string {
	return hex.EncodeToString(groupKey[:4])
}
