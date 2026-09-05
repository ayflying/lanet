# Lanet Go SDK

把任意 Go 程序变成 Lanet 群组网格中的一个节点：十几行代码完成入网、开流、接流。
与内置 agent 使用同一套 libp2p 协议栈（v0.44），隧道策略一致：
**直连优先 → 已有连接开流 → Circuit Relay v2 中继兜底**。

## 安装

```bash
go get github.com/ayflying/pvn/sdk/go/lanet
```

前置条件：已部署控制面（ctl）与中继（relay）。SDK 只需能访问 ctl（`CTLURL`），
relay 地址由 ctl 目录自动下发。无需 root/管理员权限（不创建 TUN 网卡）。

## 五分钟上手

### 场景 A：创建群组（本节点成为群主）

```go
package main

import (
	"context"
	"log"

	"github.com/ayflying/pvn/sdk/go/lanet"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := lanet.New(ctx, lanet.Config{
		CTLURL:    "http://ctl.example.com:8600", // 控制面地址（必填）
		Name:      "my-service",                  // 节点名称（必填）
		GroupName: "my-group",                    // 群组名称
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	info := client.Info()
	log.Printf("入网成功：虚拟 IP=%s，邀请码=%s", info.VirtualIP, info.InviteCode)
	// 把 info.InviteCode 分享给其他成员

	// 周期任务（刷新 NetMap / 通告地址 / 中继预约），阻塞直到 ctx 取消。
	client.Run(ctx)
}
```

### 场景 B：凭邀请码加入群组

```go
client, err := lanet.New(ctx, lanet.Config{
	CTLURL:     "http://ctl.example.com:8600",
	Name:       "another-node",
	InviteCode: "grp-xxxxxxxx", // 群主分享的邀请码；留空则创建新群组
})
```

### 场景 C：主动开流（echo 请求-响应）

```go
stream, viaRelay, err := client.Dial(ctx, "10.7.0.2")
if err != nil {
	log.Fatal(err)
}
defer stream.Close()

// 发送完毕调用 CloseWrite()，对端才能读到 EOF（半关闭语义，务必调用）。
_, _ = stream.Write([]byte("hello"))
_ = stream.CloseWrite()

// 读回程数据直到 EOF。
reply, _ := io.ReadAll(stream)
log.Printf("回复: %s (viaRelay=%v)", reply, viaRelay)
```

### 场景 D：接收入向流（提供 echo 服务）

```go
client.OnStream(func(stream lanet.Stream) {
	defer stream.Close()
	// 注意：对端 CloseWrite 后本端读到 EOF，此时再回写仍可送达。
	_, _ = io.Copy(stream, stream)
})
```

## Config 配置项

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `CTLURL` | string | —（必填） | 控制面地址，如 `http://127.0.0.1:8600` |
| `Name` | string | —（必填） | 节点名称，显示在 NetMap 中 |
| `OS` | string | `runtime.GOOS` | OS 标识 |
| `InviteCode` | string | `""` | 凭码加入群组；**留空则创建新群组** |
| `GroupName` | string | `""` | 创建模式下的群组名称（Join 模式忽略） |
| `ListenAddrs` | []string | tcp + ws + quic 全部随机端口 | 覆盖默认监听地址 |
| `WebRTC` | *bool | `true` | 启用 webrtc-direct 传输（浏览器节点可直连本节点） |
| `NetMapInterval` | time.Duration | `15s` | 周期任务（NetMap 刷新/通告/中继预约）间隔 |
| `DialTimeout` | time.Duration | `8s` | 开流超时 |
| `Quiet` | bool | `false` | 为 true 时不打日志 |
| `Standalone` | bool | `false` | 无服务器模式：不依赖 ctl/relay，DHT+mDNS 自动发现组网 |
| `NetworkKey` | string | `""` | Standalone 专用：网络密钥。留空 = 公共网络（所有留空节点互通）；相同密钥 = 私有网络 |
| `Channel` | string | `lanet.ChannelSDK` | Standalone 专用：分发渠道，参与群组身份派生。SDK 构建默认与官方发行版网络互相隔离（相同 NetworkKey 也不互通）；确需互通显式设为 `"official"` |
| `Bootstrap` | []string | `[]` | Standalone 专用：引导节点 multiaddr（已在网成员地址作为私有 DHT 种子；`lanet.DefaultBootstrap` 为公共引导） |
| `DisablePublicDHT` | bool | `false` | Standalone 私有网络专用：关闭公共 DHT 兜底，只用私有 DHT + mDNS 发现 |
| `LANForwards` | []LANForward | `[]` | 局域网转发初始映射表（`{Listen, Target}`），可热更新 |
| `ConsoleAddr` | string | `127.0.0.1:8900` | 内置 Web 控制台监听地址，`"-"` 关闭；占用时自动后移至 8910 |
| `StateFile` | string | `""` | 控制台状态持久化文件（防火墙/转发规则），空 = 仅内存 |
| `FirewallMode` | FirewallMode | `deny-all` | 防火墙初始模式，统一管控 PortFWD / TUN 入向 / OnStream 三类暴露面 |
| `FirewallRules` | []FirewallRule | `[]` | 防火墙初始放行规则（`{Source, Proto, Port}`），allow-list 模式生效 |
| `IdentityFile` | string | `""` | 节点身份密钥文件；**建议配置**，否则每次启动 PeerID/虚拟 IP 都变 |

