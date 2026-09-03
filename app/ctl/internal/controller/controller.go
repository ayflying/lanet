// =================================================================================
// 控制器注册：按 gf 规范聚合全部 v1 控制器。
// =================================================================================

package controller

import (
	groupV1 "github.com/ayflying/pvn/app/ctl/internal/controller/group"
	healthV1 "github.com/ayflying/pvn/app/ctl/internal/controller/health"
	relayV1 "github.com/ayflying/pvn/app/ctl/internal/controller/relay"
)

// 路由绑定使用的控制器实例。
type Controller struct {
	Group   *groupV1.ControllerV1
	Health  *healthV1.ControllerV1
	Relay   *relayV1.ControllerV1
}

// New 构造控制器集合。
func New() *Controller {
	return &Controller{
		Group:  &groupV1.ControllerV1{},
		Health: &healthV1.ControllerV1{},
		Relay:  &relayV1.ControllerV1{},
	}
}
