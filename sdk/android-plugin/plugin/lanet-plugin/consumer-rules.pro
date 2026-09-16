# 插件自带混淆规则：以 consumerProguardFiles 声明后，会被合并进宿主的 R8 配置。
# 宿主的 release 构建一旦开启混淆，没有这几条插件就会在运行期静默失效。

# uni-app 通过反射查 UniJSMethod 注解来枚举插件方法 —— Module 必须整类保留。
# 这条与 DCloud 官方文档给出的规则一致。
-keep public class * extends io.dcloud.feature.uniapp.common.UniModule { *; }
-keep public class * extends com.taobao.weex.common.WXModule { *; }
-keep public class * extends io.dcloud.feature.uniapp.common.UniDestroyableModule { *; }

# 注解本身与带注解的方法都不能被删改，否则反射找不到入口
-keepattributes RuntimeVisibleAnnotations,RuntimeInvisibleAnnotations
-keepclassmembers class * {
    @io.dcloud.feature.uniapp.annotation.UniJSMethod <methods>;
}

# 由 AndroidManifest 直接引用的组件
-keep class com.lanet.plugin.LanetVpnService { *; }
-keep class com.lanet.plugin.VpnAuthProxyActivity { *; }
# 初始化 Provider：系统按 manifest 里的全限定名实例化它，改名即启动期崩溃
-keep class com.lanet.plugin.LanetInitProvider { *; }

# 插件与 Go 核心之间的桥接类按名字调用
-keep class com.lanet.plugin.LanetCore { *; }

# 宿主无关门面：Unity 侧用 AndroidJavaClass("com.lanet.plugin.LanetNode")
# + CallStatic 按**类名与方法名**调用，R8 一旦改名或删方法就会在运行期抛
# NoSuchMethodError（编译期完全无感）。整类保留，方法签名也不许动。
-keep class com.lanet.plugin.LanetNode { *; }