## API 一览

| 方法 | 说明 |
|---|---|
| `New(ctx, Config)` | 创建节点并入网（创建或加入群组）；失败即返回错误 |
| `Info()` | 节点身份：PeerID / GroupID / Group / VirtualIP / InviteCode / Created |
| `OnStream(Handler)` | 注册入向流回调（协议 `/pvn/tunnel/1.0.0`，可注册多个，按序调用） |
| `Dial(ctx, virtualIP)` | 按虚拟 IP 开流，返回 `(Stream, viaRelay, error)` |
| `DialProtocol(ctx, virtualIP, protoID)` | 指定协议 ID 开流（自定义应用协议） |
| `DialPortFWD(ctx, PortFWDTarget)` | 桥接对端节点侧 TCP 服务，返回标准 `net.Conn` |
| `LastPathUsed(peerID)` | 到对端最近一次链路：`direct` / `relay` / `unknown` |
| `NetMap()` | 当前群组成员目录快照 |
| `Host()` | 底层 libp2p Host（进阶：注册自定义协议处理器） |
| `Run(ctx)` | 阻塞运行周期维护任务，直到 ctx 取消 |
| `Close()` | 关闭节点与底层连接 |

### Stream 接口

```go
type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error  // 半关闭写端：发送完毕必须调用，对端才能判 EOF
	Reset() error       // 强制中止流（异常场景，不等对端）
	ViaRelay() bool     // 本流是否经中继转发（false 即 P2P 直连）
	Protocol() string   // 流上的协议 ID
	RemotePeer() string // 对端 PeerID 文本
}
```

## 进阶用法

### 无服务器模式（Standalone）：不部署 ctl/relay 也能组网

`Standalone: true` 开启后**不需要部署任何自己的服务器**：
节点同时运行 DHT server 与 relay service（客户端即服务端），
通过 **mDNS（局域网自动发现）+ 双 DHT（私有优先 + 公共兜底）** 找到同网络成员并自动建连。
`Dial / OnStream / DialPortFWD` 等 API 与常规模式完全一致：

```go
client, err := lanet.New(ctx, lanet.Config{
	Name:       "my-node",
	Standalone: true,
	NetworkKey: "our-secret-net", // 网络密钥：相同密钥的节点组成同一张私有网络
	// 可选：私有 DHT 种子（任意已在网成员地址）；不填则靠 mDNS + 公共 DHT 兜底入网
	Bootstrap: []string{"192.168.1.10:4001/p2p/12D3KooW..."},
})
info := client.Info()
log.Printf("虚拟 IP=%s（自动派生）", info.VirtualIP)
```

**网络密钥（NetworkKey）规则**：

| NetworkKey | 网络归属 | 谁能互连 |
|---|---|---|
| 留空 | **公共网络** | 所有未设置密钥的节点都在同一张大网内，互相可见可连（开放网络，勿传敏感数据） |
| 任意非空字符串 | **私有网络** | 只有持**相同密钥**的节点能互相发现与连接 |

密钥经 `SHA256` 派生网络标识，不可反推明文；两套密钥之间完全隔离
（DHT rendezvous、mDNS tag、建连后群指纹校验三层都是独立的）。

**渠道隔离**：网络身份实际由（分发渠道, NetworkKey）共同派生。SDK 构建
的程序默认渠道为 `lanet.ChannelSDK`，与官方发行版程序（官方渠道）即使
使用完全相同的 NetworkKey 也互不相通——DHT 发现、mDNS、虚拟 IP 派生、
info 协议同群校验四层全部隔离。这是软边界：显式设置 `Config.Channel`
为相同值（如 `"official"`）即可与对应渠道互通。

工作方式：

