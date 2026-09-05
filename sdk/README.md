# Lanet SDK 总览

Lanet（群组制 P2P 虚拟局域网）对外提供四套 SDK，按运行环境选择：

| SDK | 运行环境 | 接入方式 | 是否真 P2P | 典型场景 |
|---|---|---|---|---|
| [Go SDK](./go/lanet/README.md) | Go 程序 / 服务器 | **libp2p 直接入群**（直连优先、中继兜底） | ✅ 端到端 | 后端服务互联、端口转发节点、自建服务 |
| [Web SDK](./web/README.md) | 浏览器 / Node ≥20 | **libp2p 直接入群**（js-libp2p） | ✅ 端到端 | 网页与后端服务直接互开流 |
| [C# SDK](./csharp/README.md) | Unity / .NET 8 / MAUI | **经 ws-gateway 接入**（WebSocket 帧协议） | ⚠️ 网关中转 | 游戏客户端、桌面应用 |
| [uniapp SDK](./uniapp/README.md) | uniapp / 微信小程序 / H5 / App | **经 ws-gateway 接入**（WebSocket 帧协议） | ⚠️ 网关中转 | 小程序、跨端移动应用 |

## 两种接入链路

### 1. libp2p 直连（Go / Web SDK）

```
节点 A ──────────── 直连（ws / webrtc-direct / webtransport / quic）──────────── 节点 B
   │                                                                                  │
   └────── 全部直连失败时 ──→ relay 中继（Circuit Relay v2）──→ ──────┘
```

- 节点以自己的 PeerID 凭邀请码向控制面（ctl）入群，拿到 `10.7.0.0/16` 段内的虚拟 IP；
- 数据在成员之间端到端传输，relay 只在打洞失败时兜底转发；
- ctl 只负责目录（NetMap / 邀请码 / 中继候选），**不在数据路径上**。

Go SDK 另支持**无服务器模式（Standalone）**：不部署 ctl/relay，节点同时充当
DHT server 与中继（客户端即服务端），经 mDNS + DHT 自动发现同网络成员组网。
网络归属由**网络密钥（NetworkKey）**决定：留空 = 加入公共网络（所有留空节点互通），
相同密钥 = 私有网络。详见 [Go SDK → 无服务器模式](./go/lanet/README.md)。

### 2. ws-gateway 网关中转（C# / uniapp SDK）

```
小程序 / Unity ──(WebSocket 帧协议)──→ ws-gateway ──(libp2p 隧道)──→ 目标节点
```

- 小程序、Unity 等环境没有 WebRTC / WebTransport 能力，无法运行 libp2p 协议栈；
- ws-gateway 是一个**以 Go SDK 节点身份入群的网关进程**，客户端经 WebSocket 与它交换
  统一帧协议（`[type:1][streamID:4][len:4][payload]`，Go/C#/JS 三端字节级一致）；
- 网关把客户端的 dial/data/close 翻译成 libp2p 流操作；数据实时转发、不落盘；
- 代价是数据路径多一跳（客户端→网关→目标），延迟与带宽受网关位置影响。

## 服务端部署要求

| 组件 | 作用 | Go SDK | Web SDK | C# SDK | uniapp SDK |
|---|---|:---:|:---:|:---:|:---:|
| ctl（控制面） | 群组/邀请码/NetMap/中继目录 | ✅ 必需 | ✅ 必需 | ✅ 必需 | ✅ 必需 |
| relay（中继） | 打洞失败兜底 | ✅ 必需 | ✅ 必需 | —（网关持有） | —（网关持有） |
| ws-gateway | 帧协议网关 | — | — | ✅ 必需 | ✅ 必需 |

- 组件启动示例见仓库根 `app/*/cmd`；网关入口 `go run ./app/gateway/cmd/pvn-gateway`。
- 小程序场景网关必须走 **wss + 备案域名**（微信平台要求），并在小程序后台配置 socket 合法域名。
- 网页 SDK 场景：ctl 已内置 CORS 放行；页面为 HTTPS 时 ctl / relay 需配 TLS。

## 通用概念（四个 SDK 一致）

- **群组（Group）**：一个群组独占一个 /24 子网（`10.7.0.0/16` 池分配），只有同群成员互可见。
- **邀请码（InviteCode）**：入群凭证（常规模式）。Go SDK 可创建群组（成为群主），Web SDK / 网关客户端均凭码加入。
- **网络密钥（NetworkKey）**：无服务器模式的网络归属凭证，留空 = 公共网络，相同密钥 = 私有网络。
- **虚拟 IP（VirtualIP）**：入群时分配，如 `10.7.0.2`。所有 SDK 的开流目标都是对端虚拟 IP。
- **流（Stream）**：一切访问的基本单元。SDK 之间开的是 libp2p 流（消息边界不保证，需应用层分帧）；
  网关客户端 `dial(ip, port)` 额外支持 PortFWD——把对端节点的 TCP 服务桥接为一条双向字节管道。
- **入向防火墙**：Go SDK 节点统一管控三类入向暴露面——端口转发（TCP）、
  TUN 虚拟网卡入向（IP 层 TCP/UDP）、OnStream 应用流（协议 ID），默认**全拒绝**，
  可在内置 Web 控制台（`127.0.0.1:8900`）按「来源虚拟 IP + 协议 + 端口」放行，或设置全开。
- **局域网端口转发**：Go SDK 节点可配置映射表，把本机所在真实局域网的其他设备
  （NAS/内网服务等）暴露给群内成员访问，同样受防火墙约束。
- **半关闭（CloseWrite）**：发送完毕必须半关闭写端，对端才能读到 EOF。
  这是所有 SDK 的核心语义，示例代码里都有对应调用。

## 快速开始索引

| 我要… | 看这里 |
|---|---|
| Go 后端互开流 / 做 TCP 桥接节点 | [sdk/go/lanet/README.md](./go/lanet/README.md) |
| 网页直连 P2P | [sdk/web/README.md](./web/README.md) |
| Unity / .NET 接入 | [sdk/csharp/README.md](./csharp/README.md) |
| 小程序 / uniapp 接入 | [sdk/uniapp/README.md](./uniapp/README.md) |
