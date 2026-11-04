# Android APK 的 CI 构建与签名

本文回答四件事：**CI 怎么出 APK**、**版本号怎么从 VERSION 派生**、**怎么切到正式签名**、
**本地怎么复现**。以及 CI 红了怎么排查（见最后一节）。

> 一句话现状：`main` 分支一有相关改动，`.github/workflows/android.yml` 就会现场用
> `gomobile` 编出 Go 核心 AAR、再用 Gradle 打成 `lanet-{版本}-android.apk`，
> 与 Windows / Linux 三个平台的压缩包一起挂到 **同一个** GitHub Release（tag `v{版本}`），
> 并进入 `sha256sums.txt`。**默认是 debug 签名包**——能直接装，但不是正式分发证书。

---

## 一、整体链路

```
sdk/go/mobile/*.go  --gomobile bind-->  lanet.aar  --cp-->  sdk/android/app/libs/lanet.aar
                                                                    |
                                                          ./gradlew assembleDebug
                                                                    |
                                                    lanet-0.5.108-android.apk
                                                                    |
                          artifact: lanet-apk / lanet-apk-sha256  --release.yml-->  GitHub Release v0.5.108
```

| 阶段 | 谁做 | 关键点 |
| --- | --- | --- |
| Go 核心 → AAR | `android.yml` 的 `gomobile bind` | 必须在 `sdk/go/mobile` 目录里跑（独立 go module） |
| AAR → APK | `android.yml` 的 `./gradlew` | 默认 `assembleDebug`；配了签名 Secret 自动改 `assembleRelease` |
| APK → Release | `release.yml` 的 `release` job | 与三个平台压缩包同一个 tag，`sha256sums.txt` 里多一行 |

### 为什么 android.yml 是独立 workflow

1. **环境完全不同**：Android 构建要 Java 17 + Android SDK + NDK，塞进 release.yml 的三行
   Go 交叉编译矩阵会让每个 runner 都白装一套工具链。
2. **失败可定位**：独立 job/step 命名，一眼看出死在 `gomobile` / NDK / `gradle` 哪一段。
3. **可复用**：`android.yml` 声明了 `workflow_call`，`release.yml` 直接
   `android: uses: ./.github/workflows/android.yml` 复用它。**只有一份构建逻辑**，
   不存在「发行里的 APK 和单独跑的 APK 不一致」。

`release.yml` 的依赖图（`needs`）：

```
stability ──> build（三平台压缩包）
android（独立，不等 stability）
       └─────────────┬──────────────┘
                  release（挂 Release + 合并 sha256）
```

- **android 不等 stability**：Android 构建与业务正确性无关，把它挡在发行前面只会放大
  与代码无关的偶发失败面（SDK 下载抖动、NDK 许可变更）。正确性由 `build`（已 `needs: stability`）把关。
- **release 等 build 和 android 两个**：`sha256sums.txt` 是 release job 当场合并出来的，
  必须两边都到齐才能一次写全。Android 构建失败 → 整个发行失败（有意为之：
  悄悄少一个 APK 的发行比一次红灯更难排查）。

---

## 二、版本号：versionName / versionCode 从 VERSION 派生

仓库根 `VERSION` 是**唯一真相源**（当前 `0.5.108`）。桌面包名、P2P 自更新清单、容器镜像标签
全读它，APK 也读它——否则会出现「同一版发行里 APK 报的版本和二进制不一样」
（`LanetBridge` 用 `BuildConfig.VERSION_NAME` 作为节点上报版本，写死会直接污染成员表的版本列）。

### 映射规则（可逆）

```
versionName = VERSION                         例：0.5.108
versionCode = major*1000000 + minor*10000 + patch
             = 0*1000000 + 5*10000 + 108 = 50108
```

- 上限：`minor ≤ 99`、`patch ≤ 9999`，结果落在 `0..99999999`，塞得进 Google Play 的
  `2100000000` 上限。
