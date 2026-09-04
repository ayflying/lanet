// pvn-gateway ws-gateway 服务：让 C# / Unity / 小程序等
// 无法运行 libp2p 的客户端经 WebSocket 帧协议接入群组网格。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ayflying/pvn/app/gateway/internal/gateway"
)

func main() {
	ctl := flag.String("ctl", "http://127.0.0.1:8000", "控制面地址")
	invite := flag.String("invite", "", "网关加入的群组邀请码（为空则创建新群组）")
	groupName := flag.String("group-name", "gateway-group", "创建模式下的群组名")
	listen := flag.String("listen", ":8700", "WS 监听地址")
	name := flag.String("name", "gateway", "网关节点名称")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := gateway.Run(ctx, gateway.Config{
		CTLURL:     *ctl,
		InviteCode: *invite,
		GroupName:  *groupName,
		ListenAddr: *listen,
		Name:       *name,
	})
	if err != nil {
		log.Printf("[gateway] 退出: %v", err)
		return
	}
	if info := client.Info(); *invite == "" || info.Created {
		fmt.Printf("网关群组邀请码: %s（客户端鉴权用）\n", info.InviteCode)
	}
	<-ctx.Done()
	_ = client.Close()
	log.Printf("[gateway] 已退出")
}
