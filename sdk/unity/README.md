# Lanet for Unity

Lanet 群组制 P2P 虚拟局域网的 Unity 接入包（UPM，`com.lanet.unity`）。

它提供**两条互补的链路**——按你的场景挑一条或两条都用：

| 链路 | 平台 | 能力 | 代价 |
| --- | --- | --- | --- |
| **1. ws-gateway 网关** | 全部（Windows / macOS / Linux / Android / iOS） | 经网关访问网格内任意节点的 **TCP** 服务 | 网关中转，**不是端到端**；需要一台跑着 lanet 的网关 |
| **2. Android 本地节点** | 仅 Android | App 自己成为网络成员：可 `ping` 虚拟 IP、被 mDNS 发现、任意 TCP/UDP 直连 | 需要系统 VPN 授权；依赖 `lanet-plugin.aar` |

链路 1 是「远程调用工具」，链路 2 是「真入网」。想做局域网联机游戏/文件传输，用链路 2。

## 一、安装

### 1. 引入包

三种方式任选（**推荐第一种**，方便随后续更新重建）：

```
① Unity → Window → Package Manager → +（Add package from disk…）→ 选 sdk/unity/package.json
② 把整个 sdk/unity 目录拷进项目的 Packages/ 下
③ 把整个 sdk/unity 目录拷进项目的 Assets/ 下（此时 Editor 菜单仍可用，但不会被 UPM 管理）
```

### 2. 放入原生库（只有用链路 2 才需要）

`Runtime/Plugins/Android/` 下的两个 `.aar` **不入库**（`lanet.aar` 有 38MB），
clone 后跑一次构建脚本自动填充：

```bash
python sdk/android-plugin/build.py            # 全量：编 Go 核心 + 编插件 + 同步 Unity
python sdk/android-plugin/build.py --skip-aar # 只改了 Kotlin/清单时，省两分钟
```

脚本的第 ⑥ 步会把 `lanet.aar` 与 `lanet-plugin.aar` 拷到 `Runtime/Plugins/Android/`，
并用 `javap` 校验宿主无关门面 `com.lanet.plugin.LanetNode` 的静态入口齐全
（Unity 只能调真·静态方法，漏一个 `@JvmStatic` 就会在真机上「no such method」）。

> 若只手工拷贝 AAR，记得两个都要：`lanet.aar`（Go 核心）+ `lanet-plugin.aar`（VpnService 与门面）。

### 3. 自检

菜单 **Lanet → 检查依赖与配置** 会一次性核对：构建平台、AAR 是否就位、AAR 内含架构、
`minSdk ≥ 21`、目标架构是否含 ARM64。这三项在编辑器里都不报错，只在真机启动时
以「找不到类」的形式暴露，所以建议每次换环境先点一次。

## 二、快速开始

**Lanet → 创建 LanetManager 到场景**，然后在 Inspector 上填：

链路 1（默认开）：

| 字段 | 说明 |
| --- | --- |
| `gatewayUrl` | `ws://host:8700/gateway`。**真机上不能再用 127.0.0.1**——那是手机自己 |
| `inviteCode` | 群组邀请码，网关启动日志里能查到 |
| `gatewayMode` | `client`（主动开流）/ `service`（接收入向流；网关同一时刻只允许一个 service） |

链路 2（勾 `useAndroidNode`）：

| 字段 | 说明 |
| --- | --- |
| `networkKey` | 网络密钥，与目标网络一致才能互相发现。**必填**：留空会被原生层拒绝（留空只会派生「本机专属网」，与谁都不互通） |
| `bootstrap` | 引导种子 multiaddr，填任意已在网成员的地址即可入网 |
| `autoAccept` | 自动同意陌生节点申请（联调方便，生产建议关掉走人工审批） |
| `wantTun` | 是否建虚拟网卡（关掉退化为纯应用层，不能 ping 虚拟 IP） |