- **网络即密钥**：成员发现记录的键由网络密钥派生（`SHA256`），不知道密钥
  就无法在 DHT 上定位该网络；建连后还会经 `/lanet/info/1.0.0` 校验双方网络指纹，
  异网络节点自动忽略；
- **虚拟 IP 自动派生**：无控制面分配，按 `SHA256(网络密钥, PeerID)` 确定性映射到
  `10.7.x.x`，同网络成员各自本地计算即可得到一致结果（`NetMap()` 可列出成员表）；
- **节点即服务端**：每个节点无条件运行 Circuit Relay v2 hop 中继（默认资源配额）
  与 kad-dht server 模式；公网可达的成员自然成为网络内的引导与中继节点，
  NAT 后成员经 DCUtR 打洞直连，打洞失败经可达成员中继兜底；
- **双 DHT（私有优先 + 公共兜底）**：私有网络在每台节点上同时运行两张
  完全隔离的 DHT——私有 DHT（`/lanet/kad/1.0.0` 协议前缀）只有本网络节点参与，
  路由表小、发现快；公共 DHT（`/ipfs/kad/1.0.0`）仅负责跨网冷启动时找到
  第一个「自己人」（鸡生蛋的钥匙）与兜底。同群成员一经确认即注入私有 DHT
  路由表（互为种子），之后每轮发现全部走私有快路径。
  纯隐私场景可设 `DisablePublicDHT: true` 关闭公共兜底（需保证首次经种子或局域网入网）；
- **引导（Bootstrap）**：填任意已在网成员的 multiaddr（`<addr>/p2p/<peerID>`，
  每台节点都是私有种子）可加速入网；不填则跨网场景依赖公共 DHT 兜底
  （国内可达性已实测通过，2026-09-05，武汉电信/联通出口均能连上
  4 个官方引导节点）；纯局域网靠 mDNS 自动发现。

**跨网络实测结论（2026-09-05，三节点 Docker 部署）**：两个处于不同物理局域网、
公网入向均不可达的节点（A：192.168.50.x 家宽 + 运营商 NAT；B：另一局域网），
凭相同 NetworkKey + 公共 DHT 引导：

- 数十秒内经 DHT 互相发现并拿到对方 NAT 映射公网地址；
- **DCUtR/QUIC 打洞成功，双向 `via=direct`，rtt 7-13ms 持续稳定**——
  全程无自有服务器；
- 第三个双宿节点（同属两个局域网）作为成员/中继加入后双向 direct 可达，
  印证「可达成员自动成为网络内服务端」的设计。

已验证（2026-09-05，本机 e2e `pvn-serverless-check` + `go test ./pkg/serverless/`，
libp2p v0.49 + kad-dht v0.42）：相同 NetworkKey 双节点无 ctl/relay，
双 DHT 模式互发现（私有来源标记 `dht-private`）+ info 交换，
按派生虚拟 IP 双向 echo 直连往返 PASS；单测覆盖 info 往返、hop 中继预约、
网络密钥隔离语义与「关闭公共兜底的纯私有 DHT 发现」。

实测注意事项：

- **容器部署必须持久化身份**：挂载 `/data` 卷并保持 `LANET_IDENTITY=/data/node.key`，
  否则每次重启 PeerID/虚拟 IP 都变，旧记录还会在 DHT 里残留一段时间
  （表现为成员表短暂出现「幽灵成员」，拨号失败，记录过期后自动消失）；
- 虚拟 IP 由 `SHA256(网络密钥, PeerID)` 派生，身份持久化后即恒定；
- 全员 NAT 且打洞失败时需要至少一个可达成员（公网 IP / 双宿 / 端口转发）充当
  中继与引导，纯 NAT 双端直连互相发现也仍能靠打洞碰运气（本次即成功），
  但不建议依赖。

当前限制：NAT 后节点在打洞成功前无法被直连（成员表中的中继候选打洞后可用）；
公共 DHT 兜底仅在私有网络首次牵线时使用，若关闭兜底且无种子则跨网无法冷启动；
虚拟 IP 冲突未仲裁（群规模大时注意）。

### PortFWD：访问对端节点的 TCP 服务

`DialPortFWD` 把「对端节点虚拟 IP + 端口」桥接为标准 `net.Conn`，
可用于远程桌面、数据库、任意 TCP 协议：

```go
conn, err := client.DialPortFWD(ctx, lanet.PortFWDTarget{
	VirtualIP: "10.7.0.2",
	Port:      3389, // 对端节点本机的远程桌面端口
})
if err != nil {
	log.Fatal(err)
}
defer conn.Close()
// conn 实现了标准 net.Conn，可直接交给 rdp 客户端 / net.Copy / ssh -L 等。
```

