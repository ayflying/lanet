package com.lanet.plugin

import android.content.Context
import android.util.Log
import org.json.JSONObject

/**
 * 宿主无关的 Android 门面：Unity、原生 Android、任意 JNI 调用方直接用它。
 *
 * 与 [LanetVpnModule] 的分工——Module 是 uni-app（DCloud）专属薄壳，只把
 * JS 参数与回调翻译成这里的调用；真正的行为一律在本类，因此**同一份
 * `lanet-plugin.aar` 同时服务 uni-app 与 Unity**，不必各维护一套。
 *
 * 为什么必须是静态方法：Unity 侧用 `AndroidJavaClass.CallStatic` 调用，
 * 它只能命中 Java 静态方法；Kotlin 的 `object` 必须给每个方法加
 * `@JvmStatic` 才会生成真正的 `public static`（否则只有 `INSTANCE.xxx`，
 * Unity 调不到）。改动本类时务必保持 `@JvmStatic` 不丢。
 *
 * 返回约定：一律返回 **JSON 文本**，形如
 *
 *     {"ok":true,"stage":"starting","data":"…"}
 *     {"ok":false,"error":"…"}
 *
 * Unity 侧用 `JsonUtility.FromJson<T>()` 解析，不必猜类型；`data` 字段是
 * 需要结构化数据（成员表等）时的内嵌 JSON 文本，再解析一层即可。
 */
object LanetNode {

    private const val TAG = "LanetPlugin"

    /** 门面版本，随 [version] 暴露；用于排查「客户端与 AAR 版本不匹配」。 */
    const val VERSION = "0.1.0"

    /**
     * 进程级 Application Context，由 [LanetInitProvider] 在进程启动时注入。
     *
     * 为什么需要它：Unity 侧用 `AndroidJavaClass.CallStatic` 调用只能命中
     * Java 静态方法，而参数签名又是按**传入对象的实际类**推导的——把
     * `UnityPlayerActivity` 传给形参 `Context` 会推导成
     * `Lcom/unity3d/player/UnityPlayerActivity;`，与真实签名
     * `Landroid/content/Context;` 不匹配。把 Context 的获取放到 Java 侧后，
     * C# 只需调 [startAuto] / [stopAuto] / [isAuthorizedAuto] 这类零参数或
     * 单 String 参数的方法，签名永远不会错。
     */
    @Volatile
    private var appCtx: Context? = null

    /** 由 [LanetInitProvider] 调用；宿主也可在自认为更早的时机主动调一次。 */
    @JvmStatic
    fun attach(context: Context?) {
        if (context != null) {
            appCtx = context.applicationContext ?: context
        }
    }

    // ------------------------------------------------------------------
    // 生命周期
    // ------------------------------------------------------------------

    /**
     * 无 Context 参数的启动入口（Unity / 任意 JNI 调用方）。
     *
     * 与 [start] 行为完全一致，只是改用 [LanetInitProvider] 注入的
     * Application Context。若 Provider 未生效（宿主 manifest 没合并到它），
     * 会返回明确的错误而不是抛异常。
     */
    @JvmStatic
    fun startAuto(optionsJson: String?): String {
        val c = appCtx ?: return error("原生层未初始化：LanetInitProvider 未生效（请确认 AAR 的 manifest 已合并）")
        return start(c, optionsJson)
    }

    /** 无 Context 参数的停止入口，配合 [startAuto] 使用。 */
    @JvmStatic
    fun stopAuto(): String {
        val c = appCtx ?: return error("原生层未初始化：LanetInitProvider 未生效")
        return stop(c)
    }

    /** 无 Context 参数的授权查询，配合 [startAuto] 使用。 */
    @JvmStatic
    fun isAuthorizedAuto(): Boolean = isAuthorized(appCtx)

    /**
     * 启动并入网。
     *
     * @param ctx 必须是能拉起 Activity 的 Context（Unity 传 currentActivity；
     *            只传 applicationContext 在需要弹授权框时会失败）。
     * @param optionsJson 见 [VpnAuthProxyActivity.Options.parse]：`name` /
     *            `network_key`（必填）/ `bootstrap`（数组或换行/逗号分隔字符串）/
     *            `seed` / `connect_seed` / `auto_accept` / `want_tun`（默认 true）/
     *            `version`。
     * @return `stage` 为 `awaiting_permission`（已弹系统授权框，用户同意后
     *         服务会自动起来，调用方应轮询 [status] 直到 `running=true`）
     *         或 `starting`（原先已授权，直接起服务，同样轮询）。
     *
     * 为什么不等「连上」再返回：授权弹窗是异步的，调用方（Unity 的
     * UnityPlayerActivity / uni-app 的宿主 Activity）拿不到 onActivityResult，
     * 所以「等就绪」只能交给调用方轮询。
     */
    @JvmStatic
    fun start(ctx: Context?, optionsJson: String?): String {
        if (ctx == null) return error("拿不到 Android Context，无法启动")
        return try {
            val o = VpnAuthProxyActivity.Options.parse(optionsJson)
            if (o.networkKey.isBlank()) {
                return error("network_key 不能为空：留空只会按 PeerID 派生「本机专属网」，与任何其他节点都不在同一网络，无法互相发现")
            }
            if (o.wantTun && !VpnAuthProxyActivity.isAuthorized(ctx)) {
                Log.i(TAG, "未获 VPN 授权，拉起授权代理 Activity")
                VpnAuthProxyActivity.request(
                    ctx, o.name, o.networkKey, o.bootstrap, o.wantTun, o.autoAccept, o.version,
                )
                return okJson(stage = "awaiting_permission")
            }
            LanetVpnService.start(
                ctx, o.name, o.networkKey, o.bootstrap, o.wantTun, o.autoAccept, o.version,
            )
            okJson(stage = "starting")
        } catch (t: Throwable) {
            Log.e(TAG, "启动失败", t)
            error(t.message ?: t.toString())
        }
    }

