# Lanet.Sdk — C# SDK

Lanet ws-gateway 客户端：让 **Unity / .NET 8 / MAUI** 接入 Lanet 群组网格。
零外部依赖（基于 `System.Net.WebSockets.ClientWebSocket`），双目标打包
`netstandard2.1`（Unity 2021+）+ `net8.0`，`LangVersion 9`。

## 接入方式说明

C# 环境无法运行 libp2p 协议栈（无 WebRTC/WebTransport），因此经 **ws-gateway** 接入：

```
C# 客户端 ──(WebSocket 帧协议 :8700)──→ ws-gateway ──(libp2p 隧道)──→ 网格内目标节点
```

- 网关是一个以 Go SDK 节点身份入群的进程，数据实时转发、不落盘；
- 客户端与网关之间用统一二进制帧协议（Go/JS/C# 三端字节级一致，见文末）；
- 拿到的虚拟 IP 挂在网关名下——网格内其他成员看到的是「网关节点 + 若干客户端」。

## 能力边界

- ✅ 按虚拟 IP 开流、访问网格内成员的 TCP 服务（经网关 PortFWD）
- ✅ 自定义协议直开流（对端节点注册了对应协议处理器）
- ✅ service 模式接收入向流（网关同一时刻允许**一个** service 连接）
- ✅ 心跳保活（`PingAsync`，网关回 Pong）
- ❌ TUN 内核组网、ping 虚拟 IP（网关是转发节点，非端到端）

## 安装

- .NET 8 项目：引用 `sdk/csharp/Lanet.Sdk/Lanet.Sdk.csproj` 或打包后的 NuGet；
- Unity：拷贝 `Lanet.Sdk/bin/Release/netstandard2.1/Lanet.Sdk.dll` 到 `Assets/Plugins/`
  （或用 NuForUnity / 源码工程直接引用）；
- Unity WebGL 平台不支持 `ClientWebSocket`，其余平台（Android/iOS/桌面）可用。

## 快速开始

### client 模式：访问网格内服务

```csharp
using Lanet.Sdk;

// 1. 连接网关并完成邀请码鉴权（10s 超时）
var client = await LanetGatewayClient.ConnectAsync(new GatewayOptions
{
    Url = "ws://gateway.example.com:8700/gateway",   // 生产建议 wss://
    InviteCode = "XXXXXXXXXX",                        // 网关所属群组邀请码（网关启动日志打印）
    Name = "my-unity-game"
});
Console.WriteLine($"入网成功: 虚拟IP={client.Info.VirtualIP}, 群组={client.Info.Group}");

// 2. 访问网格内 TCP 服务（目标节点虚拟 IP + 其本机端口）
var stream = await client.DialAsync("100.64.0.3", 9999);
await stream.WriteStringAsync("hello");
await stream.CloseWriteAsync();                       // 对端读到 EOF（务必调用）
byte[] reply = await stream.ReadAllAsync();           // 读到对端 EOF 为止
Console.WriteLine(Encoding.UTF8.GetString(reply));
stream.Dispose();

// 3. 自定义协议流（对端节点注册了该协议处理器时）
var s2 = await client.DialProtocolAsync("100.64.0.3", "/myapp/1.0.0");

// 4. 心跳（长连接建议每 30s 一次）
await client.PingAsync();
```

### service 模式：接收入向流

```csharp
var service = await LanetGatewayClient.ConnectAsync(new GatewayOptions
{
    Url = "ws://gateway.example.com:8700/gateway",
    InviteCode = "XXXXXXXXXX",
    Name = "my-service",
    Mode = "service"                                  // 默认 client（主动开流）
});

service.OnStream += stream =>
{
    Console.WriteLine($"入向流: protocol={stream.Protocol}, remote={stream.RemotePeer}");
    // echo 示例
    stream.OnData(data => stream.WriteAsync(data));
};
service.Closed += () => Console.WriteLine("网关连接已关闭");
service.OnError  += ex => Console.WriteLine($"连接错误: {ex.Message}");
```

## API 参考

### `GatewayOptions`

| 属性 | 默认 | 说明 |
|---|---|---|
| `Url` | —（必填） | ws-gateway 地址，如 `ws://host:8700/gateway`（生产用 `wss://`） |
| `InviteCode` | —（必填） | 群组邀请码（网关启动日志可查） |
| `Name` | `"dotnet-client"` | 客户端名称 |
| `Mode` | `"client"` | `client`（主动开流）或 `service`（接收入向流） |

### `LanetGatewayClient`

