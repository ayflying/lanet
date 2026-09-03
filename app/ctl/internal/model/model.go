// =================================================================================
// 控制面共享数据模型：api 层与 logic 层之间的视图结构。
// 注意：api 包引用本包，因此本包不得反向引用 api 包。
// =================================================================================

package model

import (
	"time"
)

// NodeView 成员节点视图（创建/加入时返回给调用方）。
type NodeView struct {
	PeerID     string    `json:"peer_id"`
	Name       string    `json:"name"`
	OS         string    `json:"os"`
	VirtualIP  string    `json:"virtual_ip"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

// MemberView NetMap 中的成员视图。
type MemberView struct {
	PeerID    string   `json:"peer_id"`
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	VirtualIP string   `json:"virtual_ip"`
	Role      string   `json:"role"`
	Addrs     []string `json:"addrs"`
}

// RelayCandidate 中继候选。
type RelayCandidate struct {
	PeerID       string    `json:"peer_id"`
	Addrs        []string  `json:"addrs"`
	Region       string    `json:"region"`
	Score        int       `json:"score"`
	CircuitCount int       `json:"circuit_count"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}
