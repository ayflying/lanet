plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// =================================================================================
// 版本号：从仓库根 VERSION 派生（唯一真相源），命令行 -P 可覆盖。
//
// 为什么不用硬编码：桌面端的发行包名、P2P 自更新清单、容器镜像标签全都读根目录
// VERSION，APK 若还写死 0.1.0，就会出现「同一版发行里 APK 报的版本和二进制不一样」。
// LanetBridge 用 BuildConfig.VERSION_NAME 当节点上报版本，写死会直接污染成员表的版本列。
//
// 为什么用 -P 而不是 Gradle 自己读文件：
//   1. Gradle 的构建脚本运行在 -P 传来的 Java Properties 里，读仓库根文件要拿
//      rootProject.rootDir 往上跳两级，再处理 CRLF/末尾换行，脆弱且难测；
//      命令行只要 `-PversionName=0.5.108 -PversionCode=50108` 两个字符串，CI 与本地
//      用同一条命令，行为完全一致（本地手敲时也有默认值兜底，见下）。
//   2. 生成源码（generated file）方案要加 sourceSets 和 task 依赖，IDE 同步时容易
//      拿到过期值，且没法解释「这个数怎么来的」。
//   取舍：CI 里必须显式传 -P，且 dev/CI 会校验它与 VERSION 一致（见 android.yml 的
//   「计算并校验版本号」步骤）；本地不传时退回下面的兜底值，保证 Android Studio
//   直接打开就能编译。
//
// versionCode 映射（可逆，见 docs/android-release.md）：
//   versionCode = major*1000000 + minor*10000 + patch
//   0.5.108 → 0*1000000 + 5*10000 + 108 = 50108
//   上限：minor ≤ 99、patch ≤ 9999（够用到 0.99.9999），得到 0..99999999，塞得进
//   Google Play 要求的 2100000000 上限。
//   刻意不用语义化版本库或时间戳：APK 的 versionCode 必须单调递增才允许覆盖安装，
//   时间戳每次构建都变，会让「同一版本号重复构建」得到不同包，与 sha256sums.txt 的
//   可复现校验冲突；而三段式映射是纯函数，同一个 VERSION 永远得到同一个 versionCode。
// =================================================================================
fun readRepoVersion(): String {
    // 仓库根 VERSION：sdk/android/app → 上三级就是仓库根（sdk/android/app/lanet…）。
    val versionFile = rootProject.rootDir.parentFile.parentFile.resolve("VERSION")
    if (!versionFile.isFile) return "0.0.0"
    return versionFile.readText().trim()
}

// 语义化版本三段式 → Android versionCode（可逆映射，见上方注释）。
// 解析失败直接抛错：宁可让 CI 在配置阶段就红，也不要悄悄打出一个 versionCode=0 的包。
fun versionCodeOf(name: String): Int {
    val parts = name.trim().split(".")
    require(parts.size == 3) { "VERSION 必须是三段式（如 0.5.108），实际为 \"$name\"" }
    val nums = parts.map {
        requireNotNull(it.toIntOrNull()) { "VERSION 段必须是数字，实际为 \"$name\"" }
    }
    val (major, minor, patch) = nums
    require(minor in 0..99) { "VERSION 次版本号超出映射上限 99：$name" }
    require(patch in 0..9999) { "VERSION 修订号超出映射上限 9999：$name" }
    return major * 1_000_000 + minor * 10_000 + patch
}

val repoVersion: String = readRepoVersion()

// -P 优先，读不到再用 VERSION 派生（本地 `gradle assembleDebug` 也能直接跑）。
val apkVersionName: String =
    (project.findProperty("versionName") as String?)?.trim()?.takeIf { it.isNotEmpty() } ?: repoVersion
val apkVersionCode: Int =
    (project.findProperty("versionCode") as String?)?.trim()?.takeIf { it.isNotEmpty() }?.toIntOrNull()
        ?: versionCodeOf(apkVersionName)

