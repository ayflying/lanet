# lanet uni-app 原生插件（lanet-vpn）

把 lanet 的 P2P 组网能力封成 **uni-app 原生插件**，让 Android App 直接成为组网成员：
拿到 `10.7.x.x` 虚拟 IP 后，可以在 App 内 ping 通其它成员、直连任意 TCP/UDP 端口，
也可被 mDNS / DHT 发现——**不需要任何服务端**。

底层复用 `sdk/android` 那份 gomobile 绑定库（`com.lanet.mobile.Node`）与已验证过的
`LanetVpnService`（Android VpnService 两阶段建卡），插件层只做「取 Context → 转发调用 →
回 JS 回调」这一件事。

```
sdk/android-plugin/                 插件工程（构建侧）
├─ build.py                         统一构建入口（五步全自动）
├─ plugin/                          Gradle 工程
│  └─ lanet-plugin/                 插件 module（Kotlin，产出 lanet-plugin.aar）
│     ├─ src/main/java/com/lanet/plugin/
│     │  ├─ LanetVpnModule.kt       插件入口（JS 调用的就是这个类）
│     │  ├─ LanetVpnService.kt      VpnService：建立 tun、持有会话
│     │  ├─ VpnAuthProxyActivity.kt 透明代理：拉系统 VPN 授权框
│     │  └─ LanetCore.kt            对 gomobile 库的薄封装
│     ├─ consumer-rules.pro         宿主混淆规则（宿主必须保住插件类）
│     └─ libs/lanet-classes.jar     构建期从 lanet.aar 抽出（compileOnly，不入库）
└─ lanet-vpn/                       交付包（拷进 uni-app 项目即用）
   ├─ package.json                  插件描述（id / class / abi / 权限）
   └─ android/
      ├─ lanet.aar                  gomobile 编译产物（三 ABI，约 38MB）
      └─ lanet-plugin.aar           插件本体（约 32KB）

sdk/uniapp-demo/                    uni-app 示例工程（Vue3）
├─ utils/lanet.js                   插件 JS 封装（Promise 化 + 降级）
└─ pages/index/index.vue            完整 UI：启动/停止/成员/审批/连接
```

## 一、构建插件

```powershell
cd sdk/android-plugin
python build.py                # 全流程：编 AAR + 编插件 + 校验 + 同步示例工程
python build.py --skip-aar     # 跳过 gomobile（沿用已有 lanet.aar），约 15s
python build.py --verify-only  # 只做交付包校验
```

五个步骤：

| 步骤 | 动作 | 关键点 |
| --- | --- | --- |
| ① | `gomobile bind` 编 `lanet.aar` | ABI 限 `arm,arm64,386`（云端打包**没有 x86_64**） |
| ② | 从 `lanet.aar` 抽 `classes.jar` | library 模块**不能直接依赖本地 .aar**，AGP 会直接报错 |
| ③ | Gradle 编插件 module | 末尾跑 `verifyNoLeak`，确认没有宿主类混进产物 |
| ④ | 校验交付包 | package.json 合法 / id 与注册名一致 / abis 与 aar 内一致 / 无宿主类泄漏 |
| ⑤ | 同步到 `sdk/uniapp-demo/nativeplugins/` | 示例工程开箱即可打包 |

前置依赖：JDK（`JAVA_HOME` 指向 17+）、Android SDK + NDK、`gomobile` 在 PATH。
脚本里的 `build_env()` 已经把本机路径写死，换机器要改这一段。

**为什么插件能本地编译**：DCloud 的离线 SDK 只在网盘分发，但 HBuilderX 自带了
`<HBuilderX>/plugins/uniapp-runextension/lib/dc_weexsdk-release.jar`，里面有
`UniModule` / `UniJSMethod` / `JSCallback` 的完整签名，直接 `compileOnly` 引它即可。
路径由 `plugin/gradle.properties` 的 `dcloudSdkDir` 控制。

## 二、接进 uni-app 项目

1. 把 `sdk/android-plugin/lanet-vpn/` 整个目录拷到工程的 `nativeplugins/` 下：

   ```
   your-uniapp/nativeplugins/lanet-vpn/package.json
   your-uniapp/nativeplugins/lanet-vpn/android/lanet.aar
   your-uniapp/nativeplugins/lanet-vpn/android/lanet-plugin.aar
   ```

