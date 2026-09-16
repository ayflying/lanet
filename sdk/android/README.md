# Lanet Android 宿主工程

把 `sdk/go/mobile` 产出的 AAR 装进 Android，让手机成为 lanet 虚拟局域网的**正式成员**：
能 ping 通 `10.7.x.x`、能被群内任意 TCP/UDP 访问，而不只是经网关转发自己的应用流量。

## 目录

```
sdk/android/
├── app/
│   ├── libs/lanet.aar              ← 需自行编译，已在 .gitignore 中忽略
│   └── src/main/
│       ├── AndroidManifest.xml     VpnService 声明 + BIND_VPN_SERVICE + 前台服务
│       ├── java/com/lanet/demo/
│       │   ├── LanetBridge.kt          对 AAR 的薄封装（唯一改动面）
│       │   ├── LanetVpnService.kt      两阶段建卡 + 前台服务
│       │   ├── VpnAuthProxyActivity.kt 承接 VPN 授权弹窗（透明 Activity）
│       │   └── MainActivity.kt         验证用面板（纯代码搭 UI，零 res 布局）
│       └── res/                        图标与 app_name
└── dist/                           构建产物输出，已忽略
```

## 一、先编译 AAR

```bash
cd ../../sdk/go/mobile
export ANDROID_HOME=<android-sdk> ANDROID_NDK_HOME=<ndk> JAVA_HOME=<jdk>
export PATH="$JAVA_HOME/bin:$PATH"          # gomobile 最后一步要调 javac
gomobile bind -target=android/arm64,android/amd64 -androidapi 21 \
  -javapkg=com.lanet -ldflags="-checklinkname=0 -s -w" -o lanet.aar .
cp lanet.aar ../android/app/libs/lanet.aar
```

参数为什么长这样，见 `sdk/go/mobile/node.go` 的包注释（四个实测坑：`-androidapi` 下限、
`-javapkg` 是前缀拼 Go 包名、Go 1.23+ 的 linkname 校验要关、`-s -w` 瘦身）。

**x86_64 目标不能省**：MuMu / AVD 等模拟器是 x86_64，只出 arm64 的 AAR 在模拟器上装不上。

## 二、编译 APK

```bash
cd sdk/android
gradle clean assembleDebug          # 或直接用 Android Studio 打开本目录
# 产物：app/build/outputs/apk/debug/app-debug.apk（双架构约 29MB）
```

无 cmd.exe 的环境（如受限沙箱）里 `gradle.bat` 跑不起来，可直接调 Gradle 主类：

```bash
java -classpath "$GRADLE_HOME/lib/gradle-launcher-8.11.1.jar" \
  org.gradle.launcher.GradleMain clean assembleDebug --no-daemon --console=plain
```

改完 Kotlin 建议 `clean assembleDebug`，别只 `assembleDebug`——增量打包复用旧条目布局时，
zip 里会出现「文件实体大小远大于条目压缩大小累加」的空洞（ZipFlinger 残影），
APK 体积虚高。判据：

```python
import zipfile, os
z = zipfile.ZipFile("app-debug.apk")
print(os.path.getsize("app-debug.apk"), sum(i.compress_size for i in z.infolist()))
```

## 三、装到设备并带参启动

```bash
adb install -r app/build/outputs/apk/debug/app-debug.apk

adb shell am start -n com.lanet.demo/.MainActivity \
  --es name mumu-emu \
  --es netkey yunloli \
  --es seed "/ip4/192.168.50.170/tcp/49440/p2p/12D3KooW…" \
  --es peer "12D3KooW…" \
  --ez autostart true --ez autoaccept true --ez autoconnect true
```

| 参数 | 作用 |
| --- | --- |
| `name` | 成员表里显示的名字 |
| `netkey` | 网络密钥，与目标网络一致才能互相发现 |
| `seed` | 引导节点 multiaddr，填任意已在网成员的地址即可入网 |
| `peer` | 主动连接的对端（裸 PeerID / 连接码 / multiaddr） |
| `autostart` | 立即走一次「建卡并入网」 |
| `autoaccept` | 自动同意陌生节点申请（NAT 后的手机收不到反向拨号，需要它） |
| `autoconnect` | 入网后自动拨 `peer` |

