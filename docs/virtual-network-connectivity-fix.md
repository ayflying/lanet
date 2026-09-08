# 虚拟局域网互通问题排障复盘

本文记录 Lanet 虚拟局域网从“节点能发现、应用流能通，但系统层虚拟 IP 互相 ping 不通或偶发丢包”到 0.5.8 稳定验证通过的完整排障过程。重点覆盖 Windows Wintun、Linux TUN、libp2p 隧道、离线成员隔离和节点重启。

## 1. 问题现象

早期版本出现过以下现象，表面上都是“虚拟 IP 不通”，实际属于不同层次的问题：

- 成员表能看到对端，libp2p 应用流或探测 echo 可以成功，但 Windows/Linux 的 `ping 10.7.x.x` 失败；
- Windows 单播 100% 丢包，进程日志几乎没有异常，邻居表却处于 `Probe`、`Unreachable` 或不完整状态；
- Linux 一端能发包，另一端的回包没有进入本地 TUN，表现为单向通或双向都不通；
- 一个离线成员被访问时，TUN 唯一读取循环被慢拨号阻塞，在线成员也随机丢包；
- 节点把 `10.7.0.0/16` 虚拟地址作为 libp2p 底层地址通告，可能通过 TUN 再拨回同一隧道，形成递归拨号；
- `/api/restart` 返回“正在重启”，但 Linux 旧进程退出后没有新进程接管；
- 升级或重启反复打开新的控制台浏览器页签；
- `xa.lanet` 可由 Lanet SDK 的 `Dial` 解析，但系统自带 `ping xa.lanet` 不能解析。

这些现象不能用单个“网络不通”结论处理，必须分别检查系统路由、邻居解析、TUN 读写格式、隧道流生命周期、拨号并发、底层地址通告和进程生命周期。

## 2. 定位方法

排障采用以下顺序，避免只在应用层重复测试：

1. **确认发现层和应用层**：检查成员表、PeerID、虚拟 IP、直连/中继路径，再用固定长度 echo 验证隧道字节流。
2. **确认系统数据面**：分别从 Windows 和 Linux 发起 ICMP、TCP、UDP，检查是否单向、是否只有跨 `/24` 失败，并记录 TUN 接口、路由和邻居状态。
3. **拆开读写路径**：确认 TUN 出向包是否被读取、是否按目标 IP 拨号、入向流是否写回 TUN、Linux 是否带正确的 virtio 头偏移。
4. **制造压力和离线目标**：同时访问在线和离线成员，观察一个失败目标是否阻塞其他目标；对同一流并发写入，检查包交错和读写竞态。
5. **检查 libp2p 底层地址**：过滤并阻断 `10.7.0.0/16` 地址，确认不会把 overlay 地址当成 underlay 传输地址。
6. **验证生命周期**：调用 `/api/restart`、`/api/quit`，检查 PID、API 恢复时间、虚拟 IP 是否保持，以及是否重复打开浏览器。

关键原则是：应用流成功只证明 libp2p 隧道成功；只有系统层 ICMP/TCP/UDP 在两端真实 TUN 上通过，才算虚拟局域网数据面完成。

## 3. 根因与修复

### 3.1 虚拟地址规划与 Windows 路由语义

虚拟 IP 在 `10.7.0.0/16` 中确定性派生，成员可能分布在不同 `/24`。早期按 `/24` 配置接口时，跨 `/24` 的包被操作系统送往真实网络网关，而不是 TUN。后来统一为整个 `10.7.0.0/16` 虚拟域，解决了跨子网路由错误。

Windows Wintun 是三层 L3 接口，没有真正的二层 ARP。即使存在 on-link `/16`，Windows 仍可能先对目的 IP 做邻居解析；邻居项无效时，单播包会卡在 `Probe` 或 `Unreachable`，TUN 进程也看不到可转发的包。

最终采用与可工作的 WireGuard 配置一致的形态：本机地址使用 `/32`；每个成员增量添加目的 IP `/32` 的 on-link 路由；通过 Windows IP Helper API 写入永久邻居项；原生 API 失败时保留 `netsh` 回退；删除历史聚合 `/16` 路由和旧邻居；同时设置 MTU 1400 和接口 metric。成员维护循环不先删后加，避免正常刷新制造路由窗口。

相关实现：`pkg/tundevice/config.go`、`pkg/tundevice/address_windows.go`、`pkg/tundevice/route_windows.go`、`pkg/tundevice/neighbor_winapi_windows.go`。

