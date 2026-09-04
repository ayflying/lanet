// pvn-web-echo：起一个「网页可直连」的 Go SDK echo 节点，用于 Web SDK 浏览器联测。
// 用法：go run ./app/agent/cmd/pvn-web-echo -ctl http://127.0.0.1:8000 -invite grp-xxx
// 行为：入网后接收 /pvn/tunnel/1.0.0 入向流并回显（echo），打印链路类型。
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	ctlURL := flag.String("ctl", "http://127.0.0.1:8000", "控制面地址")
	invite := flag.String("invite", "", "邀请码（空则创建新群组并打印）")
	name := flag.String("name", "go-echo", "节点名称")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := lanet.New(ctx, lanet.Config{
		CTLURL: *ctlURL, Name: *name, InviteCode: *invite, GroupName: "web-demo-group",
	})
	if err != nil {
		log.Fatalf("入网失败: %v", err)
	}
	defer client.Close()

	info := client.Info()
	log.Printf("入网成功: 虚拟IP=%s 群组=%s", info.VirtualIP, info.Group)
	if info.Created {
		log.Printf(">>> 邀请码（供网页 demo 使用）: %s", info.InviteCode)
	}

	client.OnStream(func(stream lanet.Stream) {
		log.Printf("收到入向流: protocol=%s viaRelay=%v", stream.Protocol(), stream.ViaRelay())
		n, err := io.Copy(stream, stream) // echo
		log.Printf("echo 完成: %d bytes, err=%v", n, err)
		_ = stream.CloseWrite()
		_ = stream.Close()
	})

	// 周期任务（NetMap 刷新 / 地址通告 / 中继预约）放后台，主协程阻塞等退出。
	go client.Run(ctx)

	log.Printf("echo 节点就绪，等待网页节点连接（Ctrl+C 退出）")
	<-ctx.Done()
}
