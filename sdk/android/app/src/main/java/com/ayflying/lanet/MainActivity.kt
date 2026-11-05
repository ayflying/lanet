package com.ayflying.lanet

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.ViewGroup
import android.widget.FrameLayout
import android.widget.Toast

/**
 * App 主界面：**两屏**，切换关系一目了然。
 *
 *  1. 启动屏（[StartScreen]）：本机名称 / 网络密钥 / 引导节点 + 一键入网。
 *  2. 控制台屏（[ConsoleWebView]）：入网成功后自动切换，加载 Go 内置的网页控制台。
 *     成员与链路、入向防火墙、端口转发、公网入口、运行日志、本机连接码分享
 *     全部在这份网页里，按页签隔离。
 *
 * 为什么分屏而不是像以前那样全堆一页：启动参数和日常运维是两件事，操作频率
 * 差一个数量级。混在一起的结果是「想看一眼成员，得先滚过三个输入框」。
 *
 * 状态机（判定逻辑集中在 [syncScreen]）：
 *   - 节点未运行 → 启动屏
 *   - 节点运行中且有 console URL → 控制台屏
 *   - 节点运行中但还没拿到 URL（启动瞬间）→ 停在启动屏并继续轮询
 */
class MainActivity : Activity() {

    private lateinit var root: ViewGroup
    private lateinit var start: StartScreen
    private var console: ConsoleWebView? = null

    /** 当前显示的是哪一屏；null 表示还没决定。 */
    private var showingConsole: Boolean? = null

    /** WebView 当前加载的地址：用于避免轮询触发重复 load（见 [syncScreen]）。 */
    private var loadedUrl = ""

    private val ui = Handler(Looper.getMainLooper())
    private var polling = false

    /** autoconnect 用：待自动拨号的目标地址。 */
    private var autoConnectTarget: String? = null

