package main

import (
	"log"

	"github.com/gogf/gf/v2/os/gctx"

	"github.com/ayflying/pvn/app/ctl/internal/cmd"
)

// version 由 CI 经 -ldflags "-X main.version=<VERSION 文件内容>" 注入。
var version = "dev"

func main() {
	log.Printf("[ctl] lanet-ctl version=%s", version)
	cmd.Main.Run(gctx.GetInitCtx())
}
