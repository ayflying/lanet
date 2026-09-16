// lanet 移动端 Android 工程：把 Go 节点核心（gomobile 产出的 AAR）接进
// Android VpnService，让手机成为真正的网络成员。
//
// 版本刻意与本机 .gradle 缓存对齐（AGP 8.6.1 / Gradle 8.11.1 / Kotlin 2.1.20），
// 避免首次构建被迫联网下载整套工具链。
plugins {
    id("com.android.application") version "8.6.1" apply false
    id("org.jetbrains.kotlin.android") version "2.1.20" apply false
}
