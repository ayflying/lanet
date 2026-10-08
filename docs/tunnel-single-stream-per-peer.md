# 隧道单流化：同一 peer 只保留一条双工流

> 状态：**已实施并通过单元测试**（随本次修复发布）。
> 触发事件：线上 `anyang-job-pc` 被误判为「一直在掉线」。

## 1. 现象

节点日志里，某个成员（实测 `anyang-job-pc`，虚拟 IPv4 `10.7.207.102`、
虚拟 IPv6 `fd00:6c61:6e65:240f:4528:3084:6637:72dc`，同一个 PeerID）
的隧道每 1~3 秒重建一次，IPv4 与 IPv6 交替出现：

```
15:08:00.544 [router] tunnel established to 10.7.207.102 peer=<peer.ID 12*enGQ3p> initiator=self ...
15:08:02.233 [router] tunnel established to fd00:6c61:...:72dc peer=<peer.ID 12*enGQ3p> initiator=self ...
15:08:02.532 [router] tunnel established to 10.7.207.102 ...
15:08:02.680 [router] tunnel established to fd00:6c61:...:72dc ...
```

伴随刷屏：

```
[router] forward to 10.7.207.102 failed (count=1): write to 10.7.207.102:
        stream reset (remote): code: 0x0: transport error: stream reset by remote, error code: 0
[probe] FAIL anyang-job-pc(10.7.207.102): direct and relay dial both failed
[probe] OK anyang-job-pc(10.7.207.102) via=relay rtt=47ms
```

**该节点并没有掉线**：成员表一直稳定在名单里（`[node] 成员表更新（4 人）: … anyang-job-pc@10.7.207.102 …`），
`[router] inbound tunnel established from 10.7.207.102` 持续出现，probe 在 FAIL 之后立刻 OK。
「掉线」是隧道被反复拆建造成的表象。

## 2. 根因

出向流按**目的 IP 字符串**建表，入向流却按 **v4+v6 两个别名归一**注册：

- 旧 `Router.streams map[string]*streamState` 以目的 IP 为 key。
  `streamTo`/`dialStream` 只登记自己拨出的那个地址 →
  发往 `10.7.207.102` 的包建流 S_v4，发往 `fd00:…:72dc` 的包又建流 S_v6，**同一 peer 出现两条流**。
- `ServeInboundStreamAliases(v4, v6, stream)` 注册时把同一个 state 绑到**两个 key**，
  并对任何不同 state 做 `delete` + `stream.Reset()`。

于是两端各自持有 S_v4/S_v6，任一新入向流注册时把同 peer 另一 key 上的老流 Reset 掉，
本地写失败 → `dropStream` → 下一个包重新拨号 → 又建立一条 → 再被 Reset。**两端同时做同样的事，互拆成死循环。**

`error code 0`（`0x0` = NO_ERROR）正是本程序自己 `Reset()` 派发的 STREAM_RESET，
不是协议拒绝或配额 —— 这也是它看起来像「对端在乱断」的原因。

次因：出向建流时若 NetMap 尚未下发对端 IPv6，流只会记住 v4；
若存活流没有并入旧流的别名，指向 IPv6 的下一个包又会再次拨号，复现同一种故障。
本次一并修掉（见 §4 的别名并集）。

## 3. 不变量与仲裁规则

> **任一时刻，每个对端 PeerID 恰好一条活跃双工流。**

同一对 peer 最多只会出现两条竞争流（各端自己拨的那条 + 对端拨进来的那条）。
仲裁规则：

1. 比较两条流的**发起方 PeerID**，字典序小者胜。
   发起方 = 该流的对端是拨号方时为对端 PeerID，本机是拨号方时为本机 PeerID。
2. 两端都知道双方 PeerID，因此各自算出的赢家**必然相同** →
   不可能出现「A 留自己拨的、B 留自己拨的」互相对拆。
3. 发起方相同（对端重拨）时保留在位的老流。该分支正常不会走到：
   `flowTo` 已按 PeerID single-flight 合并，重拨前也 Reset 了旧流；
   即便走到，失败的一方 `Close` 退场，重拨代价由 `runOutbound` 的退避兜住。

淘汰走 **`Close()` + 宽限期 `time.AfterFunc` 兜底 `Reset()`**，
**不用 Reset 淘汰**：否则对端收到 STREAM_RESET(code 0x0 = NO_ERROR)
会打出与本次故障一模一样、极具误导性的 `stream reset by remote, error code 0`。
`Close` 让对端读到 EOF 干净退出。

