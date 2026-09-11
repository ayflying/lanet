# Lanet

**Lanet** = **Lan** + **net**：把不同网络中的设备接入同一张加密的 P2P 虚拟局域网。

官方发行版是一个无需中心服务器的单程序节点。设备之间采用**按节点 ID 互连 +
连接审批**的模型：把对方的节点 ID 填进控制台并点连接，对方同意一次即永久信任，
之后自动重连。审批通过后节点会在 `lanet.db` 本地地址簿里长期记住对方，重复
连接不再需要任何网络查询。

网络密钥决定「谁可以被找到」：只有持有相同网络密钥的节点才能互相查找到地址，
密钥不同的节点即使拿到完整节点 ID 也无法定位对方。审批通过后优先打洞直连；
直连失败时，可经网络内可达成员的 Circuit Relay v2 中继。Windows 和 Linux
节点默认启用 TUN，可直接使用 `10.7.x.x` 虚拟 IP 进行 `ping` 及 TCP/UDP 通信。

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

- **按节点 ID 互连**：填写对方节点 ID 即可连接，地址由本地地址簿或私有 DHT
  自动补齐，不需要手工交换 IP 与端口；一条 ID 就够。
- **连接审批（类似加好友）**：默认开启。陌生节点的连接申请进入控制台
  「待审批」列表，同意一次即永久信任；未审批节点完全隔离——拿不到成员表、
  虚拟 IP 与任何设备信息。无人值守的中央服务器/种子节点可开启
  `auto_accept` 自动同意绕过人工审批。
- **本地地址簿**：`lanet.db`（SQLite）保存已知节点、可用地址与信任关系，
  重连优先走本地记录，零 DHT 查询流量；重启后依然记得。
- **流量友好**：地址簿为空（首次启动）时只广播自身、不发起任何主动查找，
  不为「可能有谁在线」付查询流量。
- **零中心组网**：相同网络密钥才能互相查找到地址；局域网走 mDNS，
  跨网络走双 DHT。
- **直连优先**：TCP、QUIC、WebSocket、webrtc-direct、DCUtR 打洞与
  Circuit Relay v2 组合使用，链路自动降级。
- **真实虚拟网卡**：Windows 使用 Wintun，Linux/macOS 使用系统 TUN；
  `10.7.0.0/16` 全网段均路由到虚拟网卡。
- **稳定身份与地址**：`node.key` 固定 PeerID；虚拟 IP 随身份稳定，成员还可用
  `<节点名>.lanet`、短名或原始成员名连接。
- **统一入向防火墙**：同一套规则覆盖 TUN TCP/UDP、PortFWD 与应用协议流。
- **局域网端口转发**：把节点所在真实局域网中的 NAS、数据库、远程桌面等服务
  暴露给同网络成员。
- **内置 Web 控制台**：成员与链路、连接审批、节点配置、防火墙、转发映射和
  更新操作集中管理。
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
4. 与对方**互发节点 ID**：控制台「成员与链路」页顶部可直接复制本机的完整节点
   ID（`12D3Koo…`）。把对方的节点 ID 粘贴到「添加节点」输入框点连接即可；
   对方会在「待审批的连接申请」里看到你，点“同意”后立即连通，之后自动重连。
5. 连通后即可互相 `ping 10.7.x.x`、`ping <节点名>.lanet` 或访问对方暴露的服务。

> 连接是双向的：**双方都要添加对方并同意**。这是默认的安全边界——陌生节点
> 无法探测你的成员列表、设备名或虚拟 IP。无人值守的中央服务器/种子节点可在
> 「节点配置」勾选“自动同意所有连接申请”（或设 `LANET_AUTO_ACCEPT=true`）
> 免除人工审批。

只有首次生成配置时会自动打开浏览器。后续可从托盘菜单打开控制台或退出节点。

