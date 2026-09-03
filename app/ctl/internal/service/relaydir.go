// =================================================================================
// 中继目录业务接口定义。
// =================================================================================

package service

import (
	"context"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/model"
)

type IRelayDirectory interface {
	// Register 中继注册（重复注册覆盖更新）。
	Register(ctx context.Context, registration RelayRegistration) error
	// Heartbeat 中继心跳。
	Heartbeat(ctx context.Context, heartbeat RelayHeartbeat) error
	// List 按评分排序返回前 limit 个候选。
	List(ctx context.Context, limit int) ([]model.RelayCandidate, error)
	// UpdatedAt 候选目录最近更新时间。
	UpdatedAt() time.Time
}

// RelayRegistration 中继注册参数。
type RelayRegistration struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
	Region string   `json:"region"`
	Score  int      `json:"score"`
}

// RelayHeartbeat 中继心跳参数。
type RelayHeartbeat struct {
	PeerID       string `json:"peer_id"`
	CircuitCount int    `json:"circuit_count"`
	Score        int    `json:"score"`
}

// 本地注册的接口实例。
var (
	localRelayDirectory IRelayDirectory
)

// RegisterRelayDirectory 注册中继目录服务实例（ctl 启动时调用一次）。
func RegisterRelayDirectory(svc IRelayDirectory) {
	localRelayDirectory = svc
}

// RelayDirectory 返回已注册的中继目录服务实例。
func RelayDirectory() IRelayDirectory {
	return localRelayDirectory
}
