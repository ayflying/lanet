# 控制面 IPv6 双栈：验收方案（P1-6）

> 状态：**待确认**（确认后才动代码）。范围：`app/ctl` 控制面 + `sdk/go/lanet` 自身地址来源。
> 交接遗留项 P1-6 原文含义：0.5.74「Standalone 虚拟 IPv6 双栈」只做了**节点/TUN 侧**，
> 控制面（`app/ctl`）与 Android 客户端仍是 IPv4-only。

## 1. 现状（已核对到文件与行号）

### 1.1 已经具备的部分（0.5.74 起，不需要重做）

| 能力 | 位置 | 证据 |
|---|---|---|
| IPv6 ULA 前缀校验 | `pkg/tundevice/config.go` | `lanetULAIPv6Prefix = fd00:6c61:6e65::/48`，`ConfigureTUNIPv6` 强制落在其中 |
| TUN 配 IPv6 + 成员 /128 路由 | `pkg/tundevice/config.go` | `ConfigureTUNIPv6` / `EnsureRouteIPv6`（Windows 走原生 API，Linux/macOS 走 /48 接口路由） |
| 成员视图带 IPv6 | `pkg/serverless/vaddr.go` `MemberRef`、`pkg/netmapclient/client.go` `Member`/`Route` | 字段 `virtual_ipv6,omitempty` |
| 虚拟地址解析含 IPv6 | `pkg/serverless/vaddr.go` `ResolveTarget`、`pkg/netmapclient` `Resolve` | 同时匹配 `VirtualIP` 与 `VirtualIPv6` |
| 入向流别名 | `sdk/go/lanet/client.go` | `virtualIPAliasesByPeer` → `ServeInboundStreamAliases(v4, v6, stream)` |
| Standalone 自派生地址 | `pkg/serverless` | `DeriveVirtualIPv6(groupKey, peerID)`，`validVirtualIPv6` 只认自派生值 |

### 1.2 控制面的缺口（本方案要补的）

| 缺口 | 位置 | 说明 |
|---|---|---|
| 不分配 IPv6 | `app/ctl/internal/logic/node/registry.go` | `NewRegistry` 显式要求「IPv4 /24」，`nextIP` 只在 10.7.x.2~254 里找 |
| 成员模型无 IPv6 字段 | `app/ctl/internal/model/model.go`、`app/ctl/internal/logic/group/registry.go`、`app/ctl/internal/logic/node/registry.go` | `NodeView`/`MemberView`/`Node` 只有 `VirtualIP` |
| 不落库 | `app/ctl/internal/logic/group/store.go` | members 表只有 `virtual_ip`；schema v1 |
| API 不返回 | `app/ctl/api/group/v1/group.go` | create/join/netmap 响应都没有 `virtual_ipv6` |

### 1.3 关键发现：即使控制面返回了 IPv6，数据面今天也不会用

`sdk/go/lanet/client.go` 在**控制面模式**下：

- 自身地址取 `selfVirtualIPv6()` → `disc.SelfVirtualIPv6()`，即 **Standalone 的 (群密钥, PeerID) 派生值**
  （`client.go:643`），并把 TUN 配成这个值（`client.go:1066`）；
- 只为「与自身不同的成员 `VirtualIPv6`」写 /128 路由（`client.go:1113`）——控制面今天不返回该字段，
  所以控制面模式下 IPv6 路由表是**空的**，IPv6 数据面处于未启用状态。

结论：P1-6 = **控制面分配 + SDK 采用控制面分配的地址**，两件都必须做，否则只是「字段好看」。

## 2. 目标与非目标

**目标（本次要做）**

1. 控制面为每个群组分配一个 IPv6 /64，为每个成员分配该 /64 内的 /128，并持久化、返回、可回收。
2. SDK 控制面模式改用控制面分配的自身 IPv6；控制面未返回时（旧控面）回退到原派生值，行为不回退。
3. 文档与测试覆盖；真机验证到「两个控制面客户端 IPv6 互通」。

