# Lanet Go SDK

把任意 Go 程序变成 Lanet 群组网格中的一个节点：十几行代码完成入网、开流、接流。
与内置 agent 使用同一套 libp2p 协议栈（v0.44），隧道策略一致：
**直连优先 → 已有连接开流 → Circuit Relay v2 中继兜底**。

## 安装

```bash
go get github.com/ayflying/pvn/sdk/go/lanet
```

前置条件：已部署控制面（ctl）与中继（relay）。SDK 只需能访问 ctl（`CTLURL`），
relay 地址由 ctl 目录自动下发。无需 root/管理员权限（不创建 TUN 网卡）。

## 五分钟上手

### 场景 A：创建群组（本节点成为群主）

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
		CTLURL:    "http://ctl.example.com:8600", // 控制面地址（必填）
		Name:      "my-service",                  // 节点名称（必填）
		GroupName: "my-group",                    // 群组名称
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	info := client.Info()
	log.Printf("入网成功：虚拟 IP=%s，邀请码=%s", info.VirtualIP, info.InviteCode)
	// 把 info.InviteCode 分享给其他成员

	// 周期任务（刷新 NetMap / 通告地址 / 中继预约），阻塞直到 ctx 取消。
	client.Run(ctx)
}
```

### 场景 B：凭邀请码加入群组

```go
client, err := lanet.New(ctx, lanet.Config{
	CTLURL:     "http://ctl.example.com:8600",
	Name:       "another-node",
	InviteCode: "grp-xxxxxxxx", // 群主分享的邀请码；留空则创建新群组
})
```

### 场景 C：主动开流（echo 请求-响应）

```go
stream, viaRelay, err := client.Dial(ctx, "100.64.0.2")
if err != nil {
	log.Fatal(err)
}
defer stream.Close()

// 发送完毕调用 CloseWrite()，对端才能读到 EOF（半关闭语义，务必调用）。
_, _ = stream.Write([]byte("hello"))
_ = stream.CloseWrite()

// 读回程数据直到 EOF。
reply, _ := io.ReadAll(stream)
log.Printf("回复: %s (viaRelay=%v)", reply, viaRelay)
```

### 场景 D：接收入向流（提供 echo 服务）

```go
client.OnStream(func(stream lanet.Stream) {
	defer stream.Close()
	// 注意：对端 CloseWrite 后本端读到 EOF，此时再回写仍可送达。
	_, _ = io.Copy(stream, stream)
})
```

## Config 配置项

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `CTLURL` | string | —（必填） | 控制面地址，如 `http://127.0.0.1:8600` |
| `Name` | string | —（必填） | 节点名称，显示在 NetMap 中 |
| `OS` | string | `runtime.GOOS` | OS 标识 |
| `InviteCode` | string | `""` | 凭码加入群组；**留空则创建新群组** |
| `GroupName` | string | `""` | 创建模式下的群组名称（Join 模式忽略） |
| `ListenAddrs` | []string | tcp + ws + quic 全部随机端口 | 覆盖默认监听地址 |
| `WebRTC` | *bool | `true` | 启用 webrtc-direct 传输（浏览器节点可直连本节点） |
| `NetMapInterval` | time.Duration | `15s` | 周期任务（NetMap 刷新/通告/中继预约）间隔 |
| `DialTimeout` | time.Duration | `8s` | 开流超时 |
| `Quiet` | bool | `false` | 为 true 时不打日志 |

## API 一览

| 方法 | 说明 |
|---|---|
| `New(ctx, Config)` | 创建节点并入网（创建或加入群组）；失败即返回错误 |
| `Info()` | 节点身份：PeerID / GroupID / Group / VirtualIP / InviteCode / Created |
| `OnStream(Handler)` | 注册入向流回调（协议 `/pvn/tunnel/1.0.0`，可注册多个，按序调用） |
| `Dial(ctx, virtualIP)` | 按虚拟 IP 开流，返回 `(Stream, viaRelay, error)` |
| `DialProtocol(ctx, virtualIP, protoID)` | 指定协议 ID 开流（自定义应用协议） |
| `DialPortFWD(ctx, PortFWDTarget)` | 桥接对端节点侧 TCP 服务，返回标准 `net.Conn` |
| `LastPathUsed(peerID)` | 到对端最近一次链路：`direct` / `relay` / `unknown` |
| `NetMap()` | 当前群组成员目录快照 |
| `Host()` | 底层 libp2p Host（进阶：注册自定义协议处理器） |
| `Run(ctx)` | 阻塞运行周期维护任务，直到 ctx 取消 |
| `Close()` | 关闭节点与底层连接 |