2. 在 `manifest.json` 里勾选该插件（HBuilderX 可视化界面「App 原生插件配置 → 本地插件」），
   或直接写：

   ```json
   {
     "app-plus": {
       "distribute": {
         "android": {
           "minSdkVersion": 21,
           "targetSdkVersion": 35,
           "abiFilters": ["armeabi-v7a", "arm64-v8a", "x86"],
           "permissions": ["android.permission.INTERNET", "..."]
         }
       },
       "nativePlugins": { "lanet-vpn": {} }
     }
   }
   ```

3. 用**云打包**或**自定义调试基座**出包 —— 原生插件不能靠标准基座生效。

## 三、JS 调用

`utils/lanet.js` 是把原生方法包成 Promise 的一层，建议直接拷进项目：

```js
import * as lanet from '@/utils/lanet.js'

// 启动并入网（会先弹系统 VPN 授权框，同意后自动继续）
const res = await lanet.startAndWait({
  name: 'my-phone',
  network_key: 'yunloli',          // 必填，与其它成员一致
  bootstrap: ['/ip4/43.136.124.167/tcp/4001/p2p/12D3Koo…'],
  auto_accept: true,
})

const s = lanet.status()
s.virtual_ip    // '10.7.207.102' —— 拿到它才算真入网
s.peer_id
lanet.inviteCode()  // 连接码，发给别人即可让对端连进来
lanet.members()     // 成员列表
lanet.pending()     // 待审批节点，lanet.approve(peerId) 放行
await lanet.connect('lanet://12D3Koo…@/ip4/1.2.3.4/tcp/4001')  // 主动连一个节点
await lanet.stop()
```

约定：

- 同步方法返回 **JSON 文本**（Kotlin 侧统一走 String，不依赖宿主的 fastjson），
  `utils/lanet.js` 帮你 parse 好。
- 异步方法回调统一是 `{ok, stage, data, error}`，已 Promise 化成 resolve/reject。
- H5 / 小程序里 `requireNativePlugin` 返回空，`lanet.available()` 会返回 `false`，
  调用不抛异常——UI 用它决定要不要置灰按钮。

## 四、已知限制

- **必须 API 30+**：`foregroundServiceType="specialUse"` 与 `POST_NOTIFICATIONS` 是 30/33 起的要求。
- **云端打包 ABI 白名单**只有 `armeabi-v7a` / `arm64-v8a` / `x86`，**没有 x86_64**，
  所以不能用 MuMu 等 64 位 x86 模拟器跑；真机与 arm 模拟器正常。
- **首次启动一定弹 VPN 授权框**（系统行为，不可绕过）；拒绝后 `isAuthorized()` 为 false，
  重新 `start()` 会再次弹窗。
- **手机在 NAT 后建议 `auto_accept: true`**，否则每个陌生节点都要手工审批。
- 插件只暴露 `start` / `stop` / `status` 系列入口，**不提供内核级流量统计、分应用分流**；
  需要时改 `LanetVpnModule.kt` 后重跑 `build.py`。
- 产物 `lanet.aar`（38MB）与中间产物均**不入库**，由 `build.py` 现场生成。

## 五、调试排障

| 现象 | 原因 / 处理 |
| --- | --- |
| `requireNativePlugin` 返回空 | 用了标准基座，或没勾选 nativePlugins → 换云打包/自定义基座 |
| `start` 回调 `stage='awaiting_permission'` 后一直没入网 | 系统授权框被拒绝，或授权后服务启动失败 → 看 `lastError()` |
| `status()` 里没有 `virtual_ip` | 还没握手成功：核对 `network_key`、种子地址是否可达 |
| 列表能看到成员但 ping 不通 | 检查是否真拿到虚拟 IP；纯应用层模式（`want_tun:false`）不可 ping |
| 打包报找不到 `com.lanet.plugin.LanetVpnModule` | `package.json` 的 class 与 `_dp_nativeplugin` 注册不一致 → 跑 `build.py --verify-only` |

宿主若是自己配混淆，务必带上 `lanet-plugin/consumer-rules.pro` 里的规则：插件类继承
`UniModule` 并靠注解反射枚举，被裁掉就会「插件静默不生效」。