工作方式：本节点与目标节点建立 `/pvn/portfwd/1.0.0` 协议流，
目标节点收到后向 `ip:port` 发起 TCP 连接并双向搬运。
**目标为本节点虚拟 IP 时自动映射到 127.0.0.1**（SDK 节点无 TUN，
虚拟 IP 在本机 OS 上不可路由）；目标为其他地址时由目标节点代为拨出（可触达其内网）。

> PortFWD 入向处理器默认开启（`enablePortFWD`）。若要对外提供转发能力，
> 直接用 Go SDK 即可；完整 TUN 组网（ping 虚拟 IP、子网路由）请用内置 agent。

### 自定义协议

两端约定一个协议 ID，服务端用 `Host()` 注册处理器，客户端用 `DialProtocol` 开流：

```go
// 服务端
client.Host().SetStreamHandler("/myapp/1.0.0", func(s network.Stream) {
	defer s.Close()
	// ...
})

// 客户端
stream, viaRelay, err := client.DialProtocol(ctx, "10.7.0.2", "/myapp/1.0.0")
```

### 入向防火墙：统一管控三类暴露面

节点有三类「入向暴露面」，全部受**同一个防火墙**管控（默认全拒绝）：

| 暴露面 | 含义 | 判定维度 |
|---|---|---|
| 端口转发（PortFWD） | 群内成员访问本机/局域网 TCP 服务 | 来源 + TCP + 端口 |
| TUN 虚拟网卡（IP 层） | 群内成员经虚拟网访问本机/局域网任意端口 | 来源 + TCP/UDP + 端口（包内解析） |
| 应用流（OnStream） | 群内成员打开 `/pvn/tunnel/1.0.0` 等应用流 | 来源 + 协议 ID |

三种模式：

| 模式 | 行为 |
|---|---|
| `deny-all`（默认） | 拒绝一切入向（三类暴露面都拒） |
| `allow-list` | 按规则放行（见下方规则字段） |
| `allow-all` | 全开 |

规则字段（`FirewallRule`）：

| 字段 | 取值 | 说明 |
|---|---|---|
| `Source` | 虚拟 IP / CIDR / `*` | 来源成员 |
| `Proto` | `"tcp"`（默认）/ `"udp"` / `"/"` 开头的协议 ID / `"*"` | 协议维度 |
| `Port` | 单值 / 范围 / `*` | 端口维度（协议 ID 规则须留空或 `*`） |

```go
// 编程接口（也可在 Web 控制台操作，两者热更新等价）
client.SetFirewall(lanet.FirewallModeAllowList, []lanet.FirewallRule{
	{Source: "10.7.0.5", Port: "3389"},                  // TCP：指定成员访问指定端口
	{Source: "10.7.1.0/24", Proto: lanet.FirewallProtoUDP, Port: "53"}, // UDP：网段内可用 DNS
	{Source: "*", Proto: "/pvn/tunnel/1.0.0"},             // 应用流：放行 Tunnel 协议
	{Source: "*", Proto: lanet.FirewallProtoAny, Port: "*"}, // 全部协议与端口
})
mode, rules := client.Firewall() // 读取当前快照
```

初始配置也可直接写进 `Config`（控制台 StateFile 存在时会被覆盖）：

```go
lanet.Config{
	FirewallMode:  lanet.FirewallModeAllowList,
	FirewallRules: []lanet.FirewallRule{{Source: "*", Proto: lanet.FirewallProtoAny, Port: "*"}},
}
```

判定发生在「向内网目标发起连接 / 把包写进本机协议栈 / 把流交给应用处理器」**之前**；
来源未知（不在成员表 / 包内源地址非法）的请求除 `allow-all` 外一律拒绝。
TUN 入向的拒绝以丢包实现，并有累计计数日志；注意 **ICMP 等非 TCP/UDP 包**
无端口可匹配，`deny-all` / `allow-list` 下会被丢弃（ping 不通是正常现象）。

### 局域网端口转发：把内网其他设备暴露给群内

映射表模式：入向请求端口命中 `Listen` 时转发到 `Target`（本机所在
真实局域网内的设备）：

```go
client, _ := lanet.New(ctx, lanet.Config{
	Name: "home-node",
	Standalone: true,
	NetworkKey: "our-secret-net",
	LANForwards: []lanet.LANForward{
		{Listen: 5000, Target: "192.168.1.100:5000"}, // 群内访问 <本节点IP>:5000 → NAS
		{Listen: 3389, Target: "192.168.1.101:3389"}, // → 内网远程桌面
	},
	StateFile: "lanet-state.json", // 控制台状态持久化（可选）
})
// 运行中热更新：
client.SetLANForwards([]lanet.LANForward{{Listen: 8080, Target: "192.168.1.102:80"}})
```

