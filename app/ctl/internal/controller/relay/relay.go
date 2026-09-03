// =================================================================================
// 中继目录控制器：实现 api/relay/v1.IRelayV1。
// =================================================================================

package relay

import (
	"context"

	v1 "github.com/ayflying/pvn/app/ctl/api/relay/v1"
	"github.com/ayflying/pvn/app/ctl/internal/service"
)

type ControllerV1 struct{}

func NewV1() v1.IRelayV1 {
	return &ControllerV1{}
}

// Register 中继注册。
func (c *ControllerV1) Register(ctx context.Context, req *v1.RegisterReq) (res *v1.RegisterRes, err error) {
	if err = service.RelayDirectory().Register(ctx, service.RelayRegistration{
		PeerID: req.PeerID,
		Addrs:  req.Addrs,
		Region: req.Region,
		Score:  req.Score,
	}); err != nil {
		return nil, err
	}
	return &v1.RegisterRes{Status: "registered"}, nil
}

// Heartbeat 中继心跳。
func (c *ControllerV1) Heartbeat(ctx context.Context, req *v1.HeartbeatReq) (res *v1.HeartbeatRes, err error) {
	if err = service.RelayDirectory().Heartbeat(ctx, service.RelayHeartbeat{
		PeerID:       req.PeerID,
		CircuitCount: req.CircuitCount,
		Score:        req.Score,
	}); err != nil {
		return nil, err
	}
	return &v1.HeartbeatRes{Status: "healthy"}, nil
}

// Candidates 查询中继候选。
func (c *ControllerV1) Candidates(ctx context.Context, req *v1.CandidatesReq) (res *v1.CandidatesRes, err error) {
	candidates, err := service.RelayDirectory().List(ctx, req.Limit)
	if err != nil {
		return nil, err
	}
	return &v1.CandidatesRes{
		Candidates: candidates,
		UpdatedAt:  service.RelayDirectory().UpdatedAt(),
	}, nil
}
