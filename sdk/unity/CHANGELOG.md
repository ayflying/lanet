# Changelog

本包遵循 [语义化版本](https://semver.org/lang/zh-CN/)，版本号与仓库根 `VERSION` 独立
（包版本只描述 Unity 侧接口）。

## [0.1.0] - 2026-09-16

首个版本。

### 新增

- **链路 1：ws-gateway 网关客户端** —— 与 `sdk/csharp` 逐字同源的
  `LanetGatewayClient` / `GatewayStream` / `Frames`，跨平台访问网格内任意节点的
  TCP 服务（Windows / macOS / Linux / Android / iOS）。
- **链路 2：Android 本地节点** —— `LanetAndroidNode` 静态门面复用
  `lanet-plugin.aar`，让 App 自己成为网络成员（可 ping 虚拟 IP、被 mDNS 发现）。
  非 Android 或 AAR 缺失时全部调用安全降级，不抛异常。
- **`LanetManager` 组件** —— 一个 Inspector 友好的入口同时管住两条链路，
  自动轮询节点状态；所有事件在主线程触发。
- **`LanetDispatcher`** —— 主线程泵，把网关后台线程的事件搬回 Unity 主线程
  （首次访问自动创建隐藏 `DontDestroyOnLoad` 对象，不用手动摆场景）。
- **`LanetStreamExtensions`** —— `RequestStringAsync` 一问一答、`ReadLinesAsync`
  行循环、`ContinueOnMainThread` 等流操作糖。
- **Editor 工具** —— 菜单 `Lanet → 检查依赖与配置`：核对构建平台、AAR 是否就位、
  AAR 内含架构、`minSdk ≥ 21`、目标架构是否含 ARM64；另有 `创建 LanetManager 到场景`。
- **示例** —— `Samples~/LanetDemo`：运行时可操作面板（请求 / 启停节点 / 复制连接码 /
  成员与待审批列表）。

### 配套（Android 侧）

- 抽出**宿主无关门面** `com.lanet.plugin.LanetNode`（`:android-plugin` 模块内），
  与 uni-app 的 `LanetVpnModule` 共用同一份 `lanet-plugin.aar`；
  `LanetVpnModule` 退化为纯 DCloud 薄壳。
- 新增 `LanetInitProvider`：进程启动时把 Application Context 注入门面，使 Unity 侧
  只需调 `startAuto` / `stopAuto` / `isAuthorizedAuto` 这类零参数方法。
  这样绕开 `AndroidJavaClass.CallStatic` 按传入对象实际类推导签名的陷阱
  （把 `UnityPlayerActivity` 当 `Context` 传会推导出 `UnityPlayerActivity` 从而找不到方法）。
- `build.py` 新增第 ⑥ 步：同步两个 AAR 到 `Runtime/Plugins/Android/`，并用 `javap`
  校验门面的 16 个静态入口齐全（漏 `@JvmStatic` 或被 R8 裁掉会当场报错）。

### 已知限制

- WebGL 不支持（`System.Net.WebSockets` 不可用，也没有原生节点）。
- AAR 只编了 `armeabi-v7a` / `arm64-v8a` / `x86`，**没有 x86_64**：
  64 位 x86 模拟器跑不了链路 2。
- Android targetSdk 28+ 连 `ws://` 明文需自行声明 `usesCleartextTraffic`。
- 包内 `.meta` 不入库，由 Unity 首次导入时生成。