**虚拟域名解析（.lanet DNS）**：节点内置 DNS 应答器（127.0.0.1:53），把
`<成员名>.lanet` 的查询按成员表实时应答成虚拟 IP（TTL=0 不缓存，成员换 IP 自动跟随）。
Windows 上启动时自动注册 NRPT 规则（`*.lanet → 127.0.0.1`），退出时移除，因此
`ping xa.lanet`、`ping yunloli.lanet` 直接可用；非 Windows 平台仅启动 DNS 服务，
需手动把 `.lanet` 后缀指向本机（如 /etc/resolv.conf 或 /etc/resolver/lanet）。
DNS 与 NRPT 规则强制开启、无需任何配置（0.5.11 起，控制台不再提供开关）。

**开机自启（Windows）**：在 Web 控制台“节点配置”勾选“Windows 服务自启”即可。
程序会注册名为 `Lanet` 的 LocalSystem 自动启动服务，机器开机后即运行，**无需用户登录**。
服务会沿用当前 exe 与 `lanet.json` 的绝对路径，后台运行时不启动托盘、不打开浏览器；
仍可从任意浏览器访问配置的控制台地址。取消勾选即卸载服务。升级自 v0.5.23 及更早版本时，
启用服务会自动清理旧的 `HKCU\...\Run` 登录自启项，避免重复启动。

**成员详情（识别设备）**：成员列表每行最右侧的“详情”按钮弹出**该成员**设备的
基础信息——操作系统主机名、节点名/虚拟 IP/虚拟地址/节点 ID、系统与平台、程序版本，
以及对方本机所有网卡的 IP 地址（排除回环与链路本地地址），点击 IP 可复制，支持
一键复制全部信息，方便确认“这是哪台机器”。信息经 info 协议从对端实时交换；
对端为 0.5.15 及更早版本时不显示主机名与本机 IP（提示升级）。0.5.16 起；
「成员与链路」页标题栏的“本机信息”按钮可查看控制台所在设备自身（0.5.12 起功能
保留，0.5.16 起从成员行按钮改为独立入口）。

**端口转发本地监听（0.5.13 起）**：「端口转发」映射（listen → target）里的每个
端口会在本节点上真实监听并代理到目标。此前容器节点（如 217 的 lanet-node）只有
TUN 虚拟 IP 可达，虚拟 IP 上的端口没有进程监听，群内成员访问会被内核直接拒绝
（ping 通但 TCP 连不上）；现在控制台添加转发 `7860 → 192.168.50.217:7860` 即可
让 `http://<虚拟IP>:7860` 直接可用。转发映射热更新，监听随映射增删同步启停。

命令行参数会覆盖 `lanet.json`：