    /** 停止节点并下线虚拟网卡。幂等。 */
    @JvmStatic
    fun stop(ctx: Context?): String {
        if (ctx == null) return error("拿不到 Android Context，无法停止")
        return try {
            LanetVpnService.stop(ctx)
            // Service 的 onDestroy 不保证立刻走完，先清标记让调用方状态即时正确。
            LanetVpnService.resetRunning()
            okJson(stage = "stopping")
        } catch (t: Throwable) {
            Log.e(TAG, "停止失败", t)
            error(t.message ?: t.toString())
        }
    }

    // ------------------------------------------------------------------
    // 状态查询（同步返回 JSON 文本）
    // ------------------------------------------------------------------

    /** 节点状态 JSON，见 sdk/go/mobile 的 Node.Status；未运行返回 `{"running":false}`。 */
    @JvmStatic
    fun status(): String = LanetCore.status().toString()

    @JvmStatic
    fun isRunning(): Boolean = LanetCore.isRunning()

    /** 成员表（JSON 数组）。成员要出现的前提是双方已互相审批。 */
    @JvmStatic
    fun members(): String = LanetCore.members().toString()

    /** 待审批申请（JSON 数组）。 */
    @JvmStatic
    fun pending(): String = LanetCore.pending().toString()

    /** 地址簿（JSON 数组）。 */
    @JvmStatic
    fun peers(): String = LanetCore.peers().toString()

    /** 附近节点（JSON 数组）：发现到但未互信，可用于「发现新设备」列表。 */
    @JvmStatic
    fun nearby(): String = LanetCore.nearby().toString()

    /** 可作引导种子的本机 multiaddr 列表（JSON 数组）。 */
    @JvmStatic
    fun seedAddrs(): String = LanetCore.seedAddrs().toString()

    /** 本机连接码 `lanet://<PeerID>@<addr>,…`：发给别人，对端即可连进来。 */
    @JvmStatic
    fun inviteCode(): String = LanetCore.inviteCode()

    /** 是否已获得系统 VPN 授权（`true` 时 [start] 不会弹窗）。 */
    @JvmStatic
    fun isAuthorized(ctx: Context?): Boolean {
        if (ctx == null) return false
        return VpnAuthProxyActivity.isAuthorized(ctx)
    }

    /** 最近一次失败原因；无错误时返回空串。 */
    @JvmStatic
    fun lastError(): String = LanetVpnService.lastError ?: ""

    /** 门面版本。 */
    @JvmStatic
    fun version(): String = VERSION

    // ------------------------------------------------------------------
    // 交互操作
    // ------------------------------------------------------------------

    /**
     * 主动连接一个节点。address 支持裸 PeerID / 连接码（`lanet://…`）/ multiaddr。
     *
     * 这是入网的**关键动作**：实测对端主动拨入不会在本机产生待审批，只有本机
     * 主动填对方地址才会「先信任对方」，所以 UI 必须提供这个入口。
     * 返回 `data` 为 SDK 的 JSON 结果（含 `pending` / `searching` / `virtual_ip`）。
     */
    @JvmStatic
    fun connect(address: String?): String = try {
        val r = LanetCore.connect((address ?: "").trim())
        okJson(data = r.toString())
    } catch (t: Throwable) {
        Log.e(TAG, "连接失败", t)
        error(t.message ?: t.toString())
    }

    /** 同意/拒绝一个待审批节点。 */
    @JvmStatic
    fun approve(peerId: String?, approve: Boolean): String {
        val id = (peerId ?: "").trim()
        if (id.isEmpty()) return error("peer_id 不能为空")
        return try {
            LanetCore.approve(id, approve)
            okJson()
        } catch (t: Throwable) {
            Log.e(TAG, "审批失败", t)
            error(t.message ?: t.toString())
        }
    }

    /** 移除一个成员（撤信任 + 出地址簿 + 记墓碑）。 */
    @JvmStatic
    fun remove(peerId: String?): String = try {
        LanetCore.remove((peerId ?: "").trim())
        okJson()
    } catch (t: Throwable) {
        Log.e(TAG, "移除失败", t)
        error(t.message ?: t.toString())
    }

    // ------------------------------------------------------------------

    private fun okJson(stage: String = "", data: String = ""): String {
        val o = JSONObject()
        o.put("ok", true)
        if (stage.isNotEmpty()) o.put("stage", stage)
        if (data.isNotEmpty()) o.put("data", data)
        return o.toString()
    }

    private fun error(message: String): String {
        val o = JSONObject()
        o.put("ok", false)
        o.put("error", message)
        return o.toString()
    }
}
