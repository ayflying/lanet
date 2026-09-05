# Lanet

**Lanet** = **Lan** + **net** —— 把散落各地的人拉进同一张局域网。

Lanet 是一个自研的群组制 P2P 虚拟局域网系统：客户端创建群组并邀请他人加入，**只有同群成员才处于同一个虚拟局域网**。组内流量优先 P2P 直连（打洞成功即可跑满带宽），直连失败自动降级到中继转发，保证不断连。

## 架构

```
                ┌─────────────┐
                │  ctl 控制面  │   建群 / 邀请码 / NetMap / 中继目录
                └──────┬──────┘
                       │  HTTP
        ┌──────────────┼──────────────┐
        │              │              │
  ┌─────▼─────┐  ┌─────▼─────┐  ┌─────▼─────┐
  │  agent A   │◄─┼──P2P 直连──┼─►│  agent B   │   同群成员互连
  └─────┬─────┘  └───────────┘  └─────┬─────┘
        │                             │
        └──────► relay 中继 ◄─────────┘   直连失败时兜底转发
```

- **ctl（控制面）**：GoFrame v2 编写的 HTTP 服务。负责群组创建、邀请码、成员加入、子网分配（每群独立 /24）、NetMap 查询与中继目录。
- **relay（中继）**：基于 libp2p Circuit Relay v2 的转发节点，注册到 ctl 供客户端发现，`WithInfiniteLimits` 不限时长与流量。
- **agent（客户端）**：用户终端程序。创建 libp2p Host（AutoNAT / 打洞 / AutoRelay），拉取所在群的 NetMap，通过 TUN 虚拟网卡收发包。

## 核心特性

- **群组隔离**：每群从 `10.7.0.0/16` 池分配独立 /24 子网；NetMap 仅返回同群成员，跨群天然不可见。
- **三段隧道策略**：直连优先 → 复用已有连接开流 → Circuit Relay v2 中继保底，全程自动降级。
- **虚拟网卡**：基于 `wireguard/tun` 的真实 TUN 设备，支持 Windows / Linux / macOS，本地测试可用内存回环设备。
- **打洞能力**：AutoNAT + DCUtR 打洞 + AutoRelay，典型 NAT 环境下即可直连。

## 快速开始

### 方式一：Docker Compose 部署服务端（推荐）

镜像由 CI 自动编译发布到 GHCR（`linux/amd64` + `linux/arm64`）：

```bash
git clone https://github.com/ayflying/lanet.git && cd lanet
docker compose -f deploy/docker-compose.server.yml up -d
```

该编排启动两个服务：

| 服务 | 镜像 | 端口 | 说明 |
|---|---|---|---|
| ctl | `ghcr.io/ayflying/lanet-ctl:latest` | 8000/tcp | 控制面 API，数据持久化在 `./data` |
| relay | `ghcr.io/ayflying/lanet-relay:latest` | 4001/tcp+udp | P2P 中继兜底转发 |

### 方式二：客户端容器（Linux 主机）

```bash
PVN_CTL_ADDR=http://<服务端IP>:8000 PVN_NAME=pc1 \
PVN_MODE=join PVN_INVITE=<邀请码> \
docker compose -f deploy/docker-compose.agent.yml up -d
```

需要 `NET_ADMIN` 权限与 `/dev/net/tun`（编排文件已声明）。

### 方式三：Windows / Linux 单程序（发行包，推荐）

