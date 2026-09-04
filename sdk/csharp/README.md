# Lanet.Sdk — C# SDK

Lanet ws-gateway 客户端：让 **Unity / .NET / MAUI** 接入群组网格。
零外部依赖（基于 `System.Net.WebSockets`），双目标打包
`netstandard2.1`（Unity 2021+）+ `net8.0`。

## 能力边界

- ✅ 按虚拟 IP 开流、访问网格内成员的 TCP 服务（经网关 PortFWD）
- ✅ 自定义协议直开流（对端节点注册了对应协议处理器）
- ✅ service 模式接收入向流（网关同一时刻允许一个 service 连接）
- ❌ TUN 内核组网（网关是转发节点，非端到端）

## 快速开始

```csharp
using Lanet.Sdk;

var client = await LanetGatewayClient.ConnectAsync(new GatewayOptions
{
    Url = "ws://gateway.example.com:8700/gateway",   // 生产建议 wss://
    InviteCode = "XXXXXXXXXX",                        // 网关所属群组邀请码
    Name = "my-unity-game"
});

// 访问网格内 TCP 服务（目标节点虚拟 IP + 其本机端口）
var stream = await client.DialAsync("100.64.0.3", 9999);
await stream.WriteStringAsync("hello");
await stream.CloseWriteAsync();                       // 对端读到 EOF
byte[] reply = await stream.ReadAllAsync();
Console.WriteLine(Encoding.UTF8.GetString(reply));
stream.Dispose();

// 自定义协议流
var s2 = await client.DialProtocolAsync("100.64.0.3", "/pvn/tunnel/1.0.0");

// service 模式（接收网格内节点连入）
client.OnStream += stream =>
{
    stream.OnData(data => stream.WriteAsync(data));
};
```

## Unity 接入

1. 拷贝 `Lanet.Sdk/bin/Release/netstandard2.1/Lanet.Sdk.dll` 到 `Assets/Plugins/`；
2. 或用 NuForUnity / 本地打包引用源码工程；
3. WebGL 平台不支持 `ClientWebSocket`，其余平台（Android/iOS/桌面）可用。

## 联调验证

```bash
# 服务端：ctl + relay + gateway + go-service 节点 + TCP 回显 9999
dotnet run --project Lanet.Demo -- ws://127.0.0.1:8700/gateway <inviteCode> <targetVirtualIP>
```

实测记录（2026-09-04，本机）：鉴权 → 自定义协议 echo 往返 15ms →
PortFWD TCP 回显往返 25ms → 心跳，全链路 PASS。