### 3.2 Linux TUN 回程包没有真正写入协议栈

Linux 端有两个独立问题：

1. `ip addr add` 只配置地址，不会自动把接口拉起；接口保持 `DOWN` 时，内核把发往 `10.7.0.0/16` 的包丢在接口上。
2. wireguard/tun 的 Linux 实现启用了 `IFF_VNET_HDR`，`Write` 要求数据起始偏移至少为 10 字节。偏移 0 会返回 `invalid offset`，收到的回程包因此无法进入 TUN。

修复为：地址配置后立即执行 `ip link set <tun> up`；Linux 读写使用 `virtioNetHdrLen=10` 的偏移，包数据放在 `[10:]`，由 TUN 库编码前置 virtio 头；Windows/macOS 仍使用偏移 0；入向和出向统一走同一套写入逻辑。

相关实现：`pkg/tundevice/config.go`、`pkg/tundevice/router.go`、`pkg/tundevice/address_other.go`。

### 3.3 隧道流方向、分帧和并发写入

libp2p 是字节流，单次 `Read` 可能得到半个包、多个包或合并后的边界。早期入向流也没有稳定注册到对应虚拟 IP，导致对端发来的请求到达 libp2p 后没有可靠的回程路径。

修复包括：

- 以对端 PeerID 查找虚拟 IP，把入向隧道注册到该 IP 的 `streamState`；
- 使用 `ipFramer` 按 IPv4 total length 重新切分字节流；
- 入向包通过统一防火墙后再写回 TUN；
- 同一条流的包写入使用 `writeMu` 串行化，避免多个 goroutine 交错写坏 IP 包；
- TUN 设备的所有 `Write` 使用设备级锁，符合 Wintun/NativeTun 的单写者要求；
- 同一虚拟 IP 使用 single-flight 拨号，多个并发包只触发一次真实拨号；
- 流断开时只清理触发退出的流，避免旧 goroutine 误删新流。

相关实现：`pkg/tundevice/router.go`、`pkg/tundevice/framer.go`、`pkg/tunnel/service.go`。

### 3.4 离线成员阻塞在线成员

TUN 读取循环只有一个。访问离线成员时，拨号可能经历多地址尝试、中继预约和超时；如果同步等待，其他成员的包无法从 TUN 继续取出，表现为一个失败目标拖垮整个虚拟网。

修复为按目的虚拟 IP 建立独立、有界、保序的出向 worker：不同目标可以并行拨号；同一目标按序发送；队列满时丢弃新包让 TCP/上层协议重传；worker 总数有上限且空闲回收；入队前复制 TUN 复用的读取缓冲；失败日志按首条和每 100 次限频，并截断 libp2p 超长错误。

相关实现：`pkg/tundevice/router.go`、`pkg/tunnel/service.go`。

### 3.5 overlay 地址被当作 libp2p underlay

`10.7.0.0/16` 只能承载已经建立的虚拟网络流量，不能作为 libp2p 建立底层连接的传输地址。旧节点可能已经把这类地址通告到 DHT 或 Identify；只在发现入口过滤不够，旧地址仍可能留在 peerstore 中。

修复采用三层防线：

1. `FilterUnderlayAddrs` 在 Host 地址工厂中过滤 `10.7.0.0/16`；
2. Standalone 发现收到成员地址时再次过滤，并清理 peerstore 中已存在的 overlay 地址；
3. `ConnectionGater` 在拨号、接收、加密和升级阶段都拒绝 overlay multiaddr，防止旧节点重新注入。

相关实现：`pkg/p2pkit/host.go`、`pkg/serverless/serverless.go`。

### 3.6 Linux 重启没有拉起新进程

`/api/restart` 会先返回响应，再延迟启动自身并退出。Windows 有独立的进程创建和 UAC 提权路径，但非 Windows 的 `spawnSelfWindows` 旧实现直接返回 `nil`，导致 Linux 只退出、不启动新实例。

修复为非 Windows 平台使用当前可执行文件和原参数启动独立进程组；Windows 保留 `CreateProcess`，遇到 740 提权错误时回退 `ShellExecute("runas")`。

相关实现：`app/agent/cmd/pvn-node/spawn_other.go`、`app/agent/cmd/pvn-node/spawn_windows.go`。

### 3.7 控制台页签重复打开

控制台 URL 每次进程启动都存在，不能用“本进程是否已经打开过”判断升级/重启场景。正确条件是这次启动是否新生成了配置文件。

