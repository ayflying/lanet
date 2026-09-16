package com.lanet.demo

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import org.json.JSONArray

/**
 * 演示界面：不依赖任何 res 布局文件，全部用代码搭出来。
 *
 * 这样做的理由很实际——这个 Activity 的首要职责是「证明 AAR 里的 Go 节点
 * 能在真机上入网」，界面只是操作面板；用 XML 只会多出一堆和验证无关的文件。
 * 真正要把能力接进 uni-app 时走的是原生插件那条路（见 sdk/android-plugin）。
 *
 * 界面上的字段全部对应 sdk/go/mobile/node.go 里返回的 JSON 契约，键名不能
 * 凭感觉写：status 里的网络组指纹叫 group（不是 network_group），成员表里
 * 也没有 online 字段，只有 platform / version / path。
 */
class MainActivity : Activity() {

    private lateinit var nameInput: EditText
    private lateinit var keyInput: EditText
    private lateinit var seedInput: EditText
    private lateinit var connectInput: EditText
    private lateinit var approveInput: EditText
    private lateinit var output: TextView
    private lateinit var startBtn: Button

    /** autoconnect 用：待自动拨号的目标地址。 */
    private var autoConnectTarget: String? = null

    private val ui = Handler(Looper.getMainLooper())
    private var polling = false

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

