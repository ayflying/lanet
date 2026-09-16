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

# 插件与 Go 核心之间的桥接类按名字调用
-keep class com.lanet.plugin.LanetCore { *; }
