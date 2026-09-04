# Lanet Go SDK

把任意 Go 程序变成 Lanet 群组网格中的一个节点：十几行代码完成入网、开流、接流。

## 安装

```bash
go get github.com/ayflying/pvn/sdk/go/lanet
```

## 快速开始

### 创建群组（本节点成为群主）

```go
package main

import (
	"context"
	"log"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := lanet.New(ctx, lanet.Config{
		CTLURL:    "http://ctl.example.com:8600", // 控制面地址
		Name:      "my-service",                  // 节点名称
		GroupName: "my-group",                    // 群组名称
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	info := client.Info()
	log.Printf("入网成功：虚拟 IP=%s，邀请码=%s", info.VirtualIP, info.InviteCode)

	// 周期任务（刷新 NetMap / 通告地址 / 中继预约），阻塞直到 ctx 取消。
	client.Run(ctx)
}
```

### 凭邀请码加入群组

```go
client, err := lanet.New(ctx, lanet.Config{
	CTLURL:     "http://ctl.example.com:8600",
	Name:       "another-node",
	InviteCode: "grp-xxxxxxxx", // 群主分享的邀请码
})
```

### 主动开流（按虚拟 IP）

```go
stream, viaRelay, err := client.Dial(ctx, "100.64.0.2")
if err != nil {
	log.Fatal(err)
}
defer stream.Close()

// 发送完毕调用 CloseWrite()，对端才能读到 EOF（半关闭语义）。
_, _ = stream.Write([]byte("hello"))
_ = stream.CloseWrite()

log.Printf("链路类型: %v", map[bool]string{true: "relay", false: "direct"}[viaRelay])
```

### 接收入向流

```go
client.OnStream(func(stream lanet.Stream) {
	defer stream.Close()
	// 示例：echo 服务
	_, _ = io.Copy(stream, stream)
})
```

## API 一览

| 方法 | 说明 |
|---|---|
| `New(ctx, Config)` | 创建节点并入网（创建或加入群组） |
| `Info()` | 节点身份：PeerID / 群组 / 虚拟 IP / 邀请码 |
| `OnStream(Handler)` | 注册入向流回调（可多个） |
| `Dial(ctx, virtualIP)` | 按虚拟 IP 开流，返回 `(Stream, viaRelay, error)` |
| `LastPathUsed(peerID)` | 到对端最近一次链路：direct / relay / unknown |
| `NetMap()` | 当前群组成员目录快照 |
| `Host()` | 底层 libp2p Host（进阶：自定义协议） |
| `Run(ctx)` | 阻塞运行周期维护任务 |
| `Close()` | 关闭节点 |

## 能力边界

- 隧道策略与内置 agent 一致：**直连优先 → 已有连接开流 → 中继兜底**。
- `Stream` 是 libp2p 流式语义（不是 TCP socket）：没有 `Dial(ip:port)`，
  应用层协议由业务自行约定；跨节点转发 TCP 请在 Go 节点侧自行桥接
  （后续版本计划提供 portfwd 协议）。
- 浏览器/网页接入不使用本包，请等待 Web SDK（信令 + WebRTC，roadmap 阶段 2）。

## 运行要求

- 控制面（ctl）与中继（relay）已部署，SDK 通过 `CTLURL` 访问控制面。
- 无需 root/管理员权限（不创建 TUN 网卡；完整 TUN 能力请用内置 agent）。
