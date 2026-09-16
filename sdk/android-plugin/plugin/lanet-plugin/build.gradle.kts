// uni-app 原生插件 module：lanet-vpn。
//
// 产出物 lanet-plugin.aar 只包含插件自身的类（LanetVpnModule / LanetVpnService /
// VpnAuthProxyActivity / LanetCore）与 AndroidManifest，**不含**任何编译期依赖：
//   - io.dcloud.feature.uniapp.* / com.taobao.weex.* 由宿主 APK 提供（compileOnly）
//   - com.lanet.mobile.*（gomobile 门面）由同目录的 lanet.aar 提供
//
// 这两条是硬约束，不是风格偏好：一旦混进插件 aar，宿主打包时就会 Duplicate class。
// 构建后用 verifyNoLeak 任务自检。
//
// 注意依赖写法：**library 模块不允许直接依赖本地 .aar 文件**（AGP 会报
// "Direct local .aar file dependencies are not supported when building an AAR"
// —— 因为 aar 里的类和资源不会被传递，产物是坏的）。所以这里依赖的是从
// lanet.aar 里抽出来的 classes.jar，抽取步骤在 ../build.py 里完成。
import java.util.zip.ZipFile

plugins {
    id("com.android.library")
    id("org.jetbrains.kotlin.android")
}

// HBuilderX 自带的 DCloud SDK 目录（见 gradle.properties）
val dcloudSdkDir: String = (findProperty("dcloudSdkDir") as String?)
    ?: throw GradleException(
        "gradle.properties 里缺少 dcloudSdkDir —— 需指向 HBuilderX 的 " +
            "plugins/uniapp-runextension/lib（内含 dc_weexsdk-release.jar）。"
    )

val dcloudJar = file("$dcloudSdkDir/dc_weexsdk-release.jar")
val plusJar = file("$dcloudSdkDir/lib.5plus.base-release.jar")

// 从 lanet.aar 抽出的 classes.jar（由 build.py 生成）
val lanetClassesJar = file("libs/lanet-classes.jar")

// 交付形态的 nativeplugin 包目录：../lanet-vpn/android/
val pluginAndroidDir = rootProject.layout.projectDirectory.dir("../lanet-vpn/android")

android {
    namespace = "com.lanet.plugin"
    compileSdk = 35

    defaultConfig {
        // 与 gomobile bind -androidapi 21 对齐：低于该值 Go 运行时起不来。
        minSdk = 21
        consumerProguardFiles("consumer-rules.pro")
    }

    compileOptions {
        // 刻意用 Java 8 目标：DCloud 云端打包的 JDK 是 1.8，而 lanet.aar
        // （gomobile 产出）本身就是 Java 8 字节码（major=52）。统一到 1.8
        // 可以排除「云端 D8 遇到高版本字节码」这一整类兼容风险，代价为零 ——
        // 插件本身没有用到任何 Java 8 之后的语言特性或 API。
        sourceCompatibility = JavaVersion.VERSION_1_8
        targetCompatibility = JavaVersion.VERSION_1_8
    }

    kotlinOptions {
        jvmTarget = "1.8"
    }

    // 插件不生成 BuildConfig：版本号由 JS 侧传入，避免依赖宿主的构建配置。
    buildFeatures {
        buildConfig = false
    }

    lint {
        abortOnError = false
    }
}

dependencies {
    // ---- compileOnly：只要编译期符号，运行期由宿主提供 ----

    // uni-app 插件 API：UniModule / AbsSDKInstance / UniJSCallback / UniJSMethod
    // 以及它们的父类 WXModule / WXSDKInstance（都在 dc_weexsdk-release.jar 内）。
    compileOnly(files(dcloudJar))
    if (plusJar.exists()) compileOnly(files(plusJar))

    // Go 节点核心的编译期符号（com.lanet.mobile.*）。运行期由插件包里的
    // android/lanet.aar 提供。
    compileOnly(files(lanetClassesJar))

    // Android 自带 org.json，无需声明。刻意不用 com.alibaba.fastjson：
    // 它是宿主依赖、可能被 R8 改名，插件边界一律走 String。
}

// 依赖缺失时给出人话提示，而不是让编译报一堆 Unresolved reference
gradle.taskGraph.whenReady { }
tasks.matching { it.name.startsWith("compile") }.configureEach {
    doFirst {
        if (!lanetClassesJar.exists()) {
            throw GradleException(
                "缺少 ${lanetClassesJar.path} —— 先跑 ../build.py（它会从 " +
                    "../lanet-vpn/android/lanet.aar 里抽出 classes.jar）。"
            )
        }
        if (!dcloudJar.exists()) {
            throw GradleException("找不到 DCloud SDK：$dcloudJar（核对 gradle.properties 的 dcloudSdkDir）")
        }
    }
}

// 把 release aar 收集到 nativeplugin 包目录，命名固定为 lanet-plugin.aar
val collectPluginAar by tasks.registering(Copy::class) {
    from(layout.buildDirectory.file("outputs/aar/lanet-plugin-release.aar"))
    into(pluginAndroidDir)
    rename { "lanet-plugin.aar" }
}

// 必须用 matching + configureEach（惰性）：
// AGP 的 assembleRelease 是延迟注册的，在脚本配置期用 tasks.named("assembleRelease")
// 会直接报 "Task with name 'assembleRelease' not found in project"。
tasks.matching { it.name == "assembleRelease" }.configureEach {
    finalizedBy(collectPluginAar)
}

// ---------------------------------------------------------------------------
// 自检：插件 aar 里绝不能出现宿主的类。
//
// 这是最容易犯又最难发现的错误 —— 编译器不报错、本机也能装，直到云端打包时
// 爆 Duplicate class 才发现。所以每次构建都验一遍。
//
// 写法注意：在 Gradle Kotlin DSL 里 `java.util.zip.ZipFile` 会被解析成 Gradle 的
// java 扩展而报 Unresolved reference: util —— 必须在文件顶部 import。
// ---------------------------------------------------------------------------
val verifyNoLeak by tasks.registering {
    dependsOn("assembleRelease")
    doLast {
        val aar = layout.buildDirectory.file("outputs/aar/lanet-plugin-release.aar")
            .get().asFile
        if (!aar.exists()) throw GradleException("未找到产物：$aar")

        val forbidden = listOf("io/dcloud/", "com/taobao/weex/", "com/lanet/mobile/")
        val leaked = ArrayList<String>()

        ZipFile(aar).use { outer ->
            val entry = outer.getEntry("classes.jar")
                ?: throw GradleException("aar 里没有 classes.jar（产物异常）")
            val tmp = File.createTempFile("lanet-plugin-classes", ".jar")
            try {
                outer.getInputStream(entry).use { input ->
                    tmp.outputStream().use { out -> input.copyTo(out) }
                }
                ZipFile(tmp).use { inner ->
                    val en = inner.entries()
                    while (en.hasMoreElements()) {
                        val name = en.nextElement().name
                        for (p in forbidden) {
                            if (name.startsWith(p)) {
                                leaked.add(name)
                                break
                            }
                        }
                    }
                }
            } finally {
                tmp.delete()
            }
        }

        if (leaked.isNotEmpty()) {
            throw GradleException(
                "插件 aar 泄漏了宿主类（会导致云端打包 Duplicate class）：\n" +
                    leaked.take(20).joinToString("\n") { "  $it" }
            )
        }
        logger.lifecycle("verifyNoLeak 通过：插件 aar 未包含 io.dcloud / weex / com.lanet.mobile 的任何类")
    }
}