**非目标（明确不做）**

- Android/H5 客户端改造：Android 不在本仓库。本方案只保证 API **向后兼容**（新增可选字段，
  旧客户端忽略；旧客户端字段缺失时控制面仍按 IPv4 工作）。
- 控制面 HTTP 监听地址：`app/ctl/manifest/config/config.yaml` 是 `":8000"`，Go 的 `:port`
  通配监听在 Linux/Windows 都同时接受 IPv4 与 IPv6 连接，因此不属于「IPv4-only」缺口。
- 不改 IPv4 分配结果：同一成员的 `virtual_ip` 与今天完全一致（新增列，不动 `nextIP`）。
- 不改 Standalone 模式语义：派生地址与 `validVirtualIPv6` 保持不变。

## 3. 地址方案（决策点 D1）

- 组前缀：`fd00:6c61:6e65:<subnetIndex>::/64`
  - `subnetIndex` 复用现有 `nextSubnet`（0~255），与该组 IPv4 的 `10.7.<subnetIndex>.0/24` 一一对应，
    排障时一眼能对上；`fd00:6c61:6e65::/48` 有 65536 个 /64，容量与现有 256 组上限比绰绰有余。
- 成员地址：`fd00:6c61:6e65:<subnetIndex>::<host>`，`host` 与 IPv4 主机号**同值**（2~254，十六进制打印）：
  - 例：组 7 的 IPv4 `10.7.7.5` ↔ IPv6 `fd00:6c61:6e65:7::5`；
  - 规避 `::0`（子网任播）与 `::1`（保留），从 2 起、到 254 止，每群上限 253 人与 IPv4 相同；
  - 落在 `fd00:6c61:6e65::/48` 内，`tundevice.validateULAIPv6` 必然接受。
- 单一来源常量（决策点 D2）：新增到 `pkg/protocol`（节点侧与控制面共用，`pkg/tundevice` 改为引用它）：
  - `protocol.LanetULAIPv6Prefix = "fd00:6c61:6e65::/48"`（netip.Prefix）
  - `protocol.GroupIPv6Prefix(subnetIndex int) netip.Prefix` → `/64`
  - `protocol.MemberIPv6(subnetIndex, host int) netip.Addr` → `/128`
  - 依据：`pkg/tundevice/config.go` 已定义过同一常量（未导出，控制面拿不到），两处各写一份必然漂移。

## 4. 改动清单

### 4.1 控制面（app/ctl）

| 文件 | 改动 |
|---|---|
| `internal/logic/node/registry.go` | `Node` 增 `VirtualIPv6`；`Registry` 增 `prefix6 netip.Prefix` + `usedIPv6 map[netip.Addr]string`；`NewRegistry(cidr, ipv6Prefix string, tokens)`；`Enroll` 同时分配 v4/v6；`RestoreNode` 恢复并校验 v6 在组 /64 内、不与他人冲突（旧数据 v6 为空时按 `host` 补算，保证升级后可回填）；`RemoveNode` 同时回收；`List` 排序用 v4 保持现有顺序 |
| `internal/logic/group/registry.go` | 创建群组时算组 /64；`MemberView` 增 `VirtualIPv6`；`NetMap` 增 `CIDRv6`；`Group` 增 `CIDRv6`；insert/restore 传 `VirtualIPv6`；NetMap 填充该字段 |
| `internal/logic/group/store.go` | `memberRow` 增 `VirtualIPv6`；INSERT/SELECT 列表加列；INSERT 语句显式列名（保证旧版本二进制读到新库仍能工作） |
| `internal/logic/group/migrate.go` | 追加 `{version: 2, name: "members.virtual_ipv6", up: "ALTER TABLE members ADD COLUMN virtual_ipv6 TEXT NOT NULL DEFAULT ''"}`；不改历史迁移 |
| `internal/model/model.go` | `NodeView`/`MemberView` 增 `VirtualIPv6 string \`json:"virtual_ipv6,omitempty"\`` |
| `api/group/v1/group.go` | `GroupView` 增 `cidr_v6`；`KickRes` 增 `virtual_ipv6`（被回收的地址）；其余响应经 model 视图自动带上 |
| `internal/controller/group/group.go`、`internal/service/group.go` | 透传新字段（如当前是逐字段拷贝则补一行） |
| `app/ctl/README.MD` | 补 `virtual_ipv6`/`cidr_v6` 字段说明、schema v2 迁移说明、`dbversion` 输出变化 |

