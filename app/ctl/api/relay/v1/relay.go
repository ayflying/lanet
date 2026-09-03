// =================================================================================
// 中继目录 API 版本 v1 定义。
// =================================================================================

package v1

import (
	"context"
	"time"

	"github.com/ayflying/pvn/app/ctl/internal/model"
	"github.com/gogf/gf/v2/frame/g"
)

type IRelayV1 interface {
	Register(ctx context.Context, req *RegisterReq) (res *RegisterRes, err error)
	Heartbeat(ctx context.Context, req *HeartbeatReq) (res *HeartbeatRes, err error)
	Candidates(ctx context.Context, req *CandidatesReq) (res *CandidatesRes, err error)
}

// RegisterReq 中继注册。
type RegisterReq struct {
	g.Meta `path:"/v1/relays/register" method:"post" tags:"Relay" summary:"中继节点注册"`
	PeerID string   `json:"peer_id" v:"required" dc:"中继 PeerID"`
	Addrs  []string `json:"addrs" v:"required" dc:"中继可达地址"`
	Region string   `json:"region" dc:"地域标识"`
	Score  int      `json:"score" dc:"初始评分"`
}

// RegisterRes 注册响应。
type RegisterRes struct {
	Status string `json:"status"`
}

// HeartbeatReq 中继心跳。
type HeartbeatReq struct {
	g.Meta       `path:"/v1/relays/heartbeat" method:"post" tags:"Relay" summary:"中继节点心跳"`
	PeerID       string `json:"peer_id" v:"required" dc:"中继 PeerID"`
	CircuitCount int    `json:"circuit_count" dc:"当前承载的 circuit 数"`
	Score        int    `json:"score" dc:"评分"`
}

// HeartbeatRes 心跳响应。
type HeartbeatRes struct {
	Status string `json:"status"`
}

// CandidatesReq 查询中继候选列表。
type CandidatesReq struct {
	g.Meta `path:"/v1/relays/candidates" method:"get" tags:"Relay" summary:"查询中继候选"`
	Limit  int    `json:"limit" d:"2" v:"min:1|max:10" dc:"返回数量（1-10）"`
}

// CandidatesRes 候选列表响应。
type CandidatesRes struct {
	Candidates []model.RelayCandidate `json:"candidates"`
	UpdatedAt  time.Time              `json:"updated_at"`
}
