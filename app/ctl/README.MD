# Lanet 控制面

`app/ctl` 是托管模式的 GoFrame v2 HTTP 控制面，负责群组、邀请码、成员角色、
NetMap、虚拟 IP 分配和 relay 目录。它不在成员数据路径上。

官方 `lanet` Standalone 单程序不依赖本服务。

## 启动

```bash
# 开发环境：内存存储，重启后群组数据丢失
go run ./app/ctl

# 持久化运行
PVN_CTL_ADDR=:8000 PVN_CTL_DB=./lanet.db go run ./app/ctl
```

默认监听 `:8000`。`PVN_CTL_ADDR` 覆盖 GoFrame 配置中的监听地址；只有设置
`PVN_CTL_DB` 时服务进程才使用 SQLite，否则使用内存存储。SQLite 以 WAL 模式
运行，schema 迁移在打开数据库时自动执行。

健康检查：

```bash
curl http://127.0.0.1:8000/healthz
```

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/v1/groups/create` | 创建群组，创建者成为 owner |
| `POST` | `/v1/groups/join` | 凭邀请码加入群组 |
| `POST` | `/v1/groups/announce` | 成员通告 libp2p 可达地址 |
| `POST` | `/v1/groups/invite/reset` | owner 重置邀请码与有效期 |
| `POST` | `/v1/groups/kick` | owner 移除成员并回收虚拟 IP |
| `GET` | `/v1/groups/netmap` | 查询同群成员目录 |
| `GET` | `/v1/groups` | 列出群组 |
| `POST` | `/v1/relays/register` | relay 注册 |
| `POST` | `/v1/relays/heartbeat` | relay 心跳 |
| `GET` | `/v1/relays/candidates` | 获取中继候选 |
| `GET` | `/healthz` | 健康检查 |

所有业务接口由 `MiddlewareHandlerResponse` 包装为
`{"code": 0, "message": "", "data": ...}`。浏览器 SDK 所需 CORS 已启用。

relay 每 30 秒心跳；超过 150 秒没有注册或心跳的候选会在查询时被清理。

## 虚拟地址（IPv4 + IPv6 双栈）

每个群组同时拿到一对网段，序号相同、一一对应：

| 版本 | 网段 | 成员地址 | 字段 |
|---|---|---|---|
| IPv4 | `10.7.<序号>.0/24` | `10.7.<序号>.<主机号>` | `cidr` / `virtual_ip` |
| IPv6 | `fd00:6c61:6e65:<序号>::/64` | `fd00:6c61:6e65:<序号>::<主机号>` | `cidr_v6` / `virtual_ipv6` |

- 主机号取 2~254，两个版本**同值**（`10.7.7.5` ↔ `fd00:6c61:6e65:7::5`），排障时一眼能对上；
  每群上限 253 人，两个池同进同退（不会出现「分到 IPv4 没分到 IPv6」的半分配状态）。
- IPv6 段落在节点侧 TUN 认可的 ULA 前缀 `fd00:6c61:6e65::/48` 内（`pkg/protocol` 是唯一来源，
  节点与控制面共用），成员地址以 /128 配到网卡、按成员写 /128 路由。
- 字段均为可选：老客户端忽略它们照常工作；`virtual_ipv6` 缺失时客户端回退到自己的派生地址。

## 数据库运维

四个子命令统一读取 `PVN_CTL_DB`，未设置时操作 `./lanet.db`：

```bash
PVN_CTL_DB=./lanet.db go run ./app/ctl migrate
PVN_CTL_DB=./lanet.db go run ./app/ctl dbversion
PVN_CTL_DB=./lanet.db go run ./app/ctl backup ./lanet-backup.db
PVN_CTL_DB=./lanet.db go run ./app/ctl repair
```

- `migrate`：执行尚未应用的 schema 迁移并输出前后版本。
- `dbversion`：只读查看当前迁移版本。
- `backup [dest]`：使用 `VACUUM INTO` 生成包含 WAL 最新状态的一致性备份；
  未传目标时自动生成带时间戳的文件名。
- `repair`：先备份，再尝试直接迁移；失败时导出可读数据、重建并灌回。

已发布的迁移只能追加，不能修改历史版本。

当前 schema 版本为 **2**（v2 给 `members` 增加 `virtual_ipv6` 列）。迁移只加列不改数据：
v2 之前入库的老成员在首次加载时按 IPv4 主机号补齐 IPv6 并写回该列，因此升级不需要
停机重建，也不需要成员重新入网。

## 代码结构

```text
api/                 Req/Res、路由和校验声明
internal/controller/ HTTP 控制器
internal/service/    业务接口
internal/logic/      群组、IPAM、SQLite 与 relay 目录实现
internal/model/      跨层视图模型
manifest/config/     GoFrame 服务配置
```

调用链遵循 `api -> controller -> service -> logic`。