### 4.2 SDK（sdk/go/lanet）

| 文件 | 改动 |
|---|---|
| `client.go` | 控制面模式：自身 IPv6 = 控制面返回的 `virtual_ipv6`（create/join 响应 + 首次 netmap 校准），为空时回退 `selfVirtualIPv6()`；`Info()`/控制台展示同源；新增日志（沿用现有中文日志风格） |
| `client.go` | 成员 IPv6 路由：现有逻辑已按 `m.VirtualIPv6 != selfIPv6` 增量写 /128 路由，自身地址改了之后自然生效；补一条「控制面模式下 IPv6 就绪」的日志便于排障 |

### 4.3 文档

| 文件 | 改动 |
|---|---|
| `README.md` | 控制面章节补虚拟 IPv6（地址方案、字段名、与 Standalone 的差异）；虚拟地址表补 IPv6 一列 |
| `docs/control-plane-ipv6-dual-stack.md` | 本方案落档（确认后把「状态」改为已实施，并补真机证据） |

## 5. 兼容性与迁移

- **旧客户端 → 新控制面**：忽略新字段即可，IPv4 行为不变（新字段可省略、旧字段不动）。
- **新客户端 → 旧控制面**：`virtual_ipv6` 为空 → 回退 Standalone 派生值（即今天的行为），不报错。
- **旧库 → 新库**：schema v1 自动升 v2（`ALTER TABLE ADD COLUMN ... DEFAULT ''`），已有成员保留 IPv4 分配，
  IPv6 在首次加载/续期时按 `host` 规则补齐并落库；`dbversion` 应打印 2。
- **回滚**：新库用旧二进制打开仍可读写（旧代码的 INSERT 列出显式列，不会碰到新列）；
  如需彻底回滚，`virtual_ipv6` 列留着不影响旧逻辑。

## 6. 验收标准（按级判定，全部满足才算完成）

**L1 单元测试（必须）**

- `app/ctl/internal/logic/node`：IPv6 落在组 /64 内；与 IPv4 主机号一一对应；唯一性；成员退出后地址可复用；
  池耗尽（253 人）报错明确；`RestoreNode` 恢复旧数据（v6 列为空）时按规则补齐。
- `app/ctl/internal/logic/group`：create/join 后 netmap 每个成员都有 `virtual_ipv6` 且互不相同；
  踢人后该地址可再次分配；重启（重开库）后地址不变。
- `app/ctl/internal/logic/group/migrate_test.go`：构造 v1 库 → 迁移 → 列存在、旧数据完整、`dbversion=2`。
- `pkg/protocol`：`GroupIPv6Prefix`/`MemberIPv6` 的表驱动用例（边界：0、255、host 2/254）。
- `sdk/go/lanet`：控制面模式自身地址取控制面值、为空时回退派生值（用 httptest 假控制面）。
- 全仓 `go vet ./...` + `go test -count=1 ./...` 通过；`GOOS=linux go vet`（`tools/run-node-tests.ps1 -LinuxVet`）通过。

**L2 控制面真机（必须）**

1. 本地起控制面：`PVN_CTL_DB=<临时库> go run ./app/ctl`（监听 `:8000`）。
2. `POST /v1/groups/create`（peer A）→ 响应 `creator.virtual_ipv6` 形如 `fd00:6c61:6e65:0::2`。
3. `POST /v1/groups/join`（peer B，用返回的邀请码）→ `member.virtual_ipv6` 形如 `fd00:6c61:6e65:0::3`。
4. `GET /v1/groups/netmap?peer_id=A` → 两名成员都有 IPv6，且与 create/join 返回一致。
5. 重启控制面进程 → 同一 netmap 的 IPv4/IPv6 与重启前**逐字节一致**。
6. `POST /v1/groups/kick`（踢 B）→ 响应带被回收的 `virtual_ipv6`；再 join 新 peer → 复用到同一地址。
7. 用一个真连本地控制面的 `netmapclient.Refresh` 拉取（真 HTTP，不是假服务）→ `Members[i].VirtualIPv6` 非空且可在
   `netmapclient.Resolve` 里命中。

**L3 SDK 数据面真机（必须，决定「IPv6 真的通了」）**

- 用 `sdk/go/lanet` 起两个控制面模式客户端（同一台机器，管理员权限，TUN 名称不同），断言：
  1. 两端控制台/`Info()` 显示的虚拟 IPv6 与 netmap 分配一致；
  2. 本机 `netsh interface ipv6 show address` / `ip -6 addr` 能在 TUN 上看到该 /128；
  3. `ping6 fd00:6c61:6e65:0::3`（从 A 打 B）→ **有回应**；
  4. 反向 `ping6` 也通；杀掉 B 后 A 的 `ping6` 失败（确认不是本机自环假通）。

**L4 Android（说明项，不在本仓库）**

- 只验证 API 兼容性：Android 旧版本对新控制面仍能建组、加入、拿到 IPv4 并通信（不做 IPv6）。

## 7. 风险与对策

| 风险 | 对策 |
|---|---|
| 控制面分组 IPv6 与 Standalone 派生地址在同一 /48 内，理论上可能撞车 | 两者不会同时存在于同一网络：控制面模式不用派生值（改后），Standalone 模式不读控制面；且组 /64 与派生值分布不同。L3 验收顺带确认 |
| SDK 自身地址改变导致「旧成员按派生值找我这台新节点」失败 | 控制面模式下所有成员都以 netmap 为准（同一份分配），不存在混用；回退分支只服务旧控面 |
| 迁移失败把库写脏 | 复用现有迁移框架（逐步进版本、失败即停并标脏），迁移测试覆盖；只追加列不做数据改写 |
| 真机 L3 需要两个 TUN 实例，可能与正在运行的生产节点抢网段 | 用 Standalone 之外的临时网络/临时控制面库，TUN 名称用 `lanet-p1-6-a/b`，验收结束立刻停掉并清理 |

## 8. 提交与发布计划

1. `pkg/protocol` 常量 + 表驱动测试（1 提交）
2. 控制面 IPAM/模型/存储/迁移/API/文档 + 单测（1~2 提交）
3. SDK 控制面模式自身 IPv6 + 单测（1 提交）
4. 文档（README + 本文档状态与真机证据）（1 提交）
5. VERSION → 0.5.84；推送后确认 release/docker 两条流水线全绿、资产齐全
6. 生产节点（本机 0.5.83）再走一次 `/api/update/apply` 升到 0.5.84，记录 sha256 与发行包对账

## 9. 待确认的决策点

- **D1 地址方案**：`fd00:6c61:6e65:<subnetIndex>::<host>`（与 IPv4 主机号同值）是否认可？
  备选：组内独立分配（不与 IPv4 对齐，可读性差但解耦）。
- **D2 常量位置**：放 `pkg/protocol` 并让 `pkg/tundevice` 引用（单一来源）是否认可？
- **D3 SDK 采用控制面地址**：控制面模式下自身 IPv6 改为「控制面分配，缺省回退派生值」，是否认可？
  （不改这一条，控制面返回的 IPv6 只是装饰，数据面依旧不通。）
- **D4 验收强度**：L3「真机 ping6 互通」是否作为必须项？（需要两个 SDK 客户端实例 + 管理员权限）
- **D5 版本号**：按 0.5.84 发布，可以吗？
