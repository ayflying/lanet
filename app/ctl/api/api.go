// =================================================================================
// 控制面 API 定义聚合（gf 规范：api 层仅声明路由与出入参，不含业务逻辑）。
// =================================================================================

package api

import (
	groupv1 "github.com/ayflying/pvn/app/ctl/api/group/v1"
	healthv1 "github.com/ayflying/pvn/app/ctl/api/health/v1"
	relayv1 "github.com/ayflying/pvn/app/ctl/api/relay/v1"
)

type (
	IGroupV1  = groupv1.IGroupV1
	IHealthV1 = healthv1.IHealthV1
	IRelayV1  = relayv1.IRelayV1
)
