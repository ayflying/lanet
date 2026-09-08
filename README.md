# Lanet

**Lanet** = **Lan** + **net**：把不同网络中的设备接入同一张加密的 P2P 虚拟局域网。

官方发行版是一个无需中心服务器的单程序节点。使用相同网络密钥的设备会通过
mDNS 与 DHT 自动发现，优先打洞直连；直连失败时，可经网络内可达成员的
Circuit Relay v2 中继。Windows 和 Linux 节点默认启用 TUN，可直接使用
`10.7.x.x` 虚拟 IP 进行 `ping` 及 TCP/UDP 通信。

当前版本号以仓库根目录 [`VERSION`](./VERSION) 为准。

## 运行模式

| 模式 | 适用场景 | 成员发现与地址分配 | 数据路径 |
|---|---|---|---|
| **Standalone（推荐）** | 官方 `lanet` 单程序、Go SDK 自组网 | mDNS + 私有 DHT，公共 DHT 可用于跨网冷启动；虚拟 IP 由网络身份与 PeerID 确定性派生 | 直连优先，网络内可达成员中继兜底 |
| **托管模式** | 需要邀请码、群主权限、中心成员目录的 SDK/旧 agent 场景 | `ctl` 管理群组、邀请码、NetMap 和 `/24` 子网；独立 `relay` 提供兜底 | 直连优先，指定 relay 兜底 |

官方桌面程序固定使用 Standalone 模式，不需要部署 `ctl` 或独立 `relay`。
托管模式仍保留，主要供 Web、C#、uniapp SDK 及需要中心化群组管理的系统使用。

## Standalone 架构

```text
                       mDNS / DHT 发现
              +-----------------------------+
              |                             |
        +-----v------+     P2P 直连    +-----v------+
        |  lanet A   |<--------------->|  lanet B   |
        | TUN/控制台 |                 | TUN/控制台 |
        +-----+------+                 +-----+------+
              |                             |
              +-----> 可达成员 relay <------+
                     （直连失败时）
```

每个节点同时具备客户端、DHT server 和 relay service 能力。网络身份由
`(分发渠道, 网络密钥)` 共同决定，同网络成员只在本地维护成员表，控制面不在
发现链路或数据链路中。

## 核心能力

- **零中心组网**：相同网络密钥自动成网；局域网走 mDNS，跨网络走双 DHT。
- **直连优先**：TCP、QUIC、WebSocket、webrtc-direct、DCUtR 打洞与
  Circuit Relay v2 组合使用，链路自动降级。
- **真实虚拟网卡**：Windows 使用 Wintun，Linux/macOS 使用系统 TUN；
  `10.7.0.0/16` 全网段均路由到虚拟网卡。
- **稳定身份与地址**：`node.key` 固定 PeerID；虚拟 IP 随身份稳定，成员还可用
  `<节点名>.lanet`、短名或原始成员名连接。
- **统一入向防火墙**：同一套规则覆盖 TUN TCP/UDP、PortFWD 与应用协议流。
- **局域网端口转发**：把节点所在真实局域网中的 NAS、数据库、远程桌面等服务
  暴露给同网络成员。
- **内置 Web 控制台**：成员与链路、节点配置、防火墙、转发映射和更新操作集中管理。
- **签名 P2P 更新**：官方裸机程序通过成员间分发签名清单和二进制；容器更新交给编排层。
- **渠道隔离**：官方发行版与第三方 SDK 构建默认不在同一网络，避免 SDK 程序
  意外混入官方网络。

## 快速开始

### Windows

