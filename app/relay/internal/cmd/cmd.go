// =================================================================================
// 中继程序入口（gf 规范）：gcmd 根命令 + relay / relay-check 子命令。
// =================================================================================

package cmd

import (
	"context"

	"github.com/gogf/gf/v2/os/gcmd"
)

var (
	Main = gcmd.Command{
		Name:  "main",
		Usage: "main",
		Brief: "start Lanet relay (libp2p circuit relay v2)",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			// 未指定子命令时默认启动中继。
			runRelay(ctx)
			return nil
		},
	}
)

func init() {
	// gf 规范：子命令通过 AddCommand 挂载。
	_ = Main.AddCommand(&gcmd.Command{
		Name:  "check",
		Usage: "check",
		Brief: "验证中继 Reservation 可用性",
		Func: func(ctx context.Context, parser *gcmd.Parser) (err error) {
			runRelayCheck()
			return nil
		},
	})
}