修复为 `shouldAutoOpenConsole(cfgCreated)`：配置文件新生成时允许自动打开；配置文件已存在时，普通启动、升级重启、控制台重启和退出后再次启动都只记录跳过，不打开新页签。

相关实现：`app/agent/cmd/pvn-node/console_autopen.go`、`app/agent/cmd/pvn-node/console_autopen_test.go`。

## 4. 测试补齐

### 4.1 单元、竞态和静态检查

覆盖了 TUN 双向转发、入向流注册、字节流分帧、离线目标隔离、读取缓冲数据所有权、worker 上限/队列满/空闲回收、overlay 地址过滤、防火墙、Standalone 网络密钥和私有发现、身份文件、控制面数据库迁移及首次启动页签判断。

远程 Linux 构建机结果：

```text
go test ./...       PASS
go test -race ./... PASS
go vet ./...        PASS
```

同时完成了 `windows/amd64`、`linux/amd64`、`linux/arm64` 的官方节点交叉编译。

### 4.2 E2E 和真实节点

仓库专项入口：

```bash
go run ./app/agent/cmd/pvn-e2e-check
go run ./app/agent/cmd/pvn-serverless-check
go run ./app/agent/cmd/pvn-firewall-check
go run ./app/agent/cmd/pvn-identity-check
```

真实 Windows/Linux 节点使用 0.5.8 发行产物验证：

| 项目 | 结果 |
|---|---|
| Windows → Linux ping | 100/100，0% 丢包 |
| Linux → Windows ping | 100/100，0% 丢包 |
| Linux 高频 ping | 500/500，0% 丢包 |
| DF ICMP 1200/1360 字节 | 双向各 10/10，0% 丢包 |
| TCP/UDP 虚拟 IP echo | Windows → Linux、Linux → Windows 均通过 |
| 离线成员隔离 | 在线目标 30/30，离线目标不阻塞 |
| Windows/Linux 重启恢复 | 新 PID 接管，API 恢复，等待收敛后 20/20 |
| 控制台 API | state/config/update、CRUD、非法输入、认证均符合预期 |

重启不是热切换。API 通常约 2 秒恢复，但新 TUN、P2P 连接、路由和邻居表还需要短暂收敛；实测等待约 10 秒后双向 ping 恢复 0% 丢包。生产调用方应把重启视为短暂中断并实现重试。

## 5. 结果与边界

这次修复解决的是 Windows/Linux 节点之间通过 `10.7.x.x` 虚拟 IP 进行系统层通信，覆盖 ICMP、TCP、UDP、直连和中继回退，不代表数学意义上的“没有任何 bug”。以下边界仍然成立：

- `.lanet`、短名和原始成员名是 Lanet SDK 内部成员表解析，不会自动注册到 Windows/Linux 系统 DNS；系统自带 `ping <name>.lanet` 需要额外 DNS 或 hosts；
- SDK 默认不开 TUN，真实 TUN 需要 Windows 管理员权限或 Linux `/dev/net/tun` 与 `CAP_NET_ADMIN`；
- `allow-all` 便于联调但不适合生产，远程控制台必须设置密码并限制主机防火墙来源；
- 离线目标的首次拨号仍可能产生自身超时，修复目标是隔离影响范围，而不是让不存在的节点变成可达；
- 长时间吞吐、复杂 NAT 下 WebRTC、三层物理子网路由仍属于独立验证范围。

## 6. 后续排障顺序

遇到新的“虚拟 IP 不通”问题时，按以下顺序检查：

1. 两端 `/api/state` 是否看到相同网络、正确虚拟 IP 和 `online=true`；
2. 日志确认直连或 relay 是否建立；
3. Windows 检查 TUN `/32` 路由、永久邻居、MTU；Linux 检查 `ip addr` 和 `ip link` 是否为 `UP`；
4. 检查目标 IP 是否在 `10.7.0.0/16`，以及 libp2p 通告地址中是否混入 overlay 地址；
5. 用应用流 echo、TCP、UDP、ICMP 依次定位是隧道、传输层还是系统 TUN 层；
6. 同时压测在线和离线目标，确认是否是单个慢拨号造成全局阻塞；
7. 检查日志是否出现 `invalid offset`、邻居 `Unreachable`、递归拨号或重复页签相关信息；
8. 最后检查代理、公共 DHT、relay 候选和外部网络策略。外网连接失败时使用本机代理 `HTTP_PROXY/HTTPS_PROXY=http://127.0.0.1:10808` 重试。