android {
    namespace = "com.lanet.demo"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.lanet.demo"
        // 21 与 gomobile bind -androidapi 21 对齐：低于该值 Go 运行时无法启动。
        minSdk = 21
        targetSdk = 35
        versionCode = apkVersionCode
        versionName = apkVersionName
    }

    // =============================================================================
    // 签名配置：只在 CI 提供了 ANDROID_KEYSTORE_* 时才创建 release signingConfig。
    //
    // 必需的环境变量（全部存在才启用，缺一不可，避免半套配置打出「以为签名了其实
    // 没签」的包）：
    //   ANDROID_KEYSTORE_PATH       CI 里由 Secret ANDROID_KEYSTORE_BASE64 解码落地到
    //                               $RUNNER_TEMP 下的临时密钥库文件（构建结束随 runner
    //                               销毁；刻意不放工作区，避免被 upload-artifact 扫到）
    //   ANDROID_KEYSTORE_PASSWORD   Secret ANDROID_KEYSTORE_PASSWORD
    //   ANDROID_KEY_ALIAS           Secret ANDROID_KEY_ALIAS
    //   ANDROID_KEY_PASSWORD        Secret ANDROID_KEY_PASSWORD
    //
    // 为什么用环境变量而不是 Secrets 的直接值：Gradle 配置脚本只认环境变量/属性，
    // 且密钥一旦写进 gradle.properties 就有入库风险；环境变量只在构建进程内可见。
    //
    // 为什么「缺配置就 export debug 签名，而不是留一个 signed=null 的 release」：
    // 没有签名配置时 assembleRelease 会产出 app-release-unsigned.apk，那个包**装不上**
    // （Android 7+ 拒绝未签名 APK）。CI 的交付目标是「一个能装的包」，所以缺 Secret 时
    // 走 assembleDebug（AGP 自动用 ~/.android/debug.keystore 签名，可直接 adb install），
    // 并由 android.yml 在 Release 说明与 Workflow Summary 里标明「debug 签名」。
    // =============================================================================
    signingConfigs {
        val keystorePath = System.getenv("ANDROID_KEYSTORE_PATH")
        val keystorePassword = System.getenv("ANDROID_KEYSTORE_PASSWORD")
        val keyAlias = System.getenv("ANDROID_KEY_ALIAS")
        val keyPassword = System.getenv("ANDROID_KEY_PASSWORD")
        val signingReady = listOf(keystorePath, keystorePassword, keyAlias, keyPassword)
            .all { !it.isNullOrBlank() } && file(keystorePath!!).isFile

        if (signingReady) {
            logger.lifecycle("lanet: 使用 release 签名配置（alias=$keyAlias）")
            create("release") {
                storeFile = file(keystorePath!!)
                storePassword = keystorePassword
                this.keyAlias = keyAlias
                this.keyPassword = keyPassword
            }
        } else {
            logger.lifecycle("lanet: 未提供 ANDROID_KEYSTORE_* 环境变量，release 构建将回退 debug 签名")
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // 有 release 密钥就用 release 签名；没有就复用 AGP 自动生成的 debug 签名
            // （证书是公开的 debug 证书：任何持有对应 debug.keystore 的人都能覆盖安装，
            // 只适合验证/内测，不能当正式发行包）。
            signingConfig = if (signingConfigs.findByName("release") != null) {
                signingConfigs.getByName("release")
            } else {
                signingConfigs.getByName("debug")
            }
        }
        debug {
            // debug 也带上正式版本号，便于「同一个 APK 报的版本」和文件命名对得上。
            versionNameSuffix = ""
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    // AGP 8 起 BuildConfig 默认不生成，而 LanetBridge 要用 BuildConfig.VERSION_NAME
    // 作为节点上报版本，必须显式打开。
    buildFeatures {
        buildConfig = true
    }

    // AAR 内的 libgojni.so 已按 ABI 分目录放置，无需 ndk abiFilters；
    // 只打 arm64-v8a 会让旧设备装不上，这里保留 AAR 里的全部 ABI。
    //
    // useLegacyPackaging = true（对应 manifest 的 extractNativeLibs=true）：
    // AGP 默认把 .so 以 Stored 方式放进 APK 以便 mmap，而我们的 libgojni.so
    // 未压缩就有 34MB，APK 会跟着变成 35MB+。改成压缩存放后 APK 只剩十几 MB，
    // 代价是安装时解压到 /data/app/…/lib（安装后占用不变）。对「编译一个能
    // 快速传到手机装上的 APK」这个目标，传输体积比启动时的 mmap 更值钱。
    packaging {
        jniLibs {
            useLegacyPackaging = true
        }
    }
}

dependencies {
    // gomobile 产出的 AAR：Go 节点核心（libp2p + wireguard/tun + sqlite）
    // 加 com.lanet.mobile 门面类。生成方式见 sdk/go/mobile 的包注释。
    implementation(files("libs/lanet.aar"))
}