```csharp
using Lanet.Sdk;
using UnityEngine;

public class Net : MonoBehaviour
{
    [SerializeField] LanetManager mgr;

    async void Start()
    {
        // 链路 1：一问一答（HTTP 头打任意 HTTP 服务，验证连通最快）
        await mgr.ConnectGatewayAsync();
        string reply = await mgr.RequestAsync("10.7.207.102", 4001, "GET / HTTP/1.0\r\nHost: lanet\r\n\r\n");
        Debug.Log(reply);

        // 链路 2：本机入网（仅 Android 真机；首次会弹系统授权框）
        mgr.StartNode();
        mgr.OnNodeStateChanged += st => Debug.Log($"running={st.running} ip={st.virtual_ip} 成员={st.member_count}");
    }
}
```

把示例导入工程：**Package Manager → 选中 Lanet → Samples → Import**，得到
`LanetDemo.cs`（挂上就有操作面板，含连接码复制、成员列表、待审批）。

拿到虚拟 IP 后，同网络的其他成员就能 `ping 10.7.x.x`、连你 App 监听的端口。
反过来 `LanetAndroidNode.InviteCode()` 给出本机连接码，发给别人即可让别人连进来。

## 三、API

### `LanetManager`（MonoBehaviour，推荐入口）

| 成员 | 说明 |
| --- | --- |
| `ConnectGatewayAsync()` / `DisconnectGateway()` | 链路 1 连接/断开，返回 `Task<bool>` |
| `DialAsync(virtualIp, port)` | 开一条流（`using` 包住，`Dispose` 即中止） |
| `RequestAsync(virtualIp, port, payload)` | 一问一答，返回 UTF-8 文本 |
| `StartNode(options = null)` / `StopNode()` | 链路 2 启动/停止（参数为 null 时用 Inspector 取值） |
| `ConnectPeer(address)` | 主动连接：裸 PeerID / 连接码 `lanet://…` / multiaddr |
| `ApprovePeer(peerId, approve)` / `RemovePeer(peerId)` | 审批 / 移除成员 |
| `Members()` / `Pending()` / `Nearby()` | 拉成员表 / 待审批 / 附近节点 |
| `IsGatewayConnected` / `IsNodeRunning` / `NodeStatus` / `InviteCode` | 状态读取 |
| 事件 `OnGatewayConnected` / `OnGatewayClosed` / `OnGatewayError` / `OnInboundStream` / `OnNodeStateChanged` / `OnNodeMessage` | **全部在主线程触发**，可直接碰 Unity API |

`StartNode` 返回时节点通常还没起来：若返回 `stage=awaiting_permission` 表示系统授权框
已弹出，用户同意后服务自动启动。`LanetManager` 会自动轮询（`nodeStateInterval`，默认 2s）
并通过 `OnNodeStateChanged` 通知，不用自己写等待。

### `LanetAndroidNode`（静态门面，Android 专用）

`Start(opt)` / `Stop()` / `IsRunning()` / `Status()` / `Members()` / `Pending()` /
`Peers()` / `Nearby()` / `SeedAddrs()` / `InviteCode()` / `LastError()` / `Version()` /
`Connect(addr)` / `Approve(id, bool)` / `Remove(id)`。

非 Android 平台或 AAR 缺失时：`IsSupported` / `IsAvailable` 为 false，所有调用返回
安全默认值并把原因写进 `LastError()`，**不会抛异常**——所以可以放心在跨平台代码里调用。

另有 `StartAsync` / `StopAsync` 自动切主线程（启动要拉起授权 Activity，必须在主线程调）。

### `LanetGatewayClient` / `GatewayStream` / 扩展方法

与 [sdk/csharp](../csharp/README.md) **逐字同源**（`Frames.cs`、`GatewayStream.cs`、
`LanetGatewayClient.cs` 是同一份文件）。要手写底层协议或看帧格式，读那份文档。