    /** 是否自动同意陌生节点（由 adb 参数 autoaccept 控制，默认关=人工审批）。 */
    private var autoAccept = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Android 13+ 不申请通知权限的话前台服务通知不显示（服务本身仍能跑）。
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }

        // 根容器用 FrameLayout：两屏都是「整屏占满」，切换时 removeAllViews + addView
        // 即可，不需要 Fragment 那套生命周期开销（这个 App 只有两屏）。
        root = FrameLayout(this)
        setContentView(root)

        start = StartScreen(this)
        start.startBtn.setOnClickListener { toggle() }
        start.seedInput.setText(defaultSeed())

        applyIntentExtras()
        syncScreen()
        startPolling()
    }

    /**
     * 支持从 intent 带参启动，便于用 adb 脚本化验证（不必手点输入框）：
     *
     *	adb shell am start -n com.ayflying.lanet/.MainActivity \
     *	  --es name mix3 --es netkey yunloli \
     *	  --es seed "lanet://…" --ez autostart true
     *
     * autostart=true 时会直接触发一次「建卡并入网」。真机排查时这条命令比手点
     * 可靠得多，所以留在代码里而不是临时改。
     */
    private fun applyIntentExtras() {
        val i = intent ?: return
        i.getStringExtra("name")?.let { start.nameInput.setText(it) }
        i.getStringExtra("netkey")?.let { start.keyInput.setText(it) }
        i.getStringExtra("seed")?.let { start.seedInput.setText(it) }
        autoAccept = i.getBooleanExtra("autoaccept", false)
        i.getStringExtra("peer")?.takeIf { i.getBooleanExtra("autoconnect", false) }?.let {
            autoConnectTarget = it
        }
        if (i.getBooleanExtra("autostart", false)) {
            start.startBtn.post { toggle() }
        }
    }

    // ---------------------------------------------------------------- 屏幕切换

    /**
     * 根据节点实际状态决定显示哪一屏。反复调用是安全的（幂等），
     * 轮询里每秒都会走到这里，因此**不要**在这里做重活。
     */
    private fun syncScreen() {
        val running = LanetVpnService.running || LanetBridge.isRunning()
        val consoleUrl = if (running) LanetBridge.consoleUrl() else ""

        val wantConsole = running && consoleUrl.isNotEmpty()
        if (showingConsole == wantConsole) {
            // 已经在控制台屏：只在**地址真的变了**时重新加载。
            // 轮询是 1 秒一次，无条件 load 会让 WebView 每秒重新拉一次页面，
            // 表现为界面不停闪、输入框里的内容被冲掉、日志区滚动位置丢失。
            if (wantConsole && loadedUrl != consoleUrl) console?.load(consoleUrl)
            return
        }

        if (wantConsole) showConsole(consoleUrl) else showStart()
    }

    private fun showStart() {
        console?.destroy()
        console = null
        showingConsole = false
        loadedUrl = ""
        start.startBtn.text = "建立虚拟网卡并入网"
        root.removeAllViews()
        root.addView(start.build(), matchParent())
    }

    private fun showConsole(url: String) {
        val c = ConsoleWebView(this)
        c.onShare = { text -> sharePlainText(text) }
        c.onLoadFailed = {
            // 控制台加载不出来（节点已停）→ 退回启动屏，避免停在白屏。
            ui.post { if (showingConsole == true) { LanetVpnService.resetRunning(); syncScreen() } }
        }
        console = c
        showingConsole = true
        root.removeAllViews()
        root.addView(c.asView(), matchParent())
        c.load(url)
        loadedUrl = url
    }

    private fun matchParent() = ViewGroup.LayoutParams(
        ViewGroup.LayoutParams.MATCH_PARENT,
        ViewGroup.LayoutParams.MATCH_PARENT,
    )

    // ---------------------------------------------------------------- 启动/停止

    private fun toggle() {
        if (LanetVpnService.running || LanetBridge.isRunning()) {
            LanetVpnService.stop(this)
            LanetBridge.stop()
            LanetVpnService.resetRunning()
            syncScreen()
            return
        }

        val name = start.nameInput.text.toString().trim().ifEmpty { "android-" + Build.MODEL }
        val key = start.keyInput.text.toString().trim()
        val seeds = start.seedInput.text.toString()
            .split(",").map { it.trim() }.filter { it.isNotEmpty() }
        getSharedPreferences("lanet", Context.MODE_PRIVATE).edit()
            .putString("seed", seeds.joinToString(",")).apply()

        // 先要 VPN 授权（可能弹一次系统框），授权后由代理 Activity 起服务。
        VpnAuthProxyActivity.request(
            this, name, key, seeds, wantTun = true, autoAccept = autoAccept,
        )
        start.startBtn.text = "启动中…"
        startPolling()
    }

    // ---------------------------------------------------------------- 轮询

    private fun startPolling() {
        if (polling) return
        polling = true
        ui.postDelayed(object : Runnable {
            override fun run() {
                syncScreen()
                syncAutoConnect()
                // 停止后仍多跑几轮，让「启动中 → 已停止」的状态变化能被观察到。
                if (LanetVpnService.running || LanetBridge.isRunning()) {
                    ui.postDelayed(this, 1000)
                } else {
                    polling = false
                }
            }
        }, 800)
    }

    /** autoconnect：入网是异步的，必须等节点真的起来再拨，否则会拿到「节点未运行」。 */
    private fun syncAutoConnect() {
        val target = autoConnectTarget ?: return
        if (!LanetBridge.isRunning()) return
        autoConnectTarget = null
        try {
            LanetBridge.connect(target)
            toast("已自动连接 $target")
        } catch (t: Throwable) {
            toast("自动连接失败：${t.message}")
        }
    }

    // ---------------------------------------------------------------- 生命周期

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        // 返回键优先交给网页历史栈：控制台里点进详情页后，返回应该回上一页而不是直接退出。
        val c = console
        if (showingConsole == true && c != null && c.canGoBack()) {
            c.goBack()
            return
        }
        super.onBackPressed()
    }

    override fun onDestroy() {
        polling = false
        ui.removeCallbacksAndMessages(null)
        console?.destroy()
        console = null
        super.onDestroy()
    }

    /** 引导节点默认值：上一次填过的地址会留在本地，免得每次重敲。 */
    private fun defaultSeed(): String =
        getSharedPreferences("lanet", Context.MODE_PRIVATE).getString("seed", "") ?: ""

    private fun toast(m: String) = Toast.makeText(this, m, Toast.LENGTH_SHORT).show()
}