## 4. 改动

### `pkg/tunnel/service.go`（新增方法，未改接口）

| 方法 | 说明 |
|---|---|
| `PeerRoute(virtualIP string) (peer.ID, []string, bool)` | 虚拟地址 → 对端 PeerID + 全部别名（v4, v6）；内部走既有 `netmapCli.Resolve` |
| `LocalPeerID() peer.ID` | 本机 PeerID，供仲裁使用；`s == nil \|\| s.self == nil` 返回 `""` |

`GroupNetMap` 接口保持只有 `Resolve`，无破坏性变更。

### `pkg/tundevice/router.go`（核心重构）

| 项 | 改动 |
|---|---|
| 流表 | `streams map[string]*streamState` → `flows map[peer.ID]*peerFlow` + `flowByAddr map[string]peer.ID` |
| `streamState` → `peerFlow` | 增 `peer` / `initiator`（发起方）/ `addrs`（别名）/ `retired`；写计数改原子，写操作由 `writeMu` 串行 |
| 拨号归并 | `dialing map[string]*dialCall` → `map[peer.ID]*dialCall`，IPv4/IPv6 包并发也只拨一次 |
| `lookupPeer` | 先查 `flowByAddr`（稳态 O(1)），未命中才 `PeerRoute()` 并回填该 peer 全部别名，避免每包扫 NetMap |
| 别名并集 | `mergeAddrsLocked`：新旧流别名取并集，存活流不会因为「拨号时还没拿到 IPv6」而丢掉 IPv6 别名 |
| `installFlow` | 唯一的流安装入口，承载对称仲裁 + 别名并集 + 败者优雅退场 |
| 淘汰 | `retireFlow`（`CompareAndSwap` 幂等）、`scheduleForceReset` 宽限兜底；`reapIdle` 按 peer 回收 |
| `pumpFromStream` | 每次 Read 后先查 `retired`，被淘汰的流不再往 TUN 投递，避免同一份数据在协议栈出现两次 |
| 删除 | 旧 `dropStream(virtualIP, state)` |
| 常量 | 新增 `streamRetireGrace = 2 * time.Second` |

`outbound map[string]*outboundWorker` **仍按目的 IP** —— 出向队列必须保序，
改按 PeerID 会打乱同目的地包的先后关系。

日志格式也随之明确（便于排障时直接看出竞争关系）：

```
[router] tunnel established to %s peer=%s initiator=self via=%s remote=%s
[router] inbound tunnel established from %s peer=%s initiator=remote remote=%s
[router] tunnel replaced for peer %s: 保留发起方 %s，淘汰发起方 %s
[router] tunnel closed for peer %s (%s)
```

`ServeInboundStream(v4, stream)` 签名保留（`app/agent/cmd/pvn-agent/main.go` 在用）。
入向在拿不到别名或 `RemotePeer()` 时改用 `Close()` 而非 `Reset()`。

## 5. 验证

### L1 单元测试（`pkg/tundevice/router_peerflow_test.go`）

| 用例 | 断言 |
|---|---|
| `TestIPv4AndIPv6OfSamePeerShareOneTunnelStream` | **核心回归**：v4 建流后 v6 包复用同一流；流数 = 1；两个别名绑同一 PeerID；对端只被打开 1 条流 |
| `TestDualStackPeerIsNotDialedTwiceConcurrently` | 并发双栈包只拨一次，`dialing` 归零 |
| `TestInstallFlowArbitrationIsSymmetric` | **两端同时互拨**：A/B 视角（含反序安装）留存流的 initiator 都是 `min(idA, idB)`，流数恒为 1 |
| `TestArbitrationLoserIsClosedNotReset` | 败者被 `Close`、**未**被立刻 `Reset`；胜者不动；别名索引更新 |
| `TestRetiredFlowStopsDeliveringToTUN` | 淘汰后不再往 TUN 投递 |
| `TestLookupPeerResolvesBothAliasesToSamePeer` | v4/v6 解析到同一 PeerID，并互相回填别名索引（NetMap 失效后仍命中缓存） |
| `TestDialedStreamPumpsInboundPacketsToTUN` | **0.5.96 数据面回归用例（见 §6）**：对端写回本机拨出流的回包必须到达本机 TUN；去掉修复后该用例失败（录制 0 个入向包），红绿验证通过 |
| `TestDialedStreamPumpIsNotDuplicatedOnKeptFlow` | `ensurePump` 的 CAS 防双读：同一条流重复调用只启动一个读循环 |

