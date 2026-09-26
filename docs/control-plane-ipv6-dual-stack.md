# 控制面 IPv6 双栈：验收方案（P1-6）

> 状态：**已实施并完成 L1/L2/L3 验收，随 0.5.84 发布**。范围：`app/ctl` 控制面 + `sdk/go/lanet` 自身地址来源。
> 交接遗留项 P1-6 原文含义：0.5.74「Standalone 虚拟 IPv6 双栈」只做了**节点/TUN 侧**，
> 控制面（`app/ctl`）与 Android 客户端仍是 IPv4-only。
>
> 实施记录（0.5.84）：地址方案与字段名按 §3/§4 落地；控制面侧见提交
> 「控制面分配虚拟 IPv6」；SDK 侧除 create/join 响应外，还加了「首次 NetMap 校准」
> （响应缺字段但 netmap 有时采用 netmap 值），避免 IPv6 数据面静默失效。

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
- **新客户端 → 旧控制面**：`virtual_ipv6` 为空。注意控制面模式没有本地派生来源
  （`c.disc == nil`，`selfVirtualIPv6()` 只能拿到空串），因此自身 IPv6 保持为空：
  不配 TUN v6、不写成员 v6 路由，行为与升级前完全一致（仅 IPv4），不报错。
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

**L1 结果（0.5.84，本地实跑）**

| 包 | 用例 | 结果 |
|---|---|---|
| `pkg/protocol` | `TestGroupIPv6Prefix`、`TestMemberIPv6MirrorsIPv4Host`、`TestMemberIPv6DistinctAcrossGroups` | PASS |
| `app/ctl/internal/logic/node` | `TestEnrollAllocatesPairedIPv6`、`TestRestoreNodeBackfillsMissingIPv6`、`TestRemoveNodeReclaimsIPv6Pair`、`TestNewRegistryRejectsBadIPv6Prefix`、`TestEnrollPoolExhaustionKeepsPoolsInSync` | PASS |
| `app/ctl/internal/logic/group` | `TestNetMapExposesVirtualIPv6`、`TestKickReclaimsIPv6ForNextMember`、`TestPersistentRegistryKeepsIPv6AcrossRestart`、`TestLegacyDBBackfillsMemberIPv6`（v1 库 → 迁移 → 补齐并落库） | PASS |
| `sdk/go/lanet` | `TestNewCreatesGroup`、`TestNewJoinsGroupWithInvite`（采用控制面分配值）、`TestNewAdoptsIPv6FromNetMapWhenResponseLacksIt`、`TestNewWithoutControlPlaneIPv6FallsBackToIPv4Only` | PASS |

`go test -count=1 ./...`、`tools/run-node-tests.ps1 -LinuxVet` 通过；`-race` 本机
`CGO_ENABLED=0` 且无 gcc 无法本地跑，由 CI 的 ubuntu stability 任务覆盖。

**L2 结果（0.5.84，真控制面进程 `PVN_CTL_ADDR=:18000` + 临时库）**

| 步骤 | 实测 |
|---|---|
| create | `group: 10.7.0.0/24` + `cidr_v6: fd00:6c61:6e65::/64`；`creator: 10.7.0.2 / fd00:6c61:6e65::2` |
| join | `member: 10.7.0.3 / fd00:6c61:6e65::3` |
| netmap | 两名成员的 `virtual_ip`/`virtual_ipv6` 与 create/join 完全一致 |
| 重启控制面 | netmap JSON **逐字节一致**（`cidr_v6`、两名成员地址均不变） |
| 老库模拟（`UPDATE members SET virtual_ipv6=''` 后重启） | netmap 仍给出 `::2`/`::3`，且库内该列**已被补齐写回**；`dbversion` 打印 `version=2` |
| kick B | 响应 `virtual_ip=10.7.0.3` + `virtual_ipv6=fd00:6c61:6e65::3`；随后 join 的新成员**复用了同一对地址** |
| 真 `netmapclient.Refresh` | 两成员 `VirtualIPv6` 非空；`Resolve("fd00:6c61:6e65::3")` 与 `Resolve("10.7.0.3")` 都命中同一 peer |