1. 从 [Releases](https://github.com/ayflying/lanet/releases) 下载
   `lanet-<版本>-windows-amd64.zip` 并完整解压。
2. 保持 `lanet.exe` 与 `wintun.dll` 在同一目录，双击 `lanet.exe`，在 UAC
   提示中选择“是”。程序已内置管理员清单，不需要右键提权。
3. 首次启动会创建配置并打开 `http://127.0.0.1:8900`。在“节点配置”填写
   节点名称和网络密钥，保存后重启。
4. 其他设备使用相同网络密钥启动，成员出现后即可互相 `ping 10.7.x.x` 或访问服务。

只有首次生成配置时会自动打开浏览器。后续可从托盘菜单打开控制台或退出节点。

命令行参数会覆盖 `lanet.json`：

```powershell
./lanet.exe -name pc1 -key "our-network-key"
./lanet.exe -name pc2 -key "our-network-key"

# 纯局域网发现，不接入公共 DHT
./lanet.exe -name pc3 -key "our-network-key" -bootstrap none -no-public-dht

# 临时关闭 TUN，仅使用 SDK 流/端口转发能力
./lanet.exe -tun false
```

### Linux

Linux 发行包需要 root，或至少具备 `/dev/net/tun` 和 `CAP_NET_ADMIN`：

```bash
sudo ./lanet -name server-1 -key 'our-network-key'
```

`lanet.json`、`state.json` 和 `lanet.log` 位于配置文件目录；Linux 身份文件默认是
`/data/node.key`。长期运行建议使用 systemd，并持久化这些文件。

### 公网节点容器

把根目录 [`docker-compose.yml`](./docker-compose.yml) 放到有公网 IP 的 Linux
主机，修改 `environment` 后启动：

```bash
git clone https://github.com/ayflying/lanet.git
cd lanet
docker compose up -d
docker compose logs -f node
```

至少设置：

| 配置 | 说明 |
|---|---|
| `LANET_NAME` | 节点名称 |
| `LANET_NETWORK_KEY` | 网络密钥；与其他官方节点一致 |
| `LANET_CONSOLE` | `127.0.0.1:8900` 仅本机，`0.0.0.0:8900` 允许远程访问 |
| `LANET_CONSOLE_PASSWORD` | 控制台远程开放时必须设置强密码 |
| `LANET_LISTEN` | 固定 P2P 监听地址，Compose 默认使用 4001/TCP+UDP |

放行 `4001/tcp`、`4001/udp`；只有确实远程开放控制台时才放行 `8900/tcp`。
身份、配置、状态和日志保存在 `lanet-node-data` 命名卷中。

Compose 默认没有授予 TUN 权限，因此该公网节点作为引导/中继运行并自动降级为
应用层模式。若还要让容器本身响应虚拟 IP，请按文件内注释启用
`NET_ADMIN` 与 `/dev/net/tun`。

### 源码运行

编译和测试需要 Go 1.25。官方单程序入口：

```bash
go run ./app/agent/cmd/pvn-node -name dev-node -key dev-network
```

托管模式组件：

```bash
PVN_CTL_DB=./lanet.db go run ./app/ctl
PVN_RELAY_CTL=http://127.0.0.1:8000 go run ./app/relay

go run ./app/agent/cmd/pvn-agent \
  -ctl http://127.0.0.1:8000 -mode create -name alpha -group demo -real-tun
```

详细说明见：

- [`app/agent/README.MD`](./app/agent/README.MD)：官方节点、托管 agent 与联调工具
- [`app/ctl/README.MD`](./app/ctl/README.MD)：控制面 API、持久化与运维命令
- [`app/relay/README.MD`](./app/relay/README.MD)：独立 relay 的部署与通告
- [`docs/virtual-network-connectivity-fix.md`](./docs/virtual-network-connectivity-fix.md)：虚拟局域网互通问题排障复盘、修复步骤与验证证据

## 配置与安全

### 节点文件

| 文件 | 作用 | 变更生效方式 |
|---|---|---|
| `lanet.json` | 名称、网络密钥、引导方式、控制台、监听地址、TUN 等节点配置 | 保存后重启 |
| `node.key` | Ed25519 节点身份，决定 PeerID 和稳定虚拟 IP | 不应删除或在多个在线节点间共用 |
| `state.json` | 防火墙规则与局域网端口转发映射 | 控制台保存后立即生效 |
| `lanet.log` | 运行日志和链路探测结果 | 实时写入 |
| `manifest.json` | 官方签名更新清单种子 | 随发行包提供 |

Windows 上这些文件位于 `lanet.exe`/配置文件目录；`node.key` 也随整个目录迁移。
控制台 GET 接口不会返回密码明文。

### 网络密钥与发现

- 网络密钥留空会加入官方公共网络，不适合传输敏感数据。
- 非空且相同的密钥组成私有网络；密钥经 SHA-256 派生网络标识，不直接广播明文。
- `bootstrap=public` 使用公共 DHT 完成跨网冷启动；`bootstrap=none` 仅使用 mDNS。
- 指定任意已在网成员的完整 multiaddr 可加快私有网络冷启动。
- `no_public_dht=true` 或 `-no-public-dht` 会关闭公共 DHT 兜底，此时跨网必须有
  可达种子。

官方发行版固定为 `official` 渠道，Go SDK 默认是 `sdk` 渠道。即使网络密钥相同，
两个渠道也不会互相发现；Go SDK 只有显式设置 `Channel: lanet.ChannelOfficial`
才会加入官方发行版网络。

### TUN 与防火墙

官方程序默认 `tun=true`、`firewall=allow-all`，便于直接使用虚拟 IP。SDK 默认
不开 TUN，防火墙默认 `deny-all`。请按部署边界收紧入向规则。

Windows 为每个已发现成员维护 `/32` on-link 路由及邻居项；Linux 把整个
`10.7.0.0/16` 路由到 TUN。TUN 创建或配置失败时节点仍会继续运行，`Dial`、
自定义协议和 PortFWD 不受影响，具体原因写入日志。

ICMP 没有端口：`deny-all` 或没有匹配协议规则的 `allow-list` 会使 `ping` 被丢弃。
远程开放控制台时，应同时设置密码并在主机防火墙限制来源。

### P2P 自动更新

官方裸机二进制强制启用签名 P2P 更新。节点发现同平台新版本后，从一个已持有该
版本的成员获取 Ed25519 签名清单，验签并校验 SHA-256 后替换程序，随后随机延迟
1 到 8 分钟重启。签名私钥只存在于发布流水线，成员只能转发、不能伪造清单。

`dev` 构建和容器环境自动禁用该机制。私有 GitHub 仓库的主动检查更新可通过
`github_token` 或 `GITHUB_TOKEN` 提供只读令牌。

## SDK

Lanet 提供四套 SDK：

| SDK | 接入方式 | TUN | 典型场景 |
|---|---|:---:|---|
| [Go SDK](./sdk/go/lanet/README.md) | libp2p 直连；支持 Standalone 或托管模式 | 可选 | Go 服务、原生节点、端口转发 |
| [Web SDK](./sdk/web/README.md) | 浏览器/Node 直接运行 js-libp2p | 否 | 网页与 Go 节点互开流 |
| [C# SDK](./sdk/csharp/README.md) | 经 ws-gateway 接入 | 否 | Unity、.NET、MAUI |
| [uniapp SDK](./sdk/uniapp/README.md) | 经 ws-gateway 接入 | 否 | 小程序、H5、跨端应用 |

选择指南和两种接入链路见 [`sdk/README.md`](./sdk/README.md)。浏览器、C# 和
uniapp 当前使用托管模式；Go SDK 可直接创建与官方节点同能力的 Standalone 节点。

## 托管模式运维

托管模式由 `ctl + relay + agent/SDK` 组成。`ctl` 负责群组、邀请码、角色、
NetMap 和 relay 目录，不承载数据流量；`relay` 每 30 秒向 `ctl` 心跳，超过
150 秒未续期的候选会被移除。

控制面持久化和维护示例：

```bash
export PVN_CTL_DB=./lanet.db
go run ./app/ctl migrate
go run ./app/ctl dbversion
go run ./app/ctl backup ./lanet-backup.db
go run ./app/ctl repair
```

`PVN_CTL_DB` 未设置时，控制面使用内存存储，重启后群组数据丢失。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
go run ./app/agent/cmd/pvn-e2e-check
go run ./app/agent/cmd/pvn-serverless-check
go run ./app/agent/cmd/pvn-firewall-check
go run ./app/agent/cmd/pvn-identity-check
```

远程构建发布前还应验证发行架构：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./app/agent/cmd/pvn-node
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./app/agent/cmd/pvn-node
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./app/agent/cmd/pvn-node
```

### 0.5.8 覆盖性实测

2026-09-08 在 Windows amd64 与 Linux amd64 节点上完成了当前版本的运行时回归，节点
虚拟 IP 为 `10.7.243.173` 和 `10.7.9.215`：

- Windows → Linux、Linux → Windows 各 `100/100`，丢包率 `0%`；Linux 高频 `500/500`，丢包率 `0%`；
- DF 模式下双向发送 1200/1360 字节 ICMP 均 `10/10` 成功，确认 TUN MTU 与分片边界；
- TCP、UDP 双向主动连接和原文回显均成功；同时访问离线成员时，在线成员仍保持 `30/30`，离线目标不会阻塞数据面；
- 控制面建群/邀请码入群/Relay、中继回退、Standalone DHT+mDNS、身份持久化、虚拟域名、
  防火墙、端口转发、控制台热更新、密码认证、更新检查、重启和退出入口均完成回归；
- `go test ./...`、`go test -race ./...`、`go vet ./...` 以及 Windows amd64、Linux amd64、
  Linux arm64 交叉编译均通过。

重启属于进程级重建：API 通常约 2 秒恢复，P2P 路由和邻居表还需要短暂收敛。重启后等待
约 10 秒，双向 ping 恢复 `20/20`、`0%` 丢包；这不是热重启承诺，业务侧应为重启窗口准备重试。

Windows 首次生成 `lanet.json` 时自动打开控制台页签；升级、重启或普通再次启动只记录
“跳过自动打开控制台页签”，不会重复打开浏览器。`<节点名>.lanet`、短名等名称只由
Lanet SDK 在成员表内解析，当前不会自动注册到 Windows/Linux 系统 DNS，因此系统自带的
`ping <节点名>.lanet` 仍需要额外的 DNS 或 hosts 配置。

历史真机验证：

| 日期 | 环境 | 已验证内容 |
|---|---|---|
| 2026-09-04 | 2 台 Linux + 1 台 Windows | 托管模式建群/入组/NetMap、6 个方向直连、relay 兜底、权限边界、SQLite 重启恢复 |
| 2026-09-05 | 不同物理网络的 Standalone 节点 | 公共 DHT 冷启动、私有 DHT 发现、DCUtR/QUIC 打洞，双向 direct RTT 7-13ms |
| 2026-09-08 | Windows + Linux | TUN 双向回包、跨 `/24` 虚拟 IP、Windows 单播路由与邻居处理 |

以上是对应日期的验证记录，不替代当前提交的自动化测试结果。

## CI/CD

版本号唯一来源是 [`VERSION`](./VERSION)。

| 流水线 | 产物 |
|---|---|
| `docker` | `lanet-ctl`、`lanet-relay`、`lanet-agent`、`lanet-node` 的 amd64/arm64 GHCR 镜像 |
| `release` | Windows amd64、Linux amd64/arm64 压缩包、`sha256sums.txt` 和签名 `manifest.json` |

镜像地址：

```text
ghcr.io/ayflying/lanet-ctl:latest
ghcr.io/ayflying/lanet-relay:latest
ghcr.io/ayflying/lanet-agent:latest
ghcr.io/ayflying/lanet-node:latest
```

发布包中的 `wintun.dll` 来自 Wintun 0.14.1，必须与 Windows 可执行文件一起分发。

## 目录结构

```text
app/
  agent/cmd/pvn-node/       官方 Standalone 单程序
  agent/cmd/pvn-agent/      托管模式客户端
  agent/cmd/*-check/        端到端与专项联调工具
  ctl/                      GoFrame v2 控制面
  relay/                    独立 Circuit Relay v2 服务
  gateway/                  C#/uniapp 的 WebSocket 网关
sdk/
  go/lanet/                 Go SDK
  web/                      浏览器/Node SDK
  csharp/                   Unity/.NET/MAUI SDK
  uniapp/                   uniapp/小程序 SDK
pkg/
  serverless/               Standalone 发现、网络身份、虚拟地址
  p2pkit/                   libp2p Host、打洞与 relay 封装
  tunnel/                   隧道拨号与链路降级
  tundevice/                TUN 配置、路由与 IP 包转发
  firewall/                 统一入向防火墙
  selfupdate/               签名清单校验与更新分发
  gatewayproto/             ws-gateway 二进制帧协议
build/                      容器镜像 Dockerfile
packaging/                  发行包说明、图标与 Windows 清单
```

主要依赖：Go 1.25、GoFrame v2、go-libp2p v0.49、kad-dht v0.42、wireguard/tun。

## Roadmap

- [x] Standalone 零中心组网、双 DHT、mDNS、打洞与成员中继
- [x] Windows/Linux TUN 虚拟 IP 双向通信与跨 `/24` 路由
- [x] 稳定身份、`<节点名>.lanet`、控制台、防火墙与局域网端口转发
- [x] 签名 P2P 自动更新与多平台发行流水线
- [x] Go、Web、C#、uniapp SDK 与 ws-gateway
- [ ] 真实跨机吞吐与长时间稳定性基准
- [ ] 未安装客户端设备的三层子网路由
- [ ] 浏览器 webrtc-direct 在复杂 NAT 下的跨网实测
- [ ] 托管模式 ctl API key 鉴权
