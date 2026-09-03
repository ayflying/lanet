// =================================================================================
// 健康检查控制器。
// =================================================================================

package health

import (
	"context"

	v1 "github.com/ayflying/pvn/app/ctl/api/health/v1"
)

type ControllerV1 struct{}

func NewV1() v1.IHealthV1 {
	return &ControllerV1{}
}

// Healthz 服务健康检查。
func (c *ControllerV1) Healthz(ctx context.Context, req *v1.HealthzReq) (res *v1.HealthzRes, err error) {
	res = &v1.HealthzRes{
		Status:  "ok",
		Service: "pvn-ctl",
	}
	return res, nil
}
