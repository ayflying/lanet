package com.ayflying.lanet

import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.widget.Toast

/**
 * 内嵌网页控制台的 WebView 容器。
 *
 * 设计取舍：**控制界面完全复用 sdk/go/lanet/console/index.html**，不在 Kotlin 里
 * 重画一遍。理由是那份页面已经有成熟的页签（成员与链路 / 入向防火墙 / 端口转发 /
 * 公网入口 / 运行日志），重做一套必然出现「App 有、浏览器没有」或反之的漂移。
 * 原生这层只提供网页做不到的两件事：
 *   1. 系统分享（`shareText`）——复制连接码到剪贴板网页能做，但弹出系统分享面板不能；
 *   2. 返回键交给历史栈（见 [canGoBack]）。
 *
 * 安全边界：只加载 127.0.0.1 上的控制台。控制台本身没有任何认证，一旦允许 WebView
 * 跳外链，就等于把本机控制台暴露给任意网页（WebView 默认带同进程网络权限）。
 * 所以所有非本机地址的跳转一律拦截。
 */
class ConsoleWebView(context: Context) {

    /**
     * 供同包的 [asView] 与宿主 Activity 使用；不对外开放，避免外部持有一份 ctx 引用。
     * （Kotlin 的 private 会让同文件的其他顶层扩展函数也访问不到，故用 internal。）
     */
    internal val ctx = context

    /** 分享回调：由宿主 Activity 决定怎么弹系统分享面板（WebView 自己做不到）。 */
    var onShare: ((String) -> Unit)? = null

    /** 页面加载失败回调（例如 Go 节点已停止、端口已关闭）。 */
    var onLoadFailed: (() -> Unit)? = null

    val view: WebView = WebView(context).apply {
        settings.apply {
            // 关掉一切不必要的能力：控制台是本地静态页 + 本地 API，不需要插件、
            // 不需要跨域放大、更不该允许 file:// 访问。
            javaScriptEnabled = true
            domStorageEnabled = true
            allowFileAccess = false
            allowContentAccess = false
            // 控制台的 CSS 依赖正确的 viewport 才能自适应手机宽度；
            // WebView 默认不按设备宽度排版，会渲染成「桌面版缩得很小」。
            useWideViewPort = true
            loadWithOverviewMode = false
            builtInZoomControls = false
            displayZoomControls = false
        }
        webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(
                view: WebView,
                request: WebResourceRequest,
            ): Boolean {
                val url = request.url.toString()
                // 本机控制台放行，其他一律拦截（详见类注释的安全边界）。
                if (!isLocalConsoleUrl(url)) {
                    Toast.makeText(ctx, "已拦截外部跳转：$url", Toast.LENGTH_SHORT).show()
                    return true
                }
                return false
            }

            override fun onPageStarted(view: WebView, url: String, favicon: Bitmap?) {
                failCount = 0
            }

            override fun onReceivedError(
                view: WebView,
                request: WebResourceRequest,
                error: android.webkit.WebResourceError,
            ) {
                // 只关心主文档错误：子资源（favicon 等）失败不该让整个界面报错。
                if (!request.isForMainFrame) return
                // 节点停止/重启时控制台端口会短暂关闭，Go 侧重新监听后即可访问。
                // 这里不做自动重载（会与重连逻辑打架），只通知宿主决定是否回到启动页。
                failCount++
                if (failCount >= 2) onLoadFailed?.invoke()
            }
        }
        addJavascriptInterface(HostBridge(), "LanetAndroid")
    }

    private var failCount = 0

    fun load(url: String) {
        if (url.isEmpty()) {
            onLoadFailed?.invoke()
            return
        }
        view.loadUrl(url)
    }

    fun canGoBack(): Boolean = view.canGoBack()

    fun goBack() = view.goBack()

    fun destroy() {
        onShare = null
        onLoadFailed = null
        view.destroy()
    }

    /**
     * 提供给网页的桥对象，网页里通过 `window.LanetAndroid.shareText(text)` 调用。
     *
     * 方法上有 `@JavascriptInterface` 才会被注入到 JS 全局；漏注解的表现是
     * 网页里 `LanetAndroid` 存在但调不动（静默失败），排查很绕。
     */
    private inner class HostBridge {
        @JavascriptInterface
        fun shareText(text: String) {
            val payload = text.trim()
            if (payload.isEmpty()) return
            // JS 线程回调，切回 UI 线程再弹面板。
            view.post { onShare?.invoke(payload) }
        }

        @JavascriptInterface
        fun showToast(message: String) {
            view.post {
                Toast.makeText(ctx, message, Toast.LENGTH_SHORT).show()
            }
        }
    }

    companion object {
        /** 判断是否为本机回环地址上的控制台页面。 */
        fun isLocalConsoleUrl(url: String): Boolean =
            url.startsWith("http://127.0.0.1:") || url.startsWith("http://localhost:")
    }
}

/** 系统分享面板：把连接码交给任意 App（微信、便签、邮件…）。 */
fun Context.sharePlainText(text: String) {
    val send = Intent(Intent.ACTION_SEND).apply {
        type = "text/plain"
        putExtra(Intent.EXTRA_TEXT, text)
    }
    startActivity(Intent.createChooser(send, "分享连接码").apply {
        addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
    })
}

/** 包一层 FrameLayout，方便直接把 [ConsoleWebView.view] 塞进 Activity。 */
fun ConsoleWebView.asView(): ViewGroup = FrameLayout(ctx).apply {
    addView(view, FrameLayout.LayoutParams(
        ViewGroup.LayoutParams.MATCH_PARENT,
        ViewGroup.LayoutParams.MATCH_PARENT,
    ))
}