| 成员 | 说明 |
|---|---|
| `static ConnectAsync(options, ct)` | 连接网关并鉴权（10s 超时）；失败抛异常 |
| `Info` / `GetInfo()` | 入网身份 `{VirtualIP, PeerID, Group, Mode}`（鉴权后有效） |
| `DialAsync(virtualIP, port, ct)` | PortFWD：桥接目标节点本机 TCP 服务（15s 超时） |
| `DialProtocolAsync(virtualIP, protoId, ct)` | 自定义协议开流（15s 超时） |
| `OnStream` | event，入向流到达（service 模式） |
| `Closed` | event，连接关闭（之后所有流报错） |
| `OnError` | event，连接级异常（接收循环中断等） |
| `PingAsync(ct)` | 发心跳（网关回 Pong） |
| `Dispose()` | 断开连接并中止所有流 |

### `GatewayStream`

| 成员 | 说明 |
|---|---|
| `StreamId` | 流 ID |
| `IsInbound` | 是否网格入向流（service 模式收到） |
| `ViaRelay` | 开流是否经中继（dial 成功后有效） |
| `Protocol` / `RemotePeer` | 协议 ID / 对端 PeerID（入向流有效） |
| `WriteAsync(byte[])` / `WriteStringAsync(string)` | 写数据 |
| `CloseWriteAsync()` | **半关闭写端**：对端读到 EOF；本端仍可继续读 |
| `ReadAsync(buffer, offset, count, ct)` | 读数据；返回 0 = 对端 EOF |
| `ReadAllAsync(ct)` | 便捷：读全部数据直到对端 EOF（适合请求-响应小数据） |
| `Abort()` / `Dispose()` | 强制中止流 |

### 典型读循环（流式场景）

```csharp
var buf = new byte[4096];
while (true)
{
    int n = await stream.ReadAsync(buf, 0, buf.Length, ct);
    if (n == 0) break;          // 对端 CloseWrite → EOF
    Handle(buf.AsSpan(0, n));   // 注意按应用协议分帧
}
```

线程安全：`WriteAsync` 内部有发送锁，可多线程并发调用；`ReadAsync` 请单线程消费。

## 服务端部署（ws-gateway）

```bash
# ctl + relay 已部署的前提下：
go run ./app/gateway/cmd/pvn-gateway \
    -ctl http://127.0.0.1:8000 \
    -listen :8700 \
    -invite <已有群组邀请码>      # 留空则创建新群组，启动日志打印邀请码

# 生产建议：
# 1. wss 反代（nginx/caddy 终结 TLS）→ 小程序/浏览器场景必须
# 2. 心跳：客户端每 30s PingAsync，网关侧空闲超时自动断开
```

网关以 Go SDK 身份入群：`service` 模式入向流、`client` 模式 dial 均由网关翻译为
libp2p 流操作；网关自身也注册了 PortFWD 入向处理器（目标为其虚拟 IP 时映射 127.0.0.1）。

## 联调验证

```bash
# 服务端：ctl + relay + gateway + go-service 节点 + TCP 回显 9999
dotnet run --project Lanet.Demo -- ws://127.0.0.1:8700/gateway <inviteCode> <targetVirtualIP>
```

实测记录（2026-09-04，本机）：鉴权 → 自定义协议 echo 往返 15ms →
PortFWD TCP 回显往返 25ms → 心跳，全链路 PASS。

## 帧协议（排查用）

一条二进制 WebSocket 消息即一个帧：

```
[type: 1 字节][streamID: 4 字节大端][payload 长度: 4 字节大端][payload]
```

| Type | 方向 | 含义 |
|---|---|---|
| `0x01` Auth | c→g | 鉴权 JSON `{"invite_code","name","mode"}` |
| `0x02/03` AuthOk/AuthErr | g→c | 鉴权结果 JSON |
| `0x04` Dial | c→g | 开流 JSON `{"ip","port","protocol"}`（port=PortFWD，protocol=自定义协议） |
| `0x05/06` DialOk/DialErr | g→c | 开流结果 |
| `0x07` Data | 双向 | 流数据 |
| `0x08` Close | 双向 | 半关闭写端（对端读到 EOF） |
| `0x09` Reset | 双向 | 强制中止流 |
| `0x0A/0B` Ping/Pong | c→g/g→c | 心跳 |
| `0x0C` StreamOpen | g→c | 网关推送入向流（service 模式），JSON `{"protocol","remote_peer"}` |

## 常见问题

**Q：DialAsync 报「开流超时」？**
目标虚拟 IP 不在群内 / 对端节点离线 / 目标端口未监听。先用 ctl NetMap 确认对端在线。

**Q：写完数据对端读不到 EOF？**
必须 `CloseWriteAsync()`。帧协议的 Close 帧是唯一 EOF 语义。

**Q：能连多个网关 / 断线重连？**
当前版本单网关连接，不内置重连——监听 `Closed` 后自行重建 `ConnectAsync` 并重开业务流。

**Q：Unity WebGL？**
不支持（无 `ClientWebSocket`）。WebGL 场景改用 Web SDK 或 js 移植。