        setContentView(buildUi())
        applyIntentExtras()
    }

    /**
     * 支持从 intent 带参启动，便于用 adb 脚本化验证（不必手点输入框）：
     *
     *	adb shell am start -n com.lanet.demo/.MainActivity \
     *	  --es name mix3 --es netkey yunloli \
     *	  --es seed "lanet://…" --ez autostart true
     *
     * autostart=true 时会直接触发一次「建卡并入网」。真机排查时这条命令比手点
     * 可靠得多，所以留在代码里而不是临时改。
     */
    private fun applyIntentExtras() {
        val i = intent ?: return
        i.getStringExtra("name")?.let { nameInput.setText(it) }
        i.getStringExtra("netkey")?.let { keyInput.setText(it) }
        i.getStringExtra("seed")?.let { seedInput.setText(it) }
        i.getStringExtra("peer")?.let { connectInput.setText(it) }
        autoAccept = i.getBooleanExtra("autoaccept", false)
        i.getStringExtra("peer")?.takeIf { i.getBooleanExtra("autoconnect", false) }?.let {
            autoConnectTarget = it
        }
        if (i.getBooleanExtra("autostart", false)) {
            startBtn.post { toggle() }
        }
    }

    private fun dp(v: Int): Int = (v * resources.displayMetrics.density).toInt()

    private fun buildUi(): ViewGroup {
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(16), dp(16), dp(16), dp(16))
        }

        root.addView(TextView(this).apply {
            text = "Lanet 移动端（Go 核心 + VpnService）"
            textSize = 18f
        })

        nameInput = EditText(this).apply { hint = "本机名称，如 pixel-8" }
        keyInput = EditText(this).apply { hint = "网络密钥（留空 = 本机专属网）" }
        seedInput = EditText(this).apply {
            hint = "引导节点地址，多个用逗号分隔（可空）"
            setText(defaultSeed())
        }
        connectInput = EditText(this).apply {
            hint = "对端连接码 lanet://… 或裸 PeerID"
            inputType = InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
        }
        approveInput = EditText(this).apply {
            hint = "填 PeerID（见下方待审批列表）"
            inputType = InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
        }

        root.addView(label("名称"))
        root.addView(nameInput)
        root.addView(label("网络密钥"))
        root.addView(keyInput)
        root.addView(label("引导节点（bootstrap，填已在网成员的地址/连接码）"))
        root.addView(seedInput)

        startBtn = Button(this).apply {
            text = "建立虚拟网卡并入网"
            setOnClickListener { toggle() }
        }
        root.addView(startBtn)

        root.addView(label("主动连接对端 —— 对方拨入不会进本机待审批，必须本机主动连一次"))
        root.addView(connectInput)
        root.addView(Button(this).apply {
            text = "连接"
            setOnClickListener { doConnect() }
        })

        root.addView(label("审批 —— 陌生节点申请入网时用（PeerID 见下方「待审批」）"))
        root.addView(approveInput)
        root.addView(Button(this).apply {
            text = "同意 / 拒绝"
            setOnClickListener { doApprove() }
        })

        root.addView(Button(this).apply {
            text = "刷新状态 / 成员"
            setOnClickListener { refresh() }
        })

        output = TextView(this).apply {
            textSize = 12f
            typeface = android.graphics.Typeface.MONOSPACE
            setTextIsSelectable(true)
            text = "尚未启动"
        }
        // 状态区直接进 root，由最外层 ScrollView 统一滚动。
        // 这里曾用「内层 ScrollView + weight=1」把状态区钉在底部，但模拟器/手机横屏时
        // 输入框和按钮加起来就占满屏幕，weight=1 被压缩到 0 高度，状态区（虚拟 IP、
        // 入向防火墙、成员表）整块看不见——实测 bounds 只剩 24px。整页一起滚才治本。
        root.addView(output)

        return ScrollView(this).apply { addView(root) }
    }

    private fun label(t: String) = TextView(this).apply {
        text = t
        textSize = 12f
        setPadding(0, dp(8), 0, 0)
    }

    /** 引导节点默认值：上一次填过的地址会留在本地，免得每次重敲。 */
    private fun defaultSeed(): String =
        getSharedPreferences("lanet", Context.MODE_PRIVATE).getString("seed", "") ?: ""

    private fun toggle() {
        if (LanetVpnService.running || LanetBridge.isRunning()) {
            LanetVpnService.stop(this)
            LanetBridge.stop()
            LanetVpnService.resetRunning()
            startBtn.text = "建立虚拟网卡并入网"
            refresh()
            return
        }
        val name = nameInput.text.toString().trim().ifEmpty { "android-" + Build.MODEL }
        val key = keyInput.text.toString().trim()
        val seeds = seedInput.text.toString().split(",").map { it.trim() }.filter { it.isNotEmpty() }
        getSharedPreferences("lanet", Context.MODE_PRIVATE).edit()
            .putString("seed", seeds.joinToString(",")).apply()

        // 先要 VPN 授权（可能弹一次系统框），授权后由代理 Activity 起服务。
        VpnAuthProxyActivity.request(
            this, name, key, seeds, wantTun = true, autoAccept = autoAccept,
        )
        startBtn.text = "停止"
        startPolling()
    }

    private fun doConnect() {
        val addr = connectInput.text.toString().trim()
        if (addr.isEmpty()) {
            toast("请先填连接码或 PeerID")
            return
        }
        try {
            val r = LanetBridge.connect(addr)
            output.text = "连接结果：\n$r\n\n" + render()
        } catch (t: Throwable) {
            toast("连接失败：${t.message}")
        }
    }

    private fun doApprove() {
        val pid = approveInput.text.toString().trim()
        if (pid.isEmpty()) {
            toast("请先填 PeerID")
            return
        }
        // 这个 Demo 用一个输入框配一个按钮省事：先试同意，失败再试拒绝。
        // 真实 UI 应该按行给两个明确按钮，并把返回的 name/虚拟 IP 回显出来。
        try {
            LanetBridge.approve(pid, true)
            toast("已同意 $pid")
        } catch (t: Throwable) {
            try {
                LanetBridge.approve(pid, false)
                toast("已拒绝 $pid")
            } catch (t2: Throwable) {
                toast("审批失败：${t2.message}")
            }
        }
        refresh()
    }

    private fun refresh() {
        if (LanetVpnService.lastError != null && !LanetBridge.isRunning()) {
            output.text = "错误：${LanetVpnService.lastError}"
            return
        }
        output.text = try {
            render()
        } catch (t: Throwable) {
            "读取状态失败：${t.message}"
        }
    }

    private fun render(): String {
        val sb = StringBuilder()
        val st = LanetBridge.status()
        sb.append("运行中：${st.optBoolean("running", false)}\n")
        st.optString("virtual_ip", "").takeIf { it.isNotEmpty() }
            ?.let { sb.append("虚拟 IP：$it\n") }
        st.optString("peer_id", "").takeIf { it.isNotEmpty() }
            ?.let { sb.append("PeerID：$it\n") }
        st.optString("group", "").takeIf { it.isNotEmpty() }
            ?.let { sb.append("网络组：$it\n") }
        st.optString("firewall", "").takeIf { it.isNotEmpty() }
            ?.let { sb.append("入向防火墙：$it\n") }
        if (st.optInt("tun_fd", 0) > 0) {
            sb.append("虚拟网卡：已接管 (fd=${st.optInt("tun_fd")})\n")
        } else {
            sb.append("虚拟网卡：未接管（应用层模式，虚拟 IP 不可 ping）\n")
        }

        appendList(sb, "成员", LanetBridge.members())
        appendList(sb, "待审批", LanetBridge.pending())
        appendList(sb, "已信任", LanetBridge.peers())
        appendList(sb, "附近", LanetBridge.nearby())

        val invite = LanetBridge.inviteCode()
        if (invite.isNotEmpty()) sb.append("\n本机连接码（发给对方）：\n$invite\n")
        return sb.toString()
    }

    private fun appendList(sb: StringBuilder, title: String, arr: JSONArray) {
        sb.append("\n$title (${arr.length()})：\n")
        for (i in 0 until arr.length()) {
            val o = arr.optJSONObject(i) ?: continue
            val name = o.optString("name", "").ifEmpty { "-" }
            val ip = o.optString("virtual_ip", o.optString("last_ip", "-"))
            val pid = o.optString("peer_id", "").take(16)
            val extra = listOf(
                o.optString("version", ""),
                o.optString("platform", "").ifEmpty { o.optString("source", "") },
                o.optString("reason", ""),
            ).filter { it.isNotEmpty() }.joinToString(" ")
            sb.append("  · $name  $ip  $extra  $pid\n")
        }
    }

    private fun startPolling() {
        if (polling) return
        polling = true
        ui.postDelayed(object : Runnable {
            override fun run() {
                refresh()
                // autoconnect：入网是异步的，必须等节点真的起来再拨，否则
                // 会拿到「节点未运行」。放在轮询里比 sleep 猜测可靠。
                autoConnectTarget?.let { target ->
                    if (LanetBridge.isRunning()) {
                        autoConnectTarget = null
                        connectInput.setText(target)
                        doConnect()
                    }
                }
                if (LanetVpnService.running || LanetBridge.isRunning()) {
                    ui.postDelayed(this, 3000)
                } else {
                    polling = false
                }
            }
        }, 1500)
    }

    private fun toast(m: String) = Toast.makeText(this, m, Toast.LENGTH_SHORT).show()
}
