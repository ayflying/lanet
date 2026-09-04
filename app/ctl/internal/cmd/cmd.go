// =================================================================================
// 控制面入口（gf 规范）：logic 服务注册 + api 定义的路由绑定。
// =================================================================================

package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

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
				// CORS：网页 SDK 从浏览器直连 ctl（join/netmap/announce/relays）。
				group.Middleware(func(r *ghttp.Request) {
					r.Response.CORSDefault()
					r.Middleware.Next()
				})
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

func init() {
	// 数据库运维子命令：数据库路径统一取 PVN_CTL_DB（缺省 ./lanet.db）。
	dbPath := func() string {
		if p := os.Getenv("PVN_CTL_DB"); p != "" {
			return p
		}
		return "./lanet.db"
	}

	// migrate：显式跑 schema 迁移并打印版本（容器启动前执行亦可）。
	_ = Main.AddCommand(&gcmd.Command{
		Name:  "migrate",
		Usage: "migrate",
		Brief: "运行数据库 schema 迁移并打印当前版本",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			path := dbPath()
			st, err := grouplogic.OpenStoreRaw(path)
			if err != nil {
				return err
			}
			defer st.Close()
			before, err := st.Version(ctx)
			if err != nil {
				return err
			}
			after, err := st.Migrate(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("db=%s version %d -> %d\n", path, before, after)
			return nil
		},
	})

	// version：只读检查当前迁移版本。
	_ = Main.AddCommand(&gcmd.Command{
		Name:  "dbversion",
		Usage: "dbversion",
		Brief: "查看数据库当前迁移版本",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			path := dbPath()
			st, err := grouplogic.OpenStoreRaw(path)
			if err != nil {
				return err
			}
			defer st.Close()
			v, err := st.Version(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("db=%s version=%d\n", path, v)
			return nil
		},
	})

	// repair：损坏/异常库的自动修复（先备份，能救数据就救）。
	_ = Main.AddCommand(&gcmd.Command{
		Name:  "repair",
		Usage: "repair",
		Brief: "备份并修复 SQLite 数据库（损坏自动重建，可救数据优先救）",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			path := dbPath()
			result, err := grouplogic.RepairDatabase(ctx, path)
			if err != nil {
				return err
			}
			fmt.Printf("repair done: db=%s rebuilt=%v backup=%s groups=%d members=%d addrs=%d skipped=%d\n",
				path, result.Rebuilt, result.BackupPath, result.Groups, result.Members, result.Addrs, result.Skipped)
			return nil
		},
	})

	// backup：一致性备份（VACUUM INTO），参数为目标文件路径。
	_ = Main.AddCommand(&gcmd.Command{
		Name:  "backup",
		Usage: "backup <dest>",
		Brief: "对数据库做一致性备份（VACUUM INTO，含 WAL 最新状态）",
		Arguments: []gcmd.Argument{
			{Name: "dest", Brief: "备份目标文件路径（缺省自动带时间戳）", IsArg: true},
		},
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			// gf Func 路径下位置参数索引：[0]=二进制名、[1]=子命令名、[2]=第一个位置参数。
			dest := parser.GetArg(2, "").String()
			if dest == "" {
				dest = fmt.Sprintf("%s.backup-%s", dbPath(), time.Now().Format("20060102-150405.000000000"))
			}
			path := dbPath()
			st, err := grouplogic.OpenStoreRaw(path)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.SnapshotBackup(ctx, dest); err != nil {
				return err
			}
			fmt.Printf("backup done: %s -> %s\n", path, dest)
			return nil
		},
	})
}

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