`LanetStreamExtensions` 提供 `RequestStringAsync` / `RequestAsync` / `SendStringAsync` /
`ReadLoopAsync` / `ReadLinesAsync` / `ContinueOnMainThread`。

> **不要同时导入 `sdk/csharp` 的 `Lanet.Sdk.csproj` 与本包** —— 命名空间相同会撞类型。
> 只导 C# 库不要 Unity 能力时用 `sdk/csharp`；在 Unity 里就用本包。

## 四、已知限制与坑

1. **WebGL 用不了**：`System.Net.WebSockets` 在 WebGL 上不可用，也不可能有原生节点。
2. **Android 上连 `ws://` 明文**：targetSdk 28+ 默认禁明文。要么用 `wss://`，要么在
   `Assets/Plugins/Android/AndroidManifest.xml` 的 `<application>` 上加
   `android:usesCleartextTraffic="true"`。
3. **后台线程事件**：直接用 `LanetGatewayClient` 时 `OnStream` / `Closed` / `OnError`
   跑在线程池线程，碰 Unity API 会抛 `can only be called from the main thread`。
   用 `LanetManager`（已过调度器）或自己套 `ContinueOnMainThread`。
4. **AAR 里没有 x86_64**：Go 核心只编了 `armeabi-v7a / arm64-v8a / x86`。64 位 x86
   模拟器跑不了链路 2；真机与 arm 模拟器正常。
5. **授权必须由 Activity 发起**：`VpnService.prepare()` 要一个 Activity 上下文，
   所以 `startAuto` 内部会拉起透明的 `VpnAuthProxyActivity`。用 `Application` 上下文
   会静默失败——这正是 `LanetInitProvider` 注入 Context、C# 只在主线程调 `startAuto` 的原因。
6. **VPN 只接管 10.7.0.0/16**：lanet 的虚拟网卡掩码是 /16，你的普通上网流量仍走物理网卡，
   不会被劫持。
7. **Android 13+ 通知权限**：VpnService 是前台服务，需要 `POST_NOTIFICATIONS`
   （AAR 清单已声明，运行时仍可能被用户拒绝——拒绝不影响入网，只是看不到常驻通知）。
8. **包内的 `.meta` 不入库**：Unity 首次导入时自行生成，避免与你的工程 GUID 冲突。

## 五、排障

| 现象 | 原因与处理 |
| --- | --- |
| `未找到原生类 com.lanet.plugin.LanetNode` | AAR 没进 `Runtime/Plugins/Android/`，或构建平台不是 Android。跑 `build.py` 后点 **Lanet → 检查依赖与配置** |
| `原生层未初始化：LanetInitProvider 未生效` | AAR 的 manifest 没被合并（多半是 AAR 放错位置，或用了 `.jar` 而不是 `.aar`） |
| 启动返回 `awaiting_permission` 但一直不 running | 用户在系统弹窗点了拒绝；或应用没有可用的前台服务权限。查 `LastError()` |
| 网关连上但 `RequestAsync` 超时 | 目标端口在对方节点没监听（链路是通的，服务不在）。先用 HTTP 头打一个已知服务验证 |
| 成员列表为空但能看到「附近节点」 | 正常：附近是单方发现，成为**成员**需要双方互相审批。用 `ConnectPeer` 主动连一次触发审批 |
| 编辑器里一切正常、真机崩在 `CallStatic` | 门面方法签名对不上。跑 `build.py` 看 javap 校验是否报缺方法（漏 `@JvmStatic` 或被 R8 裁掉） |
| `UnityException: … can only be called from the main thread` | 见「已知限制 3」 |

## 六、相关文档

- [`sdk/README.md`](../README.md) —— 全平台 SDK 总览
- [`sdk/csharp/README.md`](../csharp/README.md) —— 网关帧协议与 C# 库细节
- [`sdk/android-plugin/README.md`](../android-plugin/README.md) —— AAR 怎么编、uni-app 插件怎么用
- [`sdk/android/README.md`](../android/README.md) —— gomobile 绑定层