`peer` 那一步不是可选项：**对端主动拨入不会在本机产生待审批**，手机不放 NAT 里时
必须由手机侧主动连一次，才会互相加入地址簿。

## 四、验证清单

UI 里「刷新状态 / 成员」按钮下方会打出状态区（虚拟 IP、网络组、**入向防火墙**、虚拟网卡
是否已接管、成员/待审批/已信任/附近列表）。整页可滚动，状态区在下方，别以为没显示。

```bash
# 手机 → 群内成员（应 0% 丢包）
adb shell ping -c 4 10.7.207.102

# 群内成员 → 手机（反向，靠 allow-all 防火墙放行）
ping -n 4 10.7.144.45
```

日志判据（`adb logcat | grep GoLog`）：

```
[tun] 已接管宿主虚拟网卡 tun0（fd=123）
[lanet-sdk] TUN 网卡 tun0 已就绪（虚拟 IP=10.7.144.45）
[router] inbound tunnel established from 10.7.207.102 peer=<peer.ID …>
```

## 五、已知限制（实测踩过的坑）

1. **Android 把 uid 2000（adb shell）排除在 VPN 之外**（日志里 `Uids: {1-1999, 2001-99999}`）。
   用 `adb shell` 起的监听进程**无法被群内访问**——它的回包不走 tun0。要验证「手机当服务端」，
   得让 App 自己的 uid 来跑：

   ```bash
   adb shell "run-as com.lanet.demo sh -c 'nohup toybox nc -L -p 9100 echo OK >/dev/null 2>&1 &'"
   # 然后从群内另一台机器 nc 10.7.144.45 9100
   ```

   反之 ICMP 由内核生成回复、走的是路由表，所以 `adb shell ping` 一直是通的，别拿它当数据面证据。

2. **入向防火墙默认必须 allow-all**。SDK 自身默认 `deny-all`，与官方 `pvn-node`（显式 allow-all）
   不一致，表现为「手机 ping 得通别人、别人 ping 不回手机」。`LanetBridge.start()` 已传
   `firewall_mode=allow-all`。安全边界靠连接审批，不靠这里。

3. **`.lanet` DNS 在手机上必然启动失败**（`listen udp 127.0.0.1:53: bind: permission denied`），
   非 root 应用绑不了 53 端口。虚拟 IP 直连不受影响，只是 `ping <成员名>.lanet` 不可用。

4. **minSdk 21**：与 `gomobile bind -androidapi 21` 对齐，低于该值 Go 运行时起不来。

5. 覆盖安装保留 `files/` 目录，身份密钥与地址簿都在里面，所以 PeerID 与虚拟 IP 不变，
   不必重新审批。

## 六、下一步：接进你的 App

本工程是可独立安装的验证 APK（也是这套能力的地基验证）。要让别的宿主拥有同样的能力，
把 `LanetVpnService` + gomobile 库封进 AAR 复用即可，仓库里已经有两条现成的宿主链路：

| 宿主 | 用哪个 | 说明 |
| --- | --- | --- |
| uni-app | [`sdk/android-plugin`](../android-plugin/README.md) | 原生插件（nativeplugin），HBuilderX 云打包出 APK |
| Unity | [`sdk/unity`](../unity/README.md) | UPM 包 `com.lanet.unity`，Android 平台生效 |
| 任意 Android 原生工程 | 直接用 `lanet-plugin.aar` | 调宿主无关门面 `com.lanet.plugin.LanetNode` 的静态方法 |

两条宿主链路**共用同一份 `lanet-plugin.aar`**：所有行为都在宿主无关门面
`com.lanet.plugin.LanetNode` 里，uni-app 侧的 `LanetVpnModule` 只是 DCloud 薄壳，
Unity 侧直接 `AndroidJavaClass.CallStatic`。因此改行为只需改门面一处，两边同时生效。
