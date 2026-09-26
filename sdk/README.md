# Lanet SDK 总览

Lanet（群组制 P2P 虚拟局域网）对外提供七套 SDK / 接入形态，按运行环境选择：

| SDK | 运行环境 | 接入方式 | 是否真 P2P | 典型场景 |
|---|---|---|---|---|
| [Go SDK](./go/lanet/README.md) | Go 程序 / 服务器 | **libp2p 直接入群**（Standalone 或托管模式） | ✅ 端到端 | 后端服务互联、可选 TUN、端口转发节点 |
| [Web SDK](./web/README.md) | 浏览器 / Node ≥20（需打包器） | **libp2p 直接入群**（js-libp2p） | ✅ 端到端 | 网页与后端服务直接互开流 |
| [H5 SDK](./h5/README.md) | H5 页面 / 静态站点（免 npm、免打包器） | **libp2p 直接入群**（单文件直引包） | ✅ 端到端 | H5 零构建接入、uni-app 编译到 H5 |
| [Unity SDK](./unity/README.md) | Unity 2021.3+ | **ws-gateway 网关**（跨平台）／**AAR 直接入群**（仅 Android） | ⚠️ 网关中转 ／ ✅ Android 端到端 | 游戏客户端、桌面应用、手机联机 |
| [C# SDK](./csharp/README.md) | .NET 8 / MAUI / 任意 .NET | **经 ws-gateway 接入**（WebSocket 帧协议） | ⚠️ 网关中转 | 桌面 / 服务端 .NET 应用 |
| [uniapp SDK](./uniapp/README.md) | uniapp / 微信小程序 / H5 / App | **经 ws-gateway 接入**（WebSocket 帧协议） | ⚠️ 网关中转 | 小程序、跨端移动应用 |
| [Android 原生插件](./android-plugin/README.md) | uniapp 原生插件 / 任意 Android（AAR） | **AAR 直接入群**（VpnService + gomobile） | ✅ 端到端 | 手机成为网络成员：可 ping 虚拟 IP、被 mDNS 发现 |

> Unity 包（`sdk/unity`）与 Android 插件包（`sdk/android-plugin`）**共用同一份
> `lanet-plugin.aar`**：宿主无关逻辑在 `com.lanet.plugin.LanetNode`，uni-app 侧只是
> DCloud 薄壳，Unity 侧直接 `CallStatic`。改行为只需改一处。
>
> H5 包（`sdk/h5`）与 Web SDK（`sdk/web`）同理：**底层 P2P 只有一份实现**，
> H5 包是它的单文件产物 + H5 便利层（请求-响应、分帧、超时、页面卸载自动下线）。

## 三种接入链路

### 1. libp2p 直连（Go / Web / H5 SDK）

```
节点 A ──────────── 直连（ws / webrtc-direct / webtransport / quic）──────────── 节点 B
   │                                                                                  │
   └────── 全部直连失败时 ──→ relay 中继（Circuit Relay v2）──→ ──────┘
```

- 节点以自己的 PeerID 凭邀请码向控制面（ctl）入群，拿到 `10.7.0.0/16` 段内的虚拟 IP；
- 数据在成员之间端到端传输，relay 只在打洞失败时兜底转发；
- ctl 只负责目录（NetMap / 邀请码 / 中继候选），**不在数据路径上**。

Go SDK 另支持**无服务器模式（Standalone）**：不部署 ctl/relay，节点同时充当
DHT server 与中继（客户端即服务端），经 mDNS + 双 DHT（私有优先 + 公共兜底）
自动发现同网络成员组网。网络归属由**网络密钥（NetworkKey）**决定：
留空 = 加入公共网络（所有留空节点互通），相同密钥 = 私有网络。
网络身份还包含分发渠道：Go SDK 默认 `sdk`，官方发行版固定 `official`；只有
显式设置相同 `Channel` 时二者才互通。
详见 [Go SDK → 无服务器模式](./go/lanet/README.md)。

### 2. ws-gateway 网关中转（C# / uniapp / Unity 链路 1）

```
小程序 / Unity ──(WebSocket 帧协议)──→ ws-gateway ──(libp2p 隧道)──→ 目标节点
```

- 小程序、Unity 等环境没有 WebRTC / WebTransport 能力，无法运行 libp2p 协议栈；
- ws-gateway 是一个**以 Go SDK 节点身份入群的网关进程**，客户端经 WebSocket 与它交换
  统一帧协议（`[type:1][streamID:4][len:4][payload]`，Go/C#/JS 三端字节级一致）；
- 网关把客户端的 dial/data/close 翻译成 libp2p 流操作；数据实时转发、不落盘；
- 代价是数据路径多一跳（客户端→网关→目标），延迟与带宽受网关位置影响。

### 3. Android 本地节点（真入网，不经网关）

```
Android / Unity App ──(VpnService + gomobile 绑定)──→ 直接成为 lanet 网络成员
```

- Go 核心经 gomobile 编成 `lanet.aar`，App **自己就是节点**：可 `ping` 虚拟 IP、
  被 mDNS 发现、跑任意 TCP/UDP，端到端不经中转；
- 两种交付形态共用同一份 AAR：
  - **uni-app 原生插件**（[sdk/android-plugin](./android-plugin/README.md)）——HBuilderX 云打包即可；
  - **Unity 包**（[sdk/unity](./unity/README.md)）——UPM 引入，仅 Android 平台生效；
- 需要系统 VPN 授权（首次启动弹窗）；**不需要 ctl / relay / ws-gateway**；
- 已知边界：AAR 只编了 `armeabi-v7a / arm64-v8a / x86`，没有 x86_64（64 位 x86 模拟器跑不了）。

## 服务端部署要求

| 组件 | 作用 | Go SDK | Web SDK | Unity SDK | C# SDK | uniapp SDK | Android 插件 |
|---|---|:---:|:---:|:---:|:---:|:---:|:---:|
| ctl（控制面） | 群组/邀请码/NetMap/中继目录 | 仅托管模式 | ✅ 必需 | 仅链路 1 | ✅ 必需 | ✅ 必需 | — |
| relay（中继） | 打洞失败兜底 | 仅托管模式 | ✅ 必需 | —（网关持有） | —（网关持有） | —（网关持有） | 节点自兼 |
| ws-gateway | 帧协议网关 | — | — | 仅链路 1 | ✅ 必需 | ✅ 必需 | — |

Go SDK 的 Standalone 模式三项都不需要；节点自身提供 DHT 和 relay service。
Android 本地节点（`sdk/android-plugin`、`sdk/unity` 的链路 2）同样三项都不需要。

- 组件启动示例见仓库根 `app/*/cmd`；网关入口 `go run ./app/gateway/cmd/pvn-gateway`。
- 小程序场景网关必须走 **wss + 备案域名**（微信平台要求），并在小程序后台配置 socket 合法域名。
- 网页 SDK 场景：ctl 已内置 CORS 放行；页面为 HTTPS 时 ctl / relay 需配 TLS。

## 通用概念（各 SDK 一致）

- **群组（Group）**：托管模式中一个群组独占一个 `/24` 子网；Standalone
  不创建中心群组，成员在 `10.7.0.0/16` 内确定性派生虚拟 IP。
- **邀请码（InviteCode）**：入群凭证（常规模式）。Go SDK 可创建群组（成为群主），Web SDK / 网关客户端均凭码加入。
- **网络密钥（NetworkKey）**：无服务器模式的网络归属凭证，留空 = 公共网络，相同密钥 = 私有网络。
- **虚拟 IP（VirtualIP）**：入群时分配，如 `10.7.0.2`。所有 SDK 的开流目标都是对端虚拟 IP。
- **虚拟地址（VirtualHost）**：Go/Standalone 节点可按 `<节点名>.lanet`、短名或
  原始成员名解析目标，避免成员地址变化后修改业务配置。
- **流（Stream）**：一切访问的基本单元。SDK 之间开的是双向字节流（没有消息边界，需应用层分帧）；
  网关客户端 `dial(ip, port)` 额外支持 PortFWD——把对端节点的 TCP 服务桥接为一条双向字节管道。
- **入向防火墙**：Go SDK 节点统一管控三类入向暴露面——端口转发（TCP）、
  TUN 虚拟网卡入向（IP 层 TCP/UDP）、OnStream 应用流（协议 ID），默认**全拒绝**，
  可在内置 Web 控制台（`127.0.0.1:8900`）按「来源虚拟 IP + 协议 + 端口」放行，或设置全开。
- **局域网端口转发**：Go SDK 节点可配置映射表，把本机所在真实局域网的其他设备
  （NAS/内网服务等）暴露给群内成员访问，同样受防火墙约束。
- **TUN**：Go SDK 可设置 `Config.Tun=true` 创建系统虚拟网卡；默认关闭，开启后
  需要 Windows 管理员权限或 Linux `/dev/net/tun` + `CAP_NET_ADMIN`。
- **半关闭（CloseWrite）**：发送完毕必须半关闭写端，对端才能读到 EOF。
  这是所有 SDK 的核心语义，示例代码里都有对应调用。

## 快速开始索引

| 我要… | 看这里 |
|---|---|
| Go 后端互开流 / 做 TCP 桥接节点 | [sdk/go/lanet/README.md](./go/lanet/README.md) |
| 网页直连 P2P | [sdk/web/README.md](./web/README.md) |
| Unity 接入（网关或 Android 本地节点） | [sdk/unity/README.md](./unity/README.md) |
| .NET 程序接入 | [sdk/csharp/README.md](./csharp/README.md) |
| 小程序 / uniapp 接入 | [sdk/uniapp/README.md](./uniapp/README.md) |
| 让 Android App 真入网（uni-app 原生插件） | [sdk/android-plugin/README.md](./android-plugin/README.md) |
| gomobile 绑定层本身 | [sdk/android/README.md](./android/README.md) |