原有 `router_budget_test.go` / `router_recovery_test.go` 用例已迁移到新结构并保持通过。

### L1 结果（本机实跑）

`go vet ./...` 通过；`go test -count=1 ./pkg/tundevice/ ./pkg/tunnel/` 通过
（`pkg/tundevice` 4.0s、`pkg/tunnel` 1.9s）；`go test -count=1 ./...` 仅
`app/agent/cmd/pvn-node` 因 Windows 下运行 `.exe` 需要提权而失败，与本次改动无关
（该包 `[no test files]` 之外的二进制启动限制）。

### 待验证（上线后）

线上节点日志应满足：

1. 同一 peer 的 `[router] tunnel established` 在稳态下**不再重复出现**（目标：降到 0 次重建）；
2. `stream reset by remote, error code 0` **0 条**；
3. `anyang-job-pc` 的 `[probe] OK … via=relay` 连续 5 轮无 FAIL。

## 6. 0.5.96 回归：拨出流读循环丢失（2026-10-07 线上故障）

> 状态：**已修复并通过红绿验证的单元测试**（随 0.5.97 发布）。
> 触发事件：`anyang-job-pc` 升级 0.5.96 后「控制台在线 + 直连，但 ping/TCP 全部超时」。

### 6.1 现象

- 控制台/probe 一切正常：所有在线成员 `[probe] OK … via=direct rtt=1~28ms`，
  隧道长连数小时不断；`[router] tunnel established … initiator=self via=direct`
  也正常出现。
- 但数据面纯单通：`ping`/TCP 100% 超时；TUN 网卡
  `Get-NetAdapterStatistics` 的 **ReceivedBytes 全天冻结在 7618**，
  发包持续增长、收包恒为 0（SendDelta 正常、RecvDelta=0）。
- 对端 TCP 进本机同样失败 —— 对端若是 0.5.96，它拨出的流同样没人读。

### 6.2 根因

§4 的重构删掉了旧 `dialStream` 里的读循环：

```go
// 旧实现（< 0.5.96）：入向：把对端发来的包写回 TUN。
go func() { defer r.pumps.Done(); r.pumpFromStream(virtualIP, state) }()
```

重构后 `pumpFromStream` 只剩 `ServeInboundStreamAliases` 一个调用点
（入向流同步 pump），**所有本机拨出的流只写不读**。libp2p 流是双工的：
远端把响应写回同一条流，本机却没有任何 goroutine 在读 —— 回包永远滞留在
流缓冲里。控制面（probe/控制台走独立应用流）完全不受影响，
于是呈现「在线 + 直连但 ping/TCP 全断」的割裂现象。

### 6.3 修复

| 项 | 改动 |
|---|---|
| `peerFlow` | 新增 `pumped atomic.Bool`：标记读循环是否已启动 |
| `ensurePump` | 新增：`pumped` CAS 防双读 → `pumps.Add(1)` + `go pumpFromStream(flow)`，幂等 |
| `dialStream` | `installFlow` 成功后对返回流调用 `ensurePump`：candidate 与仲裁复用的 kept 都要保证有读循环（kept 可能同样来自本机更早的拨号）；且在首次出向写入**之前**启动 |
| `ServeInboundStreamAliases` | 同步 pump 改为 `ensurePump`，与拨出路径共用同一套 CAS 守卫 |

不变量升级为：**任一时刻，每个对端 PeerID 恰好一条活跃双工流，且该流有且只有一个读循环。**

### 6.4 受影响版本与部署

- 仅 **0.5.96** 受影响（读循环随 56ec972 重构丢失）；≤ 0.5.95 旧实现两条路径都有读循环，行为正常。
- 修复随 **0.5.97** 发布；至少升级两台 Windows 0.5.96 节点（`anyang-job-pc`、`tianzong-pc`）。
- 升级前后的判别方法：`ping 对端虚拟 IP` 期间看本机 TUN 网卡的 ReceivedBytes 是否增长；
  或查节点日志有没有 `tunnel established … initiator=self` 之后仍然 ping 不通。

## 7. 相关文档

- `docs/control-plane-ipv6-dual-stack.md` —— 虚拟 IPv6 双栈的来源（`ServeInboundStreamAliases` 的入向别名能力出自其 §1.1）。