# 种子表与地址治理（0.5.51）

本文档说明「群内公网设备免好友共享」「全域种子表」两个能力的**接口、步骤、限制与验收方法**。

---

## 1. 要解决的问题

原状：

1. 想用某个公网设备当入口，必须先加好友（走连接审批），否则拿不到它的地址；
2. 容器里的节点会把 **docker 内网地址**（如 `192.168.64.2:4001`）当成自己的可用地址公告出去，
   外部节点照着拨必然失败（VPS 实测症状：`建连失败: failed to dial … 192.168.64.2:4001`）；
3. 没有跨网络密钥的入口，两个互不相同的网络密钥之间完全没有打通路径。

现在：

- **群内种子表**（`group_seeds`）：同一网络密钥内被**实测拨通过**的公网设备，
  无需加好友即可作为路由入口。
- **全域种子表**（`global_seeds`）：跨网络密钥的入口，**默认关闭**，由用户显式开启。
- **地址治理**：容器内网地址**直接剔除**（不是降级），可用 `LANET_ADVERTISE` 主动声明真实可达地址。

---

## 2. 两张表必须物理隔离

隔离在**三层**同时成立，任何一层单独生效都不算达标：

| 层 | 机制 |
|---|---|
| 存储层 | `group_seeds` / `global_seeds` 是两张独立表；未知 `scope` **报错不回退**到某张默认表 |
| 协议层 | 群内通道协议 ID 按群密钥派生（`GroupProtoID(BaseSeeds, groupKey)`）；全域走固定 ID `/lanet/seeds-global/1.0.0` |
| 消息层 | `seedPayload.Scope` 与当前通道不符 → **整批丢弃**（不在群内通道里接受自称 global 的记录） |

所以：关掉全域开关时，本机既不会分享全域种子，也不会从 DHT 路由表拉全域交换对象，
群内种子的流动完全不受影响。

---

## 3. 开关与设置

存放位置：地址簿（`lanet.db`）的 `app_settings` 表。

| 键 | 默认值 | 说明 |
|---|---|---|
| `group_seeds_enabled` | `1`（开） | 群内种子共享 |
| `global_seeds_enabled` | `0`（关） | 全域种子共享。开启后本机会在私有 DHT 的**全局 rendezvous key** 上广播自己的存在 |
| `global_seeds_limit` | `1000` | 全域种子表容量上限（硬天花板 20000） |

**为什么群内默认开、全域默认关**：同一网络密钥内的公网设备本来就是本群的合法入口，
共享它不增加新的可见性；而全域会让**不同网络密钥**的节点看到你的 PeerID 与地址，必须显式同意。

### 3.1 控制台接口

```http
GET  /api/seed-settings
→ { "settings": {...}, "db_path": "...", "scopes": [ {scope, enabled, limit, total, verified, public_reachable}, ... ] }

PUT  /api/seed-settings
   body 局部更新，字段都可省略：{ "group_enabled": true, "global_enabled": false, "global_limit": 1000 }
→ { "ok": true, "settings": {...} }

GET  /api/seeds?scope=group|global
→ { "scope": "group", "seeds": [ {...} ] }

POST /api/seeds
   body: { "scope": "group", "peer_id": "12D3Koo…" }   # 手动删掉一条误入的脏记录
→ { "ok": true }
```

设置**改完立即生效**（`PUT` 内部会重新装配回调），不需要重启进程。

### 3.2 SDK 接口

```go
c, _ := lanet.New(ctx, lanet.Config{ /* … */ })

s, err := c.SeedSettings(ctx)             // 读取
s.GlobalEnabled = true
s.GlobalLimit = 500
saved, err := c.SetSeedSettings(ctx, s)   // 保存并立即生效
```

---

## 4. 验证门（谁能被当成种子）

一条记录只有在**本机实测拨通过**（`ok_count > 0`）之后才有资格被分发出去。规则：

1. **好友永不入种子表**：已信任节点在成员表里本来就能直接拨，再存一份只会占名额。
2. **不采信对端自报**：交换来的记录一律按未验证入库，「你能拨通它」不等于「我也能拨通它」。
3. **不主动探测**：本机不做周期性拨号去"验证"种子。验证完全靠本地核对
   `Network().Connectedness()` —— 只要某条种子正好被连上（bootstrap、DHT 发现、中继预约……），
   立刻升级为已验证，并把**真正生效的地址**回写。
   这是"流量不爆炸"的关键：1000 条种子若做主动探测，等于 1000 条常驻连接。

---

## 5. 流量天花板（硬约束）