- **为什么不用时间戳**：APK 的 `versionCode` 必须单调递增才允许覆盖安装；时间戳每次都变，
  会让「同一版本号重复构建」得到不同的包，与 `sha256sums.txt` 的可复现校验冲突。
  三段式映射是纯函数，同一个 VERSION 永远得到同一个 versionCode。
- 这套规则在**两处**各实现一次，**必须同步改**：
  - `sdk/android/app/build.gradle.kts` 的 `versionCodeOf()`
  - `.github/workflows/android.yml` 的「打印版本号」步骤（bash 算术）
  
  两处差异会被「校验 APK 的 versionName / versionCode」步骤当场抓住：CI 用 `aapt2 dump badging`
  读 APK 里的**真值**去比对，而不是相信构建脚本的意图。实测 `0.5.108 → 50108`。

### 为什么用 `-P` 而不是让 Gradle 自己读 VERSION

- `-PversionName=... -PversionCode=...` 只有两个字符串参数，CI 与本地用同一条命令、行为一致；
  本地不传时 `build.gradle.kts` 自己退回读 VERSION，Android Studio 直接打开就能编译。
- 生成源码（generated file）方案要加 `sourceSets` 和 task 依赖，IDE 同步时容易拿到过期值，
  且说不清「这个数怎么来的」。

---

## 三、签名：默认 debug，配了 Secret 自动切 release

### 3.1 当前默认：debug 签名

没有配置签名 Secret 时，CI 走 `assembleDebug`，产出 **debug 签名**的
`lanet-{版本}-android.apk`。

> ⚠️ **debug 签名限制（务必读）**
>
> - 用的是 AGP 自动生成的公开 `debug.keystore`（口令 `android`、别名 `androiddebugkey`），
>   **不是你的私有证书**。任何持有同款 `debug.keystore` 的人都能做出能覆盖安装你这份包的 APK。
> - 因此它只适合**验证与内测**，**不适合正式分发**，更不能上应用商店。
> - 包之间可以互相覆盖安装（同一个 debug 证书），升级不会因签名不一致被拒。
> - 换到 release 签名后**不能覆盖安装**已装的 debug 包，必须先卸载再装
>   （`adb uninstall com.lanet.demo`），因为证书变了。旧包的 `files/` 目录（身份密钥、地址簿）
>   会一起被删，PeerID 与虚拟 IP 会变，需要重新入网/审批。

缺 Secret 时 workflow **不会失败**——这是刻意的：用户要的是「CI 能自动出 APK」，
配签名是可选增强。降级信息会写进该 job 的 **Job Summary**（含缺哪几个 Secret）。

### 3.2 切到 release 签名：配 4 个 Secret 即可

只要下面 4 个 Secret **全部**存在，CI 就自动改走 `assembleRelease` + release 签名，
其它什么都不用改（不需要改 workflow、不需要改 gradle 文件）。

| Secret 名称 | 内容 | 怎么取 |
| --- | --- | --- |
| `ANDROID_KEYSTORE_BASE64` | `.jks`/`.keystore` 文件的 base64（**单行**） | `base64 -w0 release.jks`（Linux/macOS）或见下方 PowerShell |
| `ANDROID_KEYSTORE_PASSWORD` | keystore 口令 | 建 keystore 时自己设的 |
| `ANDROID_KEY_ALIAS` | 密钥别名 | 建 keystore 时 `-alias` 的值 |
| `ANDROID_KEY_PASSWORD` | 密钥口令 | 通常与 storePassword 相同 |

#### 第 1 步：生成 release keystore（只需做一次，务必备份）

```bash
keytool -genkeypair -v \
  -keystore lanet-release.jks \
  -alias lanet \
  -keyalg RSA -keysize 4096 -validity 10000 \
  -storepass '<你的口令>' -keypass '<你的口令>' \
  -dname "CN=lanet, OU=release, O=lanet, L=CN, ST=CN, C=CN"
```

Windows PowerShell（本机无 JDK 时先用 Android Studio 自带 JBR 的 keytool）：

```powershell
keytool -genkeypair -v -keystore lanet-release.jks -alias lanet `
  -keyalg RSA -keysize 4096 -validity 10000 `
  -storepass "<你的口令>" -keypass "<你的口令>" `
  -dname "CN=lanet, OU=release, O=lanet, L=CN, ST=CN, C=CN"

# base64（-w0 在 PowerShell 里写作 -Encoding 后靠 -Raw 拼接，不能有换行）
[Convert]::ToBase64String([IO.File]::ReadAllBytes("lanet-release.jks")) |
  Set-Content -NoNewline -Encoding ascii lanet-release.jks.b64
```

> 🔒 `lanet-release.jks` 一旦丢失，**永远无法**为同一个 applicationId 发升级包（应用商店会拒收）。
> 请离线备份到密码管理器/保险柜。`.jks`、`.b64` 都不许入库——`.gitignore` 里已忽略
> `secrets/` 与 `*.jks` 类敏感物，但**主仓库的 `.gitignore` 并不覆盖任意位置的 `.jks`**，
> 放文件前请自己确认路径，或直接放进已被忽略的 `secrets/`。

#### 第 2 步：在 GitHub 仓库里配置 Secret

1. 打开仓库页面 → **Settings**（顶部标签）。
2. 左侧 **Secrets and variables** → **Actions**。
3. 点 **New repository secret**，逐个添加下表四个（名字必须**逐字**一致，大小写敏感）：

   | Name | Secret |
   | --- | --- |
   | `ANDROID_KEYSTORE_BASE64` | 粘贴 `.b64` 文件的全部内容（一整行，不要带换行/引号） |
   | `ANDROID_KEYSTORE_PASSWORD` | keystore 口令 |
   | `ANDROID_KEY_ALIAS` | `lanet` |
   | `ANDROID_KEY_PASSWORD` | 密钥口令 |

4. 加完后去 **Actions** → 左侧 `android` → **Run workflow**（手动跑一次）验证。
   Job Summary 里应出现 `✅ 检测到完整 release 签名配置，将产出 release 签名 APK`，
   且「验证签名并记录证书指纹」步骤打印的证书 DN 是你的 `CN=lanet...`，不再是
   `CN=Android Debug`。

#### 第 3 步（可选）：只让已配签名的仓库出包

如果希望「Secret 没配好就别发 APK」，把 `release.yml` 里 `android` job 加一行
`if: ${{ secrets.ANDROID_KEYSTORE_BASE64 != '' }}` 是**做不到的**——`if` 里不允许 `secrets` 上下文
（会直接让 workflow 校验失败、0 job 启动即红）。正确做法是像 `android.yml` 的
「判断是否有 release 签名配置」步骤那样，用 `env:` 把 Secret 中转成环境变量再在 shell 里判断。

### 3.3 workflow 内部是怎么实现的（给以后改 CI 的人看）

`sdk/android/app/build.gradle.kts` 的 `signingConfigs` 里用 `System.getenv` 读四个变量，
**四个都在且 keystore 文件存在**才 `create("release")`；否则 `release` buildType 的
`signingConfig` 回退到 `debug`。workflow 侧：

- 四个 Secret 通过 `env:` 注入「判断」步骤 → 输出 `signed=true/false` → 后续步骤用
  `if: steps.signing.outputs.signed == 'true'`。**全程没有 `if: secrets.X`**。
- keystore 由 base64 解码落到 `$RUNNER_TEMP/lanet-release.keystore`（**不在工作区**，
  避免被 upload-artifact 扫到），`chmod 600`，先用 `keytool -list` 验一遍别名与口令，
  最后在 `if: always()` 的清理步骤里删除。

---

## 四、APK 怎么进 Release

APK 只做两件事：**作为 Release 附件**、**作为 `sha256sums.txt` 的一行**。
**绝不进 `manifest.json`**（那是 P2P 自更新的 Ed25519 信任锚，只对三个平台的裸二进制签名；
`release.yml` 的「Merge update manifests」逻辑没有改动）。

1. `android.yml` 上传两个 artifact：
   - `lanet-apk` → `dist/lanet-{VERSION}-android.apk`
   - `lanet-apk-sha256` → `dist/lanet-{VERSION}-android.apk.sha256`
2. `release.yml` 的 release job **按名字精确下载**这两个 artifact 到 `dist/`
   （三个平台的压缩包仍用 `pattern: lanet-*`）。
3. 「Merge checksums」把 `*.sha256` 拼成 `sha256sums.txt`，并在拼之前**断言 APK 本体与
   它的 `.sha256` 都在**——缺了就当场失败，避免发布一个「校验表里有一行找不到文件」的发行包。
   合并后还会跑一次 `sha256sum -c` 自检。
4. 「Attach to GitHub Release」的 `files:` 里加了 `dist/lanet-*.apk`，tag 仍是
   `v${VERSION}`——**和三个平台同一个 Release**。

> ⚠️ 命名约束：artifact 名字（`lanet-apk*`）与 `release.yml` 的下载名**必须成对维护**。
> 如果以后有人把精确下载改回 `pattern: lanet-*`，APK 会被下载两遍，
> `sha256sums.txt` 里就会出现重复行。

---

## 五、本地复现

前提：本机装好 JDK 17、Android SDK（`platforms/android-35`、`build-tools/35.0.0`）、NDK r27，
并把 `ANDROID_HOME` / `ANDROID_NDK_HOME` / `JAVA_HOME` 指向它们（`PATH` 里要有 `$JAVA_HOME/bin`，
`gomobile` 最后一步要调 `javac`）。

```bash
# ① Go 核心 → AAR（必须在 sdk/go/mobile 里跑，它是独立 go module）
cd sdk/go/mobile
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20260908204917-8b95e45f8d3e
go install golang.org/x/mobile/cmd/gobind@v0.0.0-20260908204917-8b95e45f8d3e
gomobile init
gomobile bind -target=android/arm64,android/amd64 -androidapi 21 \
  -javapkg=com.lanet -ldflags="-checklinkname=0 -s -w" -o lanet.aar .
