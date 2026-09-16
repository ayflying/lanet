package com.lanet.plugin

import android.content.Context
import android.util.Log
import io.dcloud.feature.uniapp.annotation.UniJSMethod
import io.dcloud.feature.uniapp.bridge.UniJSCallback
import io.dcloud.feature.uniapp.common.UniModule
import org.json.JSONObject

/**
 * uni-app 侧的原生插件入口（DCloud 专属薄壳）。
 *
 * 本类**只做两件事**：把 JS 传来的参数翻译成 [LanetNode] 的调用，再把
 * `{"ok","stage","data","error"}` JSON 转成 Map 回给 JS。所有行为都在
 * [LanetNode] 里实现——这样同一份 `lanet-plugin.aar` 能同时服务 uni-app
 * 与 Unity（Unity 用 `AndroidJavaClass("com.lanet.plugin.LanetNode")` 直接
 * 调静态方法），不必维护两套逻辑。改动行为请改 [LanetNode]。
 *
 * 协议约定（刻意全部走 String，不碰 fastjson）：
 *  - **入参**一律是 JSON 文本或纯字符串，不用 `com.alibaba.fastjson.JSONObject`。
 *    官方文档虽把 fastjson 列为可用参数类型，但它是宿主的依赖、可能被 R8 改名，
 *    插件跨版本引用容易在运行期炸掉；String 是最稳的边界类型。
 *  - **回调**统一回一个 Map：`{ok, stage, data, error}`，其中 `data` 是需要结构化
 *    数据时的 JSON 文本，由 JS 侧 `JSON.parse`。这样新增字段不需要改签名。
 *  - **同步方法**直接返回 JSON 文本（uni-app 反射调用支持非 void 返回）。
 *
 * 线程：`uiThread = true` 的走主线程（拉起授权 Activity、起前台服务）；
 * 网络类操作用 `false`，避免阻塞 UI（代价是调用会占住 JS 调用线程，拨号最长约 15s）。
 */
class LanetVpnModule : UniModule() {

    companion object {
        private const val TAG = "LanetPlugin"

        /** 与 [LanetNode.VERSION] 同源，供 JS 侧 `version()` 读取。 */
        const val PLUGIN_VERSION = "0.1.0"

        /**
         * 兜底 Context。
         *
         * 正常情况下用 `mUniSDKInstance.context`（uni-app 给的是 Activity，拉起
         * 授权弹窗需要它）。但插件方法也可能在页面尚未就绪时被调用，这时缓存一份
         * applicationContext 至少能保证 status/stop 之类的操作可用。
         */
        @Volatile
        private var cachedApp: Context? = null
    }

    // ------------------------------------------------------------------
    // 同步查询：直接返回，不经过回调
    // ------------------------------------------------------------------

    @UniJSMethod(uiThread = false)
    fun status(): String = LanetNode.status()

    @UniJSMethod(uiThread = false)
    fun isRunning(): Boolean = LanetNode.isRunning()

    @UniJSMethod(uiThread = false)
    fun members(): String = LanetNode.members()

    @UniJSMethod(uiThread = false)
    fun pending(): String = LanetNode.pending()

    @UniJSMethod(uiThread = false)
    fun peers(): String = LanetNode.peers()

    @UniJSMethod(uiThread = false)
    fun nearby(): String = LanetNode.nearby()

    @UniJSMethod(uiThread = false)
    fun seedAddrs(): String = LanetNode.seedAddrs()

    /** 本机连接码（`lanet://<PeerID>@<addr>,…`），发给别人即可让对端连进来。 */
    @UniJSMethod(uiThread = false)
    fun inviteCode(): String = LanetNode.inviteCode()

    /** 是否已获得系统 VPN 授权。未授权时 [start] 会先弹一次系统授权框。 */
    @UniJSMethod(uiThread = false)
    fun isAuthorized(): Boolean = LanetNode.isAuthorized(ctx())

    /** 最近一次失败原因；无错误时返回空串。 */
    @UniJSMethod(uiThread = false)
    fun lastError(): String = LanetNode.lastError()

    @UniJSMethod(uiThread = false)
    fun version(): String = LanetNode.VERSION