> 用户明确要求「不要让流量爆炸」。以下每个上限都有单测钉死（`TestSeedTrafficCeiling`），
> 后续改代码若把它们放大，测试会直接变红。

| 项 | 上限 |
|---|---|
| 单条消息记录数 | 64 |
| 单条记录地址数 | 4 |
| 单条消息字节数 | 64 KB（硬闸，读超即断） |
| 同一对端交换冷却 | 5 分钟 |
| 全局并发交换数 | 2 |
| 每轮每范围对端数 | 3 |
| 一轮周期 | 10 分钟 |
| 首轮延迟 | 45 秒 |
| 对端不支持时退避 | 1 小时 |
| 单次流超时 | 20 秒 |

**空载流量为 0**：没有成员、没有种子时 `seedPeerTargets` 返回空，整轮直接结束。

### 5.1 陈旧清理（自动剔除长期不上线）

| 档 | 容忍期 | 理由 |
|---|---|---|
| 未验证（`ok_count = 0`） | 7 天 | 从没拨通过，只是别人转发来的一条地址，别长期占名额 |
| 已验证（`ok_count > 0`） | 30 天 | 曾确认可用，容忍对方关机/出差 |

清理周期：每 15 轮核对（≈30 分钟）一次。容量溢出时另外按「未验证优先、其次最久没动的」淘汰。

---

## 6. 地址治理

`pkg/p2pkit/container.go`：

- `InContainer()`：判定顺序 `LANET_CONTAINER` 显式覆盖 → `/.dockerenv` → cgroup 关键字（含 `libpod`）。
- `AdvertiseAddrs()`：`LANET_ADVERTISE` 声明地址，形如 `1.2.3.4:4001`，
  自动展开 `tcp` + `quic-v1` 两条，上限 8 条；声明后**置顶**。

容器内网地址的处置是**剔除而非降级**。原因：容器内网卡名是 `eth0`，
不命中「宿主虚拟网卡」判定（按网卡名匹配 WSL/Hyper-V/docker 桥），
于是 docker 私网地址会落进高优先级档被学走 —— 外部节点照拨必失败。

### 6.1 容器节点如何对外可拨

容器映射了端口之后，用 `LANET_ADVERTISE` 声明宿主的公网地址：

```yaml
environment:
  LANET_ADVERTISE: "43.136.124.167:4001"
```

---

## 7. 中继候选去幽灵（同批修复）

- 候选来源：已验证种子（tier 0）→ 成员表（tier 1），剔掉 link-local / 回环 / 未指定 /
  `p2p-circuit` 地址，以及超过 `memberTTL` 未通讯的「幽灵成员」。
- `standalone` 形态（也就是发行版 `lanet`）**必须**走 `startRelayReservation`：
  它以前不预约中继，而 NODE 从 circuitv2 中继拿地址的前提是有预约，
  否则一律 `NO_RESERVATION(204)`。

---

## 8. 验收步骤

```bash
# 1. 单元 / 集成测试（本机需要先挪开 syso）
mv app/agent/cmd/pvn-node/rsrc_windows_amd64.syso /tmp/
go test ./...
go run ./app/agent/cmd/pvn-e2e-check      # 期望输出 e2e-ok
mv /tmp/rsrc_windows_amd64.syso app/agent/cmd/pvn-node/

# 2. 运行时（本机节点）
curl --noproxy '*' http://127.0.0.1:8900/api/seed-settings   # 看开关与两张表的统计
curl --noproxy '*' "http://127.0.0.1:8900/api/seeds?scope=group"

# 3. 日志判据
#    种子交换：种子表（group）入向：合并 N 条新种子（来自 …）
#    种子核对：N 条种子由「已连接」升级为已验证
#    种子表（global）已满，淘汰 N 条最差记录
#    种子表（group）清理了 N 条长期不上线的记录
#    种子交换已启用：仅群内 / 群内 + 全域（全域上限 N）
```

**流量断言（人工核对，10 分钟量级）**：空载状态下 `lanet.log` 里每分钟不应出现
持续成串的种子交换记录；有对端时一轮最多 3 条出向、且同一对端 5 分钟内不重复。

---

## 9. 已知限制

1. **换私有 DHT 前缀后新老版本互不可见**（0.5.48 起），升级要成批做。
2. 种子交换**不要求同群、不要求好友**，所以它也是一条信息通路；
   隐私边界靠「只分享本机实测拨通过的公网入口」+「协议 ID 派生」+「全域开关默认关」三重约束。
3. 无 C 编译器的机器上 `go test -race` 不可用（`-race requires cgo`），
   并发正确性靠锁纪律与重复跑测试保证。
