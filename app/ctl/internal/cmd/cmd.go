// =================================================================================
// 控制面入口（gf 规范）：logic 服务注册 + api 定义的路由绑定。
// =================================================================================

package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/ayflying/pvn/app/ctl/internal/controller"
	grouplogic "github.com/ayflying/pvn/app/ctl/internal/logic/group"
	relaylogic "github.com/ayflying/pvn/app/ctl/internal/logic/relaydir"
	"github.com/ayflying/pvn/app/ctl/internal/service"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/os/gcmd"
)

var (
	Main = gcmd.Command{
		Name:  "main",
		Usage: "main",
		Brief: "start Lanet control plane (PVN ctl)",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			registerServices(ctx)

			server := g.Server()
			// PVN_CTL_ADDR 允许部署时覆盖监听地址（如 127.0.0.1:18080），
			// 优先级高于 config.yaml 的 server.address。
			if addr := os.Getenv("PVN_CTL_ADDR"); addr != "" {
				server.SetAddr(addr)
			}
			server.Group("/", func(group *ghttp.RouterGroup) {
				group.Middleware(ghttp.MiddlewareHandlerResponse)
				ctl := controller.New()
				group.Bind(
					ctl.Health,
					ctl.Group,
					ctl.Relay,
				)
			})
			server.Run()
			return nil
		},
	}
)

// registerServices 初始化并注册 logic 层服务（gf 规范：logic 实现注册到 service 接口）。
// 群组注册表优先使用 PVN_CTL_DB 指定的 SQLite 文件（重启不丢数据）；
// 未设置时退化为纯内存模式（本地快速实验）。
func registerServices(ctx context.Context) {
	// 中继目录（内存版）。
	service.RegisterRelayDirectory(relaylogic.NewRelayDirectory())

	// 群组注册表。
	var (
		groupSvc *grouplogic.GroupService
		err      error
	)
	if dbPath := os.Getenv("PVN_CTL_DB"); dbPath != "" {
		groupSvc, err = grouplogic.NewPersistent(ctx, dbPath)
		if err != nil {
			panic(fmt.Sprintf("open group database %s: %v", dbPath, err))
		}
		g.Log().Infof(ctx, "group registry backed by SQLite: %s", dbPath)
	} else {
		groupSvc, err = grouplogic.New()
		if err != nil {
			panic(err)
		}
	}
	service.RegisterGroup(groupSvc)
}
