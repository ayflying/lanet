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

- **群组隔离**：每群从 `100.64.0.0/16` 池分配独立 /24 子网；NetMap 仅返回同群成员，跨群天然不可见。
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

### 方式三：Windows 客户端（发行包）

从 [Releases](https://github.com/ayflying/lanet/releases) 下载 `lanet-agent-windows-amd64.zip`，解压后**以管理员身份**运行（`wintun.dll` 必须与 exe 同目录）：

```powershell
# 创建群组（第一个节点）
.\lanet-agent-windows-amd64.exe -ctl http://<服务端IP>:8000 -mode create -name my-pc -group "我的局域网"

# 凭邀请码加入
.\lanet-agent-windows-amd64.exe -ctl http://<服务端IP>:8000 -mode join -invite <邀请码> -name pc2
```

### 方式四：源码运行（开发调试）

```bash
go run ./app/ctl                                  # 控制面（默认 :8000）
go run ./app/relay                                # 中继（默认监听 4001）
go run ./app/agent/cmd/pvn-agent -mode create -name alpha -ctl http://<ctl地址>:8000
go run ./app/agent/cmd/pvn-agent -mode join -name beta -invite <邀请码> -ctl http://<ctl地址>:8000
```

成员入组后获得群内虚拟 IP（如 `100.64.0.2`），直接 `ping` 组内成员的虚拟 IP 即可互通。

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
| `docker` | push main / `v*` tag | 3 个容器镜像推 GHCR（amd64+arm64）：`lanet-ctl` / `lanet-relay` / `lanet-agent`，tag 版本号 + latest |
| `release-windows` | `v*` tag / 手动 | Windows x64 发行包（agent zip 含 wintun.dll + ctl/relay exe + checksums）附加到 GitHub Release |

镜像地址：

```
ghcr.io/ayflying/lanet-ctl:latest
ghcr.io/ayflying/lanet-relay:latest
ghcr.io/ayflying/lanet-agent:latest
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
    internal/service/         peersource、tunnel 接收端
  relay/                      中继（libp2p Circuit Relay v2，gcmd 子命令：relay/check）
    internal/cmd/             中继入口
pkg/                          跨应用共享库
  p2pkit/        libp2p Host 封装
  protocol/      协议定义（/pvn/tunnel/1.0.0）
  tunnel/        隧道服务（直连→中继三段降级）
  netmapclient/  NetMap 客户端
  tundevice/     TUN 设备与路由器
build/                        容器镜像 Dockerfile（ctl/relay/agent）
deploy/                       容器编排（服务端 / 客户端）
packaging/                    发行包附带文件（如 Windows README）
.github/workflows/            CI：docker（容器镜像）、release-windows（Windows 发行包）
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
- [ ] 真实跨机带宽实测
- [ ] 子网路由（未装客户端的内网设备互通）