```powershell
./lanet.exe -name pc1 -key "our-network-key"
./lanet.exe -name pc2 -key "our-network-key"

# 跨网冷启动：填成员连接种子（推荐，纯私有零公共流量）
./lanet.exe -name pc3 -key "our-network-key" -bootstrap "<成员连接种子 multiaddr>"

# 或临时开启公共 DHT 引导（最长 10 分钟后自动关闭，连上同群成员立即关闭）
./lanet.exe -name pc4 -key "our-network-key" -public-dht -public-dht-minutes 10

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
| `LANET_AUTO_ACCEPT` | `true` = 自动同意所有连接申请（无人值守引导/中继节点建议开启） |
| `LANET_REQUIRE_APPROVAL` | `false` = 关闭连接审批，同密钥节点可直接互连（旧行为） |
| `LANET_DB` | 地址簿数据库路径，容器内默认 `/data/lanet.db`（随数据卷持久化） |

放行 `4001/tcp`、`4001/udp`；只有确实远程开放控制台时才放行 `8900/tcp`。
Compose 默认使用 `network_mode: host`（容器直接使用宿主机网络栈，打洞成功率
最高、TUN 工作在推荐形态），端口语义即宿主机端口；如需 bridge 模式请按
compose 文件内注释恢复 `ports` 映射。
身份、配置、状态和日志保存在 `lanet-node-data` 命名卷中。

Compose 默认已启用 TUN（`LANET_TUN: "true"`，并授予 `NET_ADMIN` 与
`/dev/net/tun`），容器节点开箱即可响应虚拟 IP（前提：宿主机已加载 tun 模块，
`modprobe tun`）。若只做纯引导/中继用途，可将 `LANET_TUN` 改为 `"false"`
并注释掉权限段，日志更干净、容器权限更小。

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
| `lanet.json` | 名称、网络密钥、引导方式、控制台、监听地址、TUN、连接审批等节点配置 | 保存后重启 |
| `node.key` | Ed25519 节点身份，决定 PeerID 和稳定虚拟 IP | 不应删除或在多个在线节点间共用 |
| `lanet.db` | 本地地址簿：已知节点、可用地址、信任关系与待审批记录（SQLite/WAL） | 即时写入，可安全删除（只失去「记得对方」的能力） |
| `state.json` | 防火墙规则与局域网端口转发映射 | 控制台保存后立即生效 |
| `lanet.log` | 运行日志和链路探测结果 | 实时写入 |
| `manifest.json` | 官方签名更新清单种子 | 随发行包提供 |

Windows 上这些文件位于 `lanet.exe`/配置文件目录；`node.key` 也随整个目录迁移。
控制台 GET 接口不会返回密码明文。

### 网络密钥、连接审批与地址查找

连接一个节点只需要对方的**节点 ID**（控制台页眉可复制）。整个流程分两层：

**第一层 · 网络密钥决定「能否被找到」**

- 留空 = 按本机身份派生的**专属默认网络**：开箱即用，但默认自成一张网，
  不会与其他零配置节点同网（避免超大网络带来的发现流量与成员表膨胀）。
- 填写非空且相同的密钥组成私有网络；密钥经 SHA-256 派生网络标识，不直接广播明文。
- 查找仅限同一密钥内：地址查找走由网络密钥派生的私有 DHT provider 记录，
  密钥不同的节点根本不在同一张表里，**即使拿到完整节点 ID 也无法定位对方**。
- 迁移说明：历史版本留空派生的是固定公共密钥（`lanet/public`）。由旧配置
  升级而来的节点会自动按旧密钥处理，老网络零迁移；新部署使用本机专属默认网络。

**私有协议层（0.5.34 起）——密钥决定「连得上协议」**

历史版本里成员信息交换、删除好友通知、P2P 更新分发、探测回显这些控制面
协议用的是**全局固定协议 ID**（`/lanet/info/1.0.0` 等），私有 DHT 也是全局
固定前缀（`/lanet/kad`）。后果：任何能拨通端口的 libp2p 节点——包括公网
扫描器和完全不相干网络的客户端——都能完成协议协商、把流量打到应用层：
刷待审批/附近列表、消耗探测回显，甚至经由更新协议把整份二进制拉走；
所有 lanet 部署还共用一张全局 DHT 路由网，彼此替陌生人的查询买单。

0.5.34 起这些控制面协议 ID 与私有 DHT 前缀**由群组密钥派生**（形如
`/lanet/<群指纹>/info/1.0.0`）：不知道网络密钥就构造不出正确的协议 ID，
异流量在 multistream 协商阶段即被拒绝——零 handler 触发、零应用层流量。
网络密钥由此成为真正的私有协议准入凭证。数据面（隧道 `/pvn/tunnel`、
端口转发 `/pvn/portfwd`）保持全局 ID 不变，保证同群新老版本随时互通。

- **混版本过渡**：已升级节点出向拨号自动携带「派生 ID + 历史固定 ID」双
  候选，老版本好友仍可正常握手与升级；新节点上的固定 ID 兼容入口带成员
  门与群指纹校验，陌生流量依旧进不来。
- **逃生开关**：`legacy_protocols` / `LANET_LEGACY_PROTOCOLS` /
  `-legacy-protocols`（默认 `false`）。仅当与未升级老对端互通异常时临时
  置 `true` 退回全局固定协议，问题解决后应关闭（重新暴露于跨群噪音）。

**第二层 · 连接审批决定「能否连上」**

- 把对方节点 ID 粘贴到控制台「添加节点」并点连接 → 本机立刻信任对方，
  同时向对方提交申请。
- 对方在「待审批的连接申请」里点**同意** → 双向永久信任，立即连通并自动重连。
- 未审批节点**完全隔离**：拿不到成员表、虚拟 IP、名称、版本、主机名等任何信息，
  也不进入私有 DHT 路由表。
- 开关：`require_approval`（默认 `true`）/ `LANET_REQUIRE_APPROVAL`；
  `auto_accept`（默认 `false`）/ `LANET_AUTO_ACCEPT` 供无人值守节点自动同意。
  控制台「节点配置」可直接切换，重启生效。

**删除好友是双向的（0.5.31 起）**

在控制台成员列表点「删除」，不只是本机移除，对方也会同步把它那边的本机删掉：

- 对方**在线**：本机经 `/lanet/unfriend/1.0.0` 协议即时通知，对方收到后自动
  把本机从它的地址簿与成员表移除并断开连接。
- 对方**离线**：本机记一枚「删除墓碑」；对方下次来握手时收到
  `rejected=unfriended` 标记，据此自动完成同步删除（自愈式，无需双方同时在线）。
- 删除后若对方仍可被网络发现，它会回到双方的「**附近**」列表，可随时重新申请。

**附近节点（0.5.31 起）**

控制台「附近」卡片展示同一网络密钥内可发现、但还不是好友的节点（含被删除过
的好友）。点「申请连接」即向对方发起好友申请（走双向审批）。附近记录只含
公开可发现的节点 ID 与地址，不含任何身份信息；持久化在 `lanet.db` 的
`nearby` 表，上限 200 条（按最近发现时间保留）。

**地址查找顺序（省流量）**

1. **本地地址簿优先**：`lanet.db` 里记过的地址直接用，零网络查询；
2. **私有 DHT 兜底**：地址簿未命中时向私有 DHT 查询对方地址；
3. **地址簿为空则只广播不查找**：首次启动、还没添加过任何节点时，只做自身的
   DHT 广播（让别人能找到我），不发起任何主动查询，避免为「可能有谁在线」
   付查询流量。添加过节点后自动恢复查找。

**连接方式（按推荐度）**

- **① 按节点 ID（推荐）**：控制台「添加节点」粘贴对方节点 ID，点连接。
  地址由本地地址簿或私有 DHT 自动补齐，运行时可调、无需重启。
- **② 连接种子**：把控制台「高级 → 我的连接种子」（完整 multiaddr）发给对方，
  或填到 `bootstrap`（`-bootstrap`）让每次启动自动回拨。适用于双方还不在
  同一张私有 DHT 路由表里、需要首次牵线的场景（纯私有、零公共流量）。
- **③ 公共 DHT 临时引导**：仅用于跨网零配置冷启动，见下。

> 为避免「超大局域网」：默认不把所有节点塞进一张网。要与他人互通必须
> 共享网络密钥，并且双方互相添加节点 ID。

- **公共 DHT 默认关闭（0.5.16 起）**：`enable_public_dht=true` 或 `-public-dht`
  显式开启。关闭时跨网首次牵线需用方式 ②。
- **公共 DHT 临时引导（0.5.18 起）**：即使开启，公共 DHT 也是「限时介绍人」——
  连上第一个同网络成员立即自动退出；到时限（`-public-dht-minutes`，
  配置项 `public_dht_minutes`，默认 **10 分钟**）仍未连上也自动退出。
  控制台「节点配置」里的开关**实时反映运行状态**：自动退出后开关自动变灰，
  重新勾选可即时再开（无需重启）。背景：公共 DHT 是全公网共享的，作为
  server 节点要应答全网随机查询，实测空载上行约 4~5MB/分钟（峰值连接上千
  公网节点），找到自己人后这笔流量完全可以省掉。

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