未命中映射表的放行端口回退到本机 `127.0.0.1`（原有行为）。
入向转发统一受防火墙约束——**先过防火墙，再查映射表**。

### Web 控制台

每个节点默认自带控制台：`http://127.0.0.1:8900`（端口占用自动向后尝试至 8910）。
`Config.ConsoleAddr` 可改地址，设为 `"-"` 关闭。功能：

- **成员与链路**：实时成员表 + 每个成员最近链路（direct / relay）；
- **入向防火墙**：模式切换 + 规则增删，保存即时生效；
- **局域网转发**：映射表增删，保存即时生效；
- 规则变更自动持久化到 `Config.StateFile`（未配置则仅内存）。

编程接口与控制台等价：`Firewall/SetFirewall/LANForwards/SetLANForwards`。

### 节点身份与虚拟域名：IP 变了也能连

**节点身份持久化**——不配置 `IdentityFile` 时，每次启动随机生成身份，
PeerID 与派生虚拟 IP 都会变化。配置后身份跨重启稳定，虚拟 IP 恒定：

```go
client, _ := lanet.New(ctx, lanet.Config{
	Name:         "home-node",
	Standalone:   true,
	NetworkKey:   "our-secret-net",
	IdentityFile: "node.key", // 首次自动生成，之后复用（权限 0600）
})
```

**虚拟域名**——`Dial / DialProtocol / DialPortFWD` 的目标参数除了虚拟 IP，
还支持**成员名称**：成员重启后 IP 变化，其他成员仍按名字连入，无需关心 IP：

```go
stream, _, err := client.Dial(ctx, "home-node")   // 按名称，等价于拨其当前虚拟 IP
conn, err := client.DialPortFWD(ctx, lanet.PortFWDTarget{
	VirtualIP: "home-node", // 虚拟域名同样生效
	Port:      5000,
})
```

解析规则：目标是合法 IP 时按 IP 处理；否则在当前成员表按 Name 精确匹配
（匹配到多个会报错并提示改用 IP；匹配不到报错提示未发现）。名称即节点
启动时的 `Config.Name`，在控制台成员表中可见。

### Run / Close 生命周期

- `New` 返回即已入网（群组创建/加入完成），可立即 `Dial`；
- `Run(ctx)` 负责周期通告地址、刷新 NetMap、续期中继预约——**必须有人调用**，
  否则中继兜底链路会失效（直连不受影响）。典型写法：主 goroutine 里 `client.Run(ctx)`，
  业务逻辑在其他 goroutine；或 `go client.Run(ctx)` 后主逻辑自己阻塞；
- `Close()` 释放 libp2p Host 并停止控制台；`ctx` 取消时 `Run` 返回，随后 `defer client.Close()`。

## 常见问题

**Q：`Dial` 返回 `viaRelay=true` 是坏事吗？**
不是。说明直连打洞失败，流量经 relay 转发，功能等价、延迟略高。
可用 `LastPathUsed(peerID)` 观测链路类型。

**Q：为什么写完数据对端读不到 EOF？**
必须调用 `stream.CloseWrite()`。libp2p 流没有「发送完毕」的隐式信号，
半关闭是唯一的 EOF 语义（Windows 对端尤其依赖此判断）。

**Q：无效邀请码 / ctl 不可达会怎样？**
`New` 直接返回错误，节点不会进入半入网状态。

**Q：portfwd 转发后回程数据被掐断？**
已在 SDK 内处理（双向均结束才关闭连接）。请确保使用当前版本。

## 实测记录

- 2026-09-04：三机真机联测（2×Linux + Windows），6 方向直连 + relay 兜底 + 边界场景 + 重启恢复全过；
- `go test ./...` + `go run ./app/agent/cmd/pvn-e2e-check`（端到端本地验证）持续绿。

## 能力边界

- SDK 节点无 TUN：不能 ping 虚拟 IP、不能当子网路由器（这些用内置 agent）；
- 流是消息流不是字节流：libp2p 不保证消息边界，跨流传协议请自行分帧
  （长度前缀 / 分隔符）；PortFWD 桥接的 TCP 连接无此限制；
- 浏览器接入用 [Web SDK](../web/README.md)，小程序/Unity 用网关 SDK（见 [总览](../README.md)）。