从 [Releases](https://github.com/ayflying/lanet/releases) 下载对应平台压缩包并解压。
**只有一个程序 `lanet`**——客户端与服务端一体，无需部署任何服务器。

**Windows 双击即用**：右键以管理员身份运行 `lanet.exe`（无黑框窗口），
桌面托盘出现图标、浏览器自动打开 Web 控制台；在「节点配置」里填好
节点名称与网络密钥，保存后重启程序即完成入网。托盘右键可打开控制台或退出。
`wintun.dll` 必须与 `lanet.exe` 同目录。

```powershell
# 命令行方式（可选，参数会覆盖 lanet.json 配置文件）
.\lanet.exe -name pc1 -key "我们的网络密钥"
.\lanet.exe -name pc2 -key "我们的网络密钥"   # 相同密钥自动互相发现并组网
```

零参数启动时自动在 exe 同目录生成 `lanet.json`（配置）、`node.key`（身份）、
`lanet.log`（日志）。控制台默认 `http://127.0.0.1:8900`（**仅本机可访问**）：
查看成员、配置防火墙与端口转发（即时生效）、修改节点配置（重启生效）。
页面为全屏宽度布局，适配手机浏览器。

**控制台安全**：如需从其他设备访问控制台，在「节点配置」把控制台地址改为
`0.0.0.0:8900` 并设置控制台密码（登录会话 7 天有效），重启生效；
SDK 侧对应 `Config.ConsolePassword`。容器镜像默认已监听 `0.0.0.0`（环境变量
`LANET_CONSOLE`），远程暴露时同样务必设置 `LANET_CONSOLE_PASSWORD`。

### 方式四：源码运行（开发调试）

```bash
go run ./app/ctl                                  # 控制面（默认 :8000）
go run ./app/relay                                # 中继（默认监听 4001）
go run ./app/agent/cmd/pvn-agent -mode create -name alpha -ctl http://<ctl地址>:8000
go run ./app/agent/cmd/pvn-agent -mode join -name beta -invite <邀请码> -ctl http://<ctl地址>:8000
```

成员入组后获得群内虚拟 IP（如 `10.7.0.2`），直接 `ping` 组内成员的虚拟 IP 即可互通。

### 群主管理

创建者即群主（`role=owner`），拥有以下专属能力：

```bash
# 重置邀请码（旧码立即作废；valid_seconds 缺省为永久有效）
curl -X POST http://<ctl>/v1/groups/invite/reset \
  -d '{"operator_peer_id":"<群主peer>","group_id":"g0","valid_seconds":86400}'

# 踢出成员（回收虚拟 IP、清除通告地址，被踢者立即离开局域网）
curl -X POST http://<ctl>/v1/groups/kick \
  -d '{"operator_peer_id":"<群主peer>","group_id":"g0","target_peer_id":"<被踢peer>"}'
```

群组数据（群组/成员/角色/邀请码及有效期/通告地址）存储于 SQLite（WAL 模式），服务重启后自动恢复。通过环境变量控制：

- `PVN_CTL_DB`：SQLite 数据库路径；未设置时退化为内存模式（重启丢数据）
- `PVN_CTL_ADDR`：监听地址覆盖（如 `127.0.0.1:18090`），优先于 config.yaml

## 验证

```bash
# 全部单元测试
go test ./...

# 端到端链路验证（本地起 ctl + relay + 双 agent，实测建群→入组→直连→回包）
go run ./app/agent/cmd/pvn-e2e-check
```

### 真机联测记录（2026-09-04）

三机环境：217（Linux，跑 ctl+relay 服务端）、243（Linux）、Windows 开发机（后两台各跑 agent/探针）。实测覆盖：

| 验证项 | 结果 |
|---|---|
| 建群 → 入组 → NetMap（独立 /24 分配、通告地址） | ✅ |
| 6/6 方向跨机隧道回包（Win↔243、Win↔217、217↔243 双向，`path=direct`） | ✅ |
| relay 中继转发（双端预约 Reservation 后经中继回包） | ✅ |
| 边界场景：错误邀请码 / 重复加入 / 非群主操作 / 邀请码过期 / 踢人即时生效 | ✅ |
| ctl 容器重启后数据完整恢复（SQLite WAL）；四个运维子命令容器内实跑 | ✅ |

注意事项（实测经验）：

- 探针/测试客户端每次 join 都会占用一个虚拟 IP（10.7.x.x 池），频繁重启会持续消耗；正式使用无需关心，长期自动化测试建议建独立群组或实现成员回收。
- Windows 端回显类自测程序需注意 libp2p 流的半关闭语义：发送完成后 `CloseWrite()` 半关闭写端，对端才能用 `ReadAll` 判定 EOF，否则双方互等死锁。

## SDK

Lanet 对外提供两套 SDK，让其他项目以编程方式接入群组网格：

### Go SDK（`sdk/go/lanet`）

把任意 Go 程序变成网格中的一个节点：入网、开流、接流，十几行代码完成。

```go
client, err := lanet.New(ctx, lanet.Config{
	CTLURL: "http://ctl.example.com:8000",
	Name:   "my-service",
	GroupName: "my-group", // 或填 InviteCode 凭邀请码加入
})
defer client.Close()

// 按虚拟 IP 开流（直连优先、中继兜底，与 agent 一致）
stream, viaRelay, _ := client.Dial(ctx, "10.7.0.2")
defer stream.Close()

// 接收入向流
client.OnStream(func(stream lanet.Stream) { /* ... */ })

// 周期维护（NetMap 刷新 / 地址通告 / 中继预约），阻塞直到 ctx 取消
client.Run(ctx)
```

完整 API 与示例见 [sdk/go/lanet/README.md](sdk/go/lanet/README.md)。

### Web SDK（`sdk/web`，npm 包名 `@lanet/sdk-web`）

网页作为一个**真正的 P2P 节点**加入群组：js-libp2p（WebSocket + WebRTC +
Circuit Relay v2），与 Go 节点互开隧道流（`/pvn/tunnel/1.0.0`）。
直连优先（ws / webrtc-direct）、relay 电路兜底，无需独立信令服务器。

```js
import { createNode } from '@lanet/sdk-web'
const node = await createNode({ ctlURL, inviteCode, name: 'web-demo' })
const stream = await node.dial('10.7.0.2')  // 按虚拟 IP 开流
```

联调工具：`go run ./app/agent/cmd/pvn-web-echo`（Go echo 节点，打印邀请码）+
`node sdk/web/test/interop.mjs`（Node 互通验证）。浏览器与 Go 互通已本机实测
（直连 echo 往返 ~100ms），详见 [sdk/web/README.md](sdk/web/README.md)。
受浏览器沙箱限制无 TUN/虚拟 IP 路由能力；组内 TCP 服务桥接（portfwd）见 Roadmap。

### ws-gateway + C# / uniapp SDK（网关接入模式）

无法运行 libp2p 的运行时（C#/Unity、微信小程序等）经 **ws-gateway** 接入：
网关以 Go SDK 节点身份入群，客户端用一条 WebSocket 走统一的二进制帧协议
（auth / dial / data / close，见 `pkg/gatewayproto`）。

```bash
# 网关服务（加入指定群组；-invite 省略则创建新群组并打印邀请码）
go run ./app/gateway/cmd/pvn-gateway -ctl http://ctl:8000 -invite XXXXXXXXXX
```

- **C# SDK**（`sdk/csharp`，NuGet 包 `Lanet.Sdk`，netstandard2.1 + net8.0）：
  `DialAsync(ip, port)` / `DialProtocolAsync` / `OnStream`，Unity 2021+ 可用；
- **uniapp SDK**（`sdk/uniapp`，npm 包 `@lanet/sdk-uniapp`）：
  自动适配 uni / wx / H5，小程序要求网关走 wss + 备案域名；
- Go SDK 节点自动获得 **PortFWD 端口转发**能力（`/pvn/portfwd/1.0.0`）：
  网关客户端 dial 的 `ip:port` 由目标节点 net.Dial 到本机/内网服务，
  远程桌面、数据库等 TCP 应用由此桥接。

三端互通已本机实测 PASS：js 客户端（自定义协议 echo 1ms、PortFWD 3ms）、
.NET 客户端（echo 15ms、PortFWD 25ms）。详见各 SDK 目录 README。

## 中继兜底与数据库运维

### relay 链路说明

- relay 启动时通过 `-ctl http://<ctl>:8000` 向控制面**自注册**，之后每 30s 心跳保活；心跳失联会自动重新注册。容器编排（`deploy/docker-compose.server.yml`）已默认配置。
- 容器 NAT 环境下 relay 通告的默认是容器内网地址，外部 agent 不可达——用 `PVN_RELAY_ADVERTISE` 显式指定宿主可达地址（逗号分隔 multiaddr，如 `/ip4/1.2.3.4/tcp/4001,/ip4/1.2.3.4/udp/4001/quic-v1`）。
- agent 入网后会**主动向 relay 预约**（`EnsureRelayReservation`，周期补充）：Circuit Relay v2 要求目标 peer 必须有 Reservation，否则对端经中继拨号会报 `NO_RESERVATION(204)`。AutoRelay 只在节点自认不可达时才预约，公网可达节点必须靠主动预约兜底。
- ctl 对超过 150s 无心跳的候选做惰性清理，agent 不会拿到失效 relay。

### 数据库运维子命令（ctl 二进制）

通过 `PVN_CTL_DB` 指定库路径（缺省 `./lanet.db`），容器内可直接 `docker exec`：

```bash
lanet-ctl migrate              # 显式跑 schema 版本化迁移并打印版本
lanet-ctl dbversion            # 查看当前迁移版本
lanet-ctl backup [dest]        # VACUUM INTO 一致性备份（缺省自动带纳秒时间戳）
lanet-ctl repair               # 损坏库自动修复：先备份→直迁→失败则导出可读数据→重建灌回
```

schema 迁移采用账本表 `schema_migrations` 记录版本，迁移以有序切片内嵌在代码中；**禁止修改已发布的迁移，只允许追加新版本**。

## CI/CD

| 流水线 | 触发 | 产物 |
|---|---|---|
| `docker` | push main / `v*` tag | 4 个容器镜像推 GHCR（amd64+arm64）：`lanet-ctl` / `lanet-relay` / `lanet-agent` / `lanet-node`，含 VERSION 文件版本号 tag + latest |
| `release` | push main / `v*` tag / 手动 | 全平台发行压缩包附加到 GitHub Release（见下） |

### 版本号与发行流程

版本号唯一来源是根目录 **`VERSION` 文件**（三段式，如 `0.2.1`）：

1. **每次代码修改先改 VERSION 再提交**（版本号 +1）；
2. `docker` 流水线读取 VERSION，镜像获得 `0.2.1` 版本 tag（外加 latest / main / sha）；
3. `release` 流水线自动交叉编译并打包发行文件（**全部压缩，不直接上传 exe**）：

| 发行包 | 内容 |
|---|---|
| `lanet-{版本}-windows-amd64.zip` | lanet-agent / ctl / relay / node（exe）+ wintun.dll + README + VERSION |
| `lanet-{版本}-linux-amd64.tar.gz` | lanet-agent / ctl / relay / node + VERSION |
| `lanet-{版本}-linux-arm64.tar.gz` | 同上（arm64） |
| `sha256sums.txt` | 全部压缩包校验和 |

- 发行包同时创建/更新 GitHub Release（tag `v{VERSION}`）；
- 各二进制由 `-ldflags "-X main.version={VERSION}"` 注入版本，启动日志可见（如 `[node] 启动 ... version=0.2.1`），可用于部署核验。

镜像地址：

```
ghcr.io/ayflying/lanet-ctl:latest
ghcr.io/ayflying/lanet-relay:latest
ghcr.io/ayflying/lanet-agent:latest
ghcr.io/ayflying/lanet-node:latest
```

> 容器镜像均为私有，拉取前需 `docker login ghcr.io`。
> Windows 发行包中的 `wintun.dll` 来自 [wintun.net 官方 0.14.1](https://www.wintun.net/)，不打入 exe（运行时从 exe 同目录加载），因此必须随包分发。

## 目录结构

工程由 `gf init -m` 脚手架生成，遵循 GoFrame 官方工程规范：

```
app/                          gf mono-repo 应用目录
  ctl/                        控制面（HTTP 服务，gf 规范分层）
    api/                      接口定义层：Req/Res + 路由声明（group/health/relay）
    internal/
      cmd/                    入口：服务注册 + 路由绑定
      controller/             控制器：实现 api 接口，委托 service
      logic/                  业务实现：group（SQLite 持久化）、node（IPAM）、relaydir
      model/                  api/logic 共享视图结构
      service/                接口定义层（logic 实现后注册）
      consts/ hack/ manifest/ gf 脚手架标准目录
  agent/                      客户端
    cmd/pvn-agent/            客户端入口（gcmd 声明式参数）
    cmd/pvn-e2e-check/        端到端验证
    cmd/pvn-web-echo/         Web SDK 联调 echo 节点
    internal/service/         tunnel 接收端
  relay/                      中继（libp2p Circuit Relay v2，gcmd 子命令：relay/check）
    internal/cmd/             中继入口
  gateway/                    ws-gateway（C#/uniapp 等运行时的网关接入）
    cmd/pvn-gateway/          网关入口
    internal/gateway/         WS 帧协议服务（auth/dial/data/close）
sdk/                          对外 SDK
  go/lanet/                   Go SDK（入网 / Dial / DialPortFWD / OnStream / Run）
  web/                        Web SDK（js-libp2p：浏览器/Node 节点入网互开流）
  csharp/                     C# SDK（Unity/.NET/MAUI，经 ws-gateway 接入）
  uniapp/                     uniapp SDK（小程序/H5/Node，经 ws-gateway 接入）
pkg/                          跨应用共享库
  p2pkit/        libp2p Host 封装（含 webrtc-direct 传输层）
  peersource/    中继候选客户端（控制面 /v1/relays/candidates）
  protocol/      协议定义（/pvn/tunnel/1.0.0、/pvn/portfwd/1.0.0）
  gatewayproto/  ws-gateway 帧协议（Go/C#/JS 三端同构）
  tunnel/        隧道服务（直连→中继三段降级）
  netmapclient/  NetMap 客户端
  tundevice/     TUN 设备与路由器
build/                        容器镜像 Dockerfile（ctl/relay/agent）
deploy/                       容器编排（服务端 / 客户端）
packaging/                    发行包附带文件（README、logo 图标）
.github/workflows/            CI：docker（容器镜像）、release（全平台压缩发行包）
```

接口走 gf 标准链路：`api`（声明 Req/Res 与路由）→ `controller`（实现）→ `service`（接口）→ `logic`（实现），
响应统一为 `MiddlewareHandlerResponse` 包装（`{code, message, data}`），入参校验用 gf `v:` 规则。

## 技术栈

Go · GoFrame v2 · go-libp2p v0.44 · wireguard/tun

## Roadmap

- [x] 群组/成员/邀请码落库 SQLite（重启不丢）
- [x] 邀请码过期、成员踢出、群主权限
- [x] 容器镜像 CI（GHCR，amd64+arm64）与服务端/客户端编排
- [x] Windows 发行包 CI（交叉编译 + wintun.dll 打包 → Release）
- [x] 真实跨机联测（217/243/Windows 三机，6 方向隧道 + relay 兜底 + 边界场景，见「真机联测记录」）
- [x] Go SDK（`sdk/go/lanet`：New / Dial / OnStream / Run，宿主程序十几行代码入网）
- [x] Web SDK（`sdk/web`：js-libp2p 浏览器/Node 入网、Dial、OnStream；与 Go 节点互通实测通过）
- [x] relay 支持 WebSocket 监听（tcp 与 ws 共享 4001 端口）+ ctl CORS（浏览器直连前提）
- [x] portfwd 端口转发协议（Go SDK 节点自动启用；浏览器/C#/小程序经网关桥接组内 TCP 服务）
- [x] ws-gateway（`app/gateway`）+ C# SDK（`sdk/csharp`）+ uniapp SDK（`sdk/uniapp`），三端帧协议同构，互通实测通过
- [ ] 虚拟 IP 成员下线回收（长期测试场景）
- [ ] 真实跨机带宽实测
- [ ] 子网路由（未装客户端的内网设备互通）
- [ ] Web SDK 浏览器端 webrtc-direct 直连实测（NAT 穿透场景，需公网 STUN/TURN）
- [ ] ctl API key 鉴权（网关/SDK 场景下管理操作的权限控制）
- [ ] ctl API key 鉴权（SDK 场景下管理操作的权限控制）