cp lanet.aar ../android/app/libs/lanet.aar

# ② AAR → APK。CI 里因为没有签名 Secret，走的是 assembleDebug。
cd ../android
gradle wrapper --gradle-version 8.11.1 --distribution-type bin   # 仓库不提交 wrapper，需要先生成
./gradlew clean assembleDebug
# 产物：app/build/outputs/apk/debug/app-debug.apk
```

版本号：不传 `-P` 时 Gradle 自己从 `../../VERSION` 读；想显式指定就用

```bash
./gradlew clean assembleDebug -PversionName=0.5.108 -PversionCode=50108
```

本地试 release 签名（等价于配了 4 个 Secret 后的 CI 路径）：

```bash
export ANDROID_KEYSTORE_PATH=/abs/path/lanet-release.jks
export ANDROID_KEYSTORE_PASSWORD='<口令>'
export ANDROID_KEY_ALIAS='lanet'
export ANDROID_KEY_PASSWORD='<口令>'
./gradlew clean assembleRelease
$ANDROID_HOME/build-tools/35.0.0/apksigner verify --print-certs \
  app/build/outputs/apk/release/app-release.apk
```

`clean` 不能省：增量打包复用旧条目布局时 zip 里会出现空洞（ZipFlinger 残影，APK 体积虚高），
见 [`sdk/android/README.md`](../../sdk/android/README.md) 第二节。

> 本仓库的开发者机器上**可能没有** JDK / Android SDK（例如纯 Windows 开发机），
> 那就别在本机跑 Android 构建——交给 CI。本地能做的验证见 `sdk/android/README.md`。

---

## 六、首次 CI 失败时怎么排查

按 job / step 的顺序从上往下查，**每一步的失败信息都刻意带上了定位前缀**：

| 现象（哪一步红） | 常见原因 | 怎么确认 |
| --- | --- | --- |
| `Set up Go` | `sdk/go/mobile/go.mod` 里的 `go 1.26.0` 还没有对应的稳定版 setup-go 镜像 | 看日志里 `go-version` 是否报 `no stable version`；必要时先在 `setup-go` 里临时指定 `go-version: '1.26'` |
| `安装 platform 35 / build-tools / NDK` | `sdkmanager` 组件名写错、或 `ndk;27.x` 版本号不存在（版本号会随 NDK 发版变） | `sdkmanager --list \| grep ndk` 看可用版本，然后改 `android.yml` 顶部的 `env.ANDROID_NDK_VERSION` |
| `gomobile bind（Go 核心 → lanet.aar）` | ① 在错误的目录跑（必须 `working-directory: sdk/go/mobile`）；② 少了 `-androidapi 21` → `unsupported API version 16`；③ 少了 `-checklinkname=0` → `invalid reference to net.zoneCache`；④ `ANDROID_NDK_HOME` 没导出 | 看报错原文，四个坑在 `sdk/go/mobile/node.go` 包注释里有完整说明 |
| `生成 gradle wrapper` | `setup-gradle` 的 `gradle-version` 与 AGP 8.6.1 不兼容（应保持 8.11.1） | 看日志里的 Gradle 版本号 |
| `编译 APK` | ① `libs/lanet.aar` 没拷进去（上一步失败但被 `\|\| true` 吞掉？我们没这么写，所以只会是上一步真的红了）；② JDK 版本不对（AGP 8.6 要求 17）；③ Kotlin/AGP 下载超时 | 日志里 `What went wrong:` 段落 |
| `校验 APK 的 versionName / versionCode` | VERSION 不是三段式，**或** gradle 与 workflow 两处映射规则不同步 | 日志会打印 `APK versionName=... 与 VERSION=... 不一致` |
| `验证签名并记录证书指纹` | debug keystore 生成失败（少见）；或 release 签名配置半套（口令不对） | 看 `apksigner verify` 原文；release 场景下先确认 `keytool -list` 那步过了 |
| `Download Android APK`（release job） | `android.yml` 没有上传 `lanet-apk`（例如改名了） | 确认 `android.yml` 的 upload 名字与 `release.yml` 的 download 名字一致 |
| `Merge checksums` | 缺 `lanet-*-android.apk` 或其 `.sha256`；或 `sha256sum -c` 自检失败 | 日志会打印 `dist/` 下的文件清单与 `sha256sums.txt` 全文 |
| `Attach to GitHub Release` | tag 冲突 / 权限不足（`permissions: contents: write` 是否被组织策略限制） | 看 action-gh-release 的报错 |

**通用排查手段**

1. **只看 Android**：Actions → 左侧 `android` → `Run workflow`（`workflow_dispatch`）。
   这条路径不碰三个平台的 Go 编译，最快能独立验证 Android 链路。
2. **看 Job Summary**：签名模式（debug/release）、APK 版本号、缺失的 Secret、证书指纹
   都写在 Summary 里，不用翻日志。
3. **下载 artifact**：`lanet-apk` 里就是最终那个 APK，`adb install` 直接装到手机验证。
4. **判据：APK 一定可装**。`apksigner verify` 通过 + `aapt2 dump badging` 版本号对 = 产物可用。

---

## 七、已知限制

1. **CI 只验「编得出来、装得上」，不验「跑得通 VPN」**。真实组网验证仍要手机实测，
   见 `sdk/android/README.md` 第三节与第四节（装到设备 + 带参启动 + 验证清单）。
2. **APK 是双架构包**（`arm64-v8a` + `x86_64`），约 30MB。只出 arm64 会让 MuMu/AVD 模拟器装不上，
   所以 x86_64 不能省；`useLegacyPackaging = true` 已把 `.so` 压缩存放，体积从 35MB+ 降到十几 MB。
3. **没有 ABI split**：没做按架构分包，一次下载两个架构。将来要降体积可以加 `splits { abi { ... } }`，
   但要同步改 Release 附件命名与校验和逻辑。
4. **debug 签名的包不能与 release 签名的包互相覆盖**，切换签名前必须先卸载（见 3.1 的警告）。
