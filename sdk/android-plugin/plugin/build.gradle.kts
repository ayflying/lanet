// uni-app 原生插件的编译工程。
//
// 为什么需要它：DCloud 规定「生成 uni-app 插件」的交付形态是**预编译好的 aar**，
// 不是源码。所以这里把插件 module 编成 lanet-plugin.aar，再和 Go 核心的
// lanet.aar 一起放进 ../lanet-vpn/android/，组成可直接拷进 uni-app 项目的
// nativeplugins 包。
//
// 编译依赖里的 DCloud SDK 来自 HBuilderX 自带的 jar（见 gradle.properties 的
// dcloudSdkDir），全部以 compileOnly 引入 —— 它们必须由宿主 APK 提供，
// 一旦被打进插件 aar 就会和宿主的同名类冲突（官方文档明确警告过这点）。
plugins {
    id("com.android.library") version "8.6.1" apply false
    id("org.jetbrains.kotlin.android") version "2.1.20" apply false
}
