plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.lanet.demo"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.lanet.demo"
        // 21 与 gomobile bind -androidapi 21 对齐：低于该值 Go 运行时无法启动。
        minSdk = 21
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
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
