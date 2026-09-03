package main

import (
	"github.com/gogf/gf/v2/os/gctx"

	"github.com/ayflying/pvn/app/agent/internal/cmd"
)

func main() {
	cmd.Main.Run(gctx.GetInitCtx())
}