**L3 结果（0.5.84，WSL Docker 内两个托管模式 SDK 客户端 + 真实 TUN + `ping6`）**

环境：`sdk/go/lanet` 临时客户端（`Tun=true`，TUN 名 `lanet-p1-6-a/b`，`FirewallMode=allow-all`）
跑在 WSL2 的两个容器里（`--cap-add NET_ADMIN --device /dev/net/tun`），控制面跑在 Windows 宿主
（`http://172.25.64.1:18000`），群组由 A 创建、B 凭邀请码加入。

| 断言 | 实测 |
|---|---|
| `Info()` 与 netmap 分配一致 | A `fd00:6c61:6e65::2`、B `fd00:6c61:6e65::3`，与 netmap 逐字段一致 |
| TUN 上的 /128 | `ip -6 addr`：A `fd00:6c61:6e65::2/128`、B `fd00:6c61:6e65::3/128`；`ip -6 route` 有 `fd00:6c61:6e65::/48 dev lanet-p1-6-*` |
| A → B `ping6` | **4/4 收到，0% 丢包**（rtt 1.7~2.6ms） |
| B → A `ping6` | **4/4 收到，0% 丢包**（rtt 0.5~0.8ms） |
| IPv4 基线对照 | A → B `ping 10.7.0.3` 2/2 收到，确认两条栈走同一隧道 |
| 隧道证据 | `[router] tunnel established to fd00:6c61:6e65::3 via peer=… remote=/ip4/172.20.0.3/udp/…/quic-v1` |
| 杀掉 B 后 A `ping6` | 3 发 0 收、100% 丢包；`[router] forward to fd00:6c61:6e65::3 failed … all dials failed` —— 确认不是本机自环假通 |

> 说明：本环境控制面没有可用 relay，而托管模式下 SDK 只在中继连接后经 identify 学到对端地址
> （`pkg/tunnel` 主动拨号刻意不带 Addrs，见其注释）。因此验收时由临时客户端按 NetMap 通告地址
> 显式 `Host().Connect` 一次（IPv4 同样需要）；这是环境前置条件，与 IPv6 无关。

**L4 Android（说明项，不在本仓库）**

- 只验证 API 兼容性：Android 旧版本对新控制面仍能建组、加入、拿到 IPv4 并通信（不做 IPv6）。
  本次未做（无 Android 环境）；新字段全部 `omitempty`，旧客户端忽略即可。

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
5. VERSION → 0.5.84；推送后确认 release/docker 两条流水线全绿、资产齐全 ✅
   （`78d1054`：release/docker 均 success；v0.5.84 五个资产齐全，windows zip sha256 `22dfcb66…`）
6. 生产节点（本机 0.5.83）再走一次 `/api/update/apply` 升到 0.5.84，记录 sha256 与发行包对账 ✅
   - 日志：`[update] sha256 校验通过 22dfcb66d8c9a272…`（= 发布页 windows zip 哈希）；
     `[service-restart] 已在服务启动前切换到暂存版本` → `服务已重新启动`；
     `[node] 启动 … version=0.5.84`；
   - 服务 Running；虚拟 IP `10.7.207.102`、PeerID `12D3KooWJVB9…` 均未变；
   - 已安装 `lanet.exe` 哈希 `3756d1f0478cb1496342864a34a9907559156133628cac4fc48f19e76c0812ed`
     **等于**发布包内 exe 哈希；`.rollback` 保留。

## 9. 决策点（已确认，记录结论）

- **D1 地址方案**：`fd00:6c61:6e65:<subnetIndex>::<host>`（与 IPv4 主机号同值）—— 已采用。
- **D2 常量位置**：`pkg/protocol` 为唯一来源，`pkg/tundevice` 引用 —— 已采用。
- **D3 SDK 采用控制面地址**：托管模式自身 IPv6 = 控制面分配值；控制面未返回时为空
  （该模式没有本地派生来源），退化为仅 IPv4 —— 已采用。
- **D4 验收强度**：L1 + L2 + L3（真机 `ping6` 双向）均为必须项 —— 已全部完成。
- **D5 版本号**：0.5.84 —— 已发布。