    // ------------------------------------------------------------------
    // 异步操作：结果经 UniJSCallback 回传
    // ------------------------------------------------------------------

    /**
     * 启动并入网。
     *
     * options JSON 支持：`name` / `network_key` / `bootstrap`（数组或换行分隔的
     * 字符串）/ `seed` / `connect_seed` / `auto_accept` / `want_tun`（默认 true）/
     * `version`。
     *
     * 返回值里的 `stage` 有两种：
     *  - `awaiting_permission` —— 已弹出系统 VPN 授权框，用户同意后服务会自动起来，
     *    JS 侧应继续轮询 [status] 直到 `running = true`；
     *  - `starting` —— 已直接启动服务（原先就授权过），同样轮询 [status]。
     */
    @UniJSMethod(uiThread = true)
    fun start(optionsJson: String?, callback: UniJSCallback?) {
        forward(callback, LanetNode.start(ctx(), optionsJson))
    }

    @UniJSMethod(uiThread = true)
    fun stop(callback: UniJSCallback?) {
        forward(callback, LanetNode.stop(ctx()))
    }

    /**
     * 主动连接一个节点。
     *
     * address 支持裸 PeerID / 连接码（`lanet://…`）/ multiaddr。
     *
     * 这是手机入网的关键动作：对端主动拨入**不会**在本机产生待审批，
     * 只有本机主动填对方地址才会「先信任对方」——所以 UI 必须有这个入口，
     * 干等是等不到入网的。
     */
    @UniJSMethod(uiThread = false)
    fun connect(address: String?, callback: UniJSCallback?) {
        forward(callback, LanetNode.connect(address))
    }

    /** options JSON：`{peer_id, approve}`；approve 为 false 时即拒绝并清理该节点。 */
    @UniJSMethod(uiThread = false)
    fun approve(optionsJson: String?, callback: UniJSCallback?) {
        val peerId = try {
            JSONObject(optionsJson ?: "{}").optString("peer_id", "")
        } catch (t: Throwable) {
            reply(callback, ok = false, error = "options 不是合法 JSON: ${t.message}")
            return
        }
        val approve = try {
            JSONObject(optionsJson ?: "{}").optBoolean("approve", true)
        } catch (t: Throwable) {
            true
        }
        forward(callback, LanetNode.approve(peerId, approve))
    }

    /** 移除一个成员（撤信任 + 出地址簿 + 记墓碑）。 */
    @UniJSMethod(uiThread = false)
    fun remove(peerId: String?, callback: UniJSCallback?) {
        forward(callback, LanetNode.remove(peerId))
    }

    // ------------------------------------------------------------------

    private fun ctx(): Context? {
        val c = try {
            mUniSDKInstance?.context
        } catch (t: Throwable) {
            Log.w(TAG, "从 UniSDKInstance 取 Context 失败", t)
            null
        }
        if (c != null) {
            cachedApp = c.applicationContext
            return c
        }
        return cachedApp
    }

    /** 把 [LanetNode] 返回的统一 JSON 拆成 JS 侧习惯的 Map 回调。 */
    private fun forward(cb: UniJSCallback?, json: String) {
        try {
            val o = JSONObject(json)
            reply(
                cb,
                ok = o.optBoolean("ok", false),
                stage = o.optString("stage", ""),
                data = o.optString("data", ""),
                error = o.optString("error", ""),
            )
        } catch (t: Throwable) {
            reply(cb, ok = false, error = "解析门面返回失败: ${t.message}")
        }
    }

    private fun reply(
        cb: UniJSCallback?,
        ok: Boolean,
        stage: String = "",
        data: String = "",
        error: String = "",
    ) {
        if (cb == null) {
            if (!ok) Log.w(TAG, "操作失败且无回调可回：$error")
            return
        }
        // 只用基本类型与 String：避免依赖宿主的 fastjson 序列化实现。
        val m = HashMap<String, Any>()
        m["ok"] = ok
        m["stage"] = stage
        m["data"] = data
        m["error"] = error
        try {
            cb.invoke(m)
        } catch (t: Throwable) {
            Log.w(TAG, "回调失败", t)
        }
    }
}
