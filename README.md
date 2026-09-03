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

```bash
# 启动控制面（默认 :8000）
go run ./app/ctl

# 启动中继（需公网可达，注册到 ctl）
go run ./cmd/pvn-relay -ctl http://<ctl地址>:8000 -name relay-1

# 客户端创建群组
go run ./app/agent/cmd/pvn-agent -mode create -name alpha -ctl http://<ctl地址>:8000

# 其他成员凭邀请码加入
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

## 目录结构

```
app/
  ctl/        控制面：群组、中继目录、健康检查
  agent/      客户端入口：pvn-agent、pvn-e2e-check
cmd/
  pvn-relay/        中继节点
  pvn-relay-check/  中继自检工具
pkg/
  p2pkit/     libp2p Host 封装
  protocol/   协议定义（/pvn/tunnel/1.0.0）
  tunnel/     隧道服务（直连→中继三段降级）
  netmapclient/  NetMap 客户端
  tundevice/  TUN 设备与路由器
```

## 技术栈

Go · GoFrame v2 · go-libp2p v0.44 · wireguard/tun

## Roadmap

- [x] 群组/成员/邀请码落库 SQLite（重启不丢）
- [x] 邀请码过期、成员踢出、群主权限
- [ ] 真实跨机带宽实测
- [ ] 子网路由（未装客户端的内网设备互通）
