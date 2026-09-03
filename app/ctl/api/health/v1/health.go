// =================================================================================
// 健康检查 API 定义。
// =================================================================================

package v1

import (
	"context"

	"github.com/gogf/gf/v2/frame/g"
)

type IHealthV1 interface {
	Healthz(ctx context.Context, req *HealthzReq) (res *HealthzRes, err error)
}

// HealthzReq 健康检查请求。
type HealthzReq struct {
	g.Meta `path:"/healthz" method:"get" tags:"Health" summary:"服务健康检查"`
}

// HealthzRes 健康检查响应。
type HealthzRes struct {
	Status  string `json:"status" dc:"健康状态"`
	Service string `json:"service" dc:"服务标识"`
}
