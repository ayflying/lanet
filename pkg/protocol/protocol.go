// Package protocol 集中定义 lanet 的应用层协议 ID 及其「按群派生」规则。
//
// 这是依赖树的叶子包（只依赖 go-libp2p/core/protocol），serverless 与
// selfupdate 都可以安全引用，不会形成循环依赖。
//
// 0.5.33 私有协议加固：控制面协议（成员信息交换、删除好友通知、P2P 更新
// 分发、探测回显）的协议 ID 不再是全局固定值，而是由群组密钥派生出「群指纹」
// 段拼进 ID（GroupProtoID）。效果——
//   - 不知道网络密钥的节点构造不出正确的协议 ID，multistream 协商阶段即被拒，
//     根本进不到应用层 handler：零无效握手、零待审批/附近污染、零探测流量；
//   - 私有 DHT 前缀同样按群派生（见 serverless 侧），彻底结束「所有 lanet
//     部署共用一张全局路由网」带来的跨群路由表膨胀与查询中继流量。
//
// 数据面协议（Tunnel / PortFWD）保持全局固定 ID：它们承载的是虚拟 IP 数据包
// 与端口转发，入向已有防火墙 + 成员门把关，且必须允许同群「新老版本」直接互连
// （老版本只认固定 ID），不能被升级节奏绑死。
package protocol

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/libp2p/go-libp2p/core/protocol"
)

const (
	Tunnel  protocol.ID = "/pvn/tunnel/1.0.0"
	Probe   protocol.ID = "/pvn/probe/1.0.0"
	Control protocol.ID = "/pvn/ctrl/1.0.0"
	// PortFWD 端口转发：流上首条消息为 2 字节大端长度 + 目标地址
	// （"ip:port" 文本），之后为双向原始字节。接收端 net.Dial
	// 目标地址后做透明搬运。
	PortFWD protocol.ID = "/pvn/portfwd/1.0.0"
)

// 控制面协议的「基名」段。完整 ID = GroupProtoID(base, groupKey)。
// 派生时只用基名，避免历史全路径混入指纹计算。
const (
	BaseInfo        = "info"
	BaseUnfriend    = "unfriend"
	BaseEcho        = "echo"
	BaseUpdManifest = "update-manifest"
	BaseUpdFile     = "update-file"
)

// GroupFingerprint 群组指纹短串（8 位 hex，展示/日志/协议 ID 派生共用）。
// 与 serverless.GroupFingerprint 同算法，放这里是为了让叶子包自洽、
// serverless 反向复用而不引入额外依赖。
func GroupFingerprint(groupKey []byte) string {
	if len(groupKey) < 4 {
		return hex.EncodeToString(groupKey)
	}
	return hex.EncodeToString(groupKey[:4])
}

// GroupProtoID 由（基名, 群组密钥）派生带群指纹的私有协议 ID：
//
//	/lanet/<群指纹>/<基名>/1.0.0
//
// 例：/lanet/3f9a1c20/info/1.0.0。不同网络密钥 → 不同群指纹 → 不同协议 ID，
// 异群节点连协议都协商不上，从传输层即完成隔离。
func GroupProtoID(base string, groupKey []byte) protocol.ID {
	return protocol.ID("/lanet/" + GroupFingerprint(groupKey) + "/" + base + "/1.0.0")
}

// DHTNamespace 参与私有 DHT 前缀派生的域分隔。
const DHTNamespace = "lanet-private-dht-v1:"

// DHTPrefixFor 由群组密钥（serverless.GroupKey 的派生值，已含渠道与网络密钥）
// 派生私有 DHT 的协议前缀。
//
// kad-dht 会把前缀补全为 <prefix>/kad/1.0.0 作为其协议 ID。历史版本所有 lanet
// 网络共用固定前缀 /lanet（= /lanet/kad/1.0.0），相当于全世界所有部署挤进
// 同一张私有 DHT：A 网络节点要为 B 网络的路由/查询买单（路由表膨胀、替陌生人
// 应答 FindProvider）。派生后每个网络一张独立 DHT，跨群流量归零。
//
// 派生值域安全：前缀经 CID 编码，必须落在合法多链接命名空间。这里统一以
// "/lanet/" 为根再拼一段 8 位 hex 子标签，既与公共 DHT 的 /ipfs 隔离，
// 也保证任意密钥都得到稳定、合法、可复现的前缀。
func DHTPrefixFor(groupKey []byte) string {
	h := sha256.Sum256(append([]byte(DHTNamespace), groupKey...))
	return "/lanet/" + hex.EncodeToString(h[:4])
}