### Stream 接口

```go
type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error  // 半关闭写端：发送完毕必须调用，对端才能判 EOF
	Reset() error       // 强制中止流（异常场景，不等对端）
	ViaRelay() bool     // 本流是否经中继转发（false 即 P2P 直连）
	Protocol() string   // 流上的协议 ID
	RemotePeer() string // 对端 PeerID 文本
}
```

## 进阶用法

### PortFWD：访问对端节点的 TCP 服务

`DialPortFWD` 把「对端节点虚拟 IP + 端口」桥接为标准 `net.Conn`，
可用于远程桌面、数据库、任意 TCP 协议：

```go
conn, err := client.DialPortFWD(ctx, lanet.PortFWDTarget{
	VirtualIP: "100.64.0.2",
	Port:      3389, // 对端节点本机的远程桌面端口
})
if err != nil {
	log.Fatal(err)
}
defer conn.Close()
// conn 实现了标准 net.Conn，可直接交给 rdp 客户端 / net.Copy / ssh -L 等。
```

工作方式：本节点与目标节点建立 `/pvn/portfwd/1.0.0` 协议流，
目标节点收到后向 `ip:port` 发起 TCP 连接并双向搬运。
**目标为本节点虚拟 IP 时自动映射到 127.0.0.1**（SDK 节点无 TUN，
虚拟 IP 在本机 OS 上不可路由）；目标为其他地址时由目标节点代为拨出（可触达其内网）。

> PortFWD 入向处理器默认开启（`enablePortFWD`）。若要对外提供转发能力，
> 直接用 Go SDK 即可；完整 TUN 组网（ping 虚拟 IP、子网路由）请用内置 agent。

### 自定义协议

两端约定一个协议 ID，服务端用 `Host()` 注册处理器，客户端用 `DialProtocol` 开流：

```go
// 服务端
client.Host().SetStreamHandler("/myapp/1.0.0", func(s network.Stream) {
	defer s.Close()
	// ...
})

// 客户端
stream, viaRelay, err := client.DialProtocol(ctx, "100.64.0.2", "/myapp/1.0.0")
```

### Run / Close 生命周期

- `New` 返回即已入网（群组创建/加入完成），可立即 `Dial`；
- `Run(ctx)` 负责周期通告地址、刷新 NetMap、续期中继预约——**必须有人调用**，
  否则中继兜底链路会失效（直连不受影响）。典型写法：主 goroutine 里 `client.Run(ctx)`，
  业务逻辑在其他 goroutine；或 `go client.Run(ctx)` 后主逻辑自己阻塞；
- `Close()` 释放 libp2p Host；`ctx` 取消时 `Run` 返回，随后 `defer client.Close()`。

## 常见问题

**Q：`Dial` 返回 `viaRelay=true` 是坏事吗？**
不是。说明直连打洞失败，流量经 relay 转发，功能等价、延迟略高。
可用 `LastPathUsed(peerID)` 观测链路类型。

**Q：为什么写完数据对端读不到 EOF？**
必须调用 `stream.CloseWrite()`。libp2p 流没有「发送完毕」的隐式信号，
半关闭是唯一的 EOF 语义（Windows 对端尤其依赖此判断）。

**Q：无效邀请码 / ctl 不可达会怎样？**
`New` 直接返回错误，节点不会进入半入网状态。

**Q：portfwd 转发后回程数据被掐断？**
已在 SDK 内处理（双向均结束才关闭连接）。请确保使用当前版本。

## 实测记录

- 2026-09-04：三机真机联测（2×Linux + Windows），6 方向直连 + relay 兜底 + 边界场景 + 重启恢复全过；
- `go test ./...` + `go run ./app/agent/cmd/pvn-e2e-check`（端到端本地验证）持续绿。

## 能力边界

- SDK 节点无 TUN：不能 ping 虚拟 IP、不能当子网路由器（这些用内置 agent）；
- 流是消息流不是字节流：libp2p 不保证消息边界，跨流传协议请自行分帧
  （长度前缀 / 分隔符）；PortFWD 桥接的 TCP 连接无此限制；
- 浏览器接入用 [Web SDK](../web/README.md)，小程序/Unity 用网关 SDK（见 [总览](../README.md)）。
