package com.lanet.plugin

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject

/**
 * VpnService 授权代理。
 *
 * 为什么需要它：`VpnService.prepare()` 会拉起系统授权弹窗，结果通过
 * `onActivityResult` 回到**发起方 Activity**。uni-app 这类框架的宿主 Activity
 * 不会把 onActivityResult 让给插件，插件若直接调 prepare() 就永远收不到回调。
 *
 * 解法是插件自己起一个透明 Activity 完成「prepare → 弹窗 → 拿结果」，
 * 拿到 RESULT_OK 后再把真正的 Service 起起来，然后自己 finish。
 * 该 Activity 对用户不可见（Theme.Translucent.NoTitleBar + noHistory）。
 */
class VpnAuthProxyActivity : Activity() {

    companion object {
        private const val TAG = "LanetPlugin"
        private const val REQ_VPN = 0x1601

        const val EXTRA_NAME = "name"
        const val EXTRA_NETWORK_KEY = "network_key"
        const val EXTRA_BOOTSTRAP = "bootstrap"
        const val EXTRA_WANT_TUN = "want_tun"
        const val EXTRA_AUTO_ACCEPT = "auto_accept"
        const val EXTRA_VERSION = "version"

        /**
         * 请求建立 VPN 并启动节点。
         *
         * 若系统已授权（或不需要授权）会直接启服务、不弹任何窗；否则弹出一次
         * 系统授权框，用户同意后继续。
         */
        fun request(
            ctx: Context,
            name: String,
            key: String,
            bootstrap: List<String>,
            wantTun: Boolean = true,
            autoAccept: Boolean = false,
            version: String = "",
        ) {
            val i = Intent(ctx, VpnAuthProxyActivity::class.java).apply {
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                putExtra(EXTRA_NAME, name)
                putExtra(EXTRA_NETWORK_KEY, key)
                putStringArrayListExtra(EXTRA_BOOTSTRAP, ArrayList(bootstrap))
                putExtra(EXTRA_WANT_TUN, wantTun)
                putExtra(EXTRA_AUTO_ACCEPT, autoAccept)
                putExtra(EXTRA_VERSION, version)
            }
            ctx.startActivity(i)
        }

        /** 判断是否已拿到 VPN 授权（无需弹窗）。 */
        fun isAuthorized(ctx: Context): Boolean = try {
            VpnService.prepare(ctx) == null
        } catch (t: Throwable) {
            Log.w(TAG, "查询 VPN 授权状态失败", t)
            false
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val prep = VpnService.prepare(this)
        if (prep == null) {
            Log.i(TAG, "已授权，直接启动服务")
            launchService()
            finish()
            return
        }
        Log.i(TAG, "未授权，拉起系统授权弹窗")
        startActivityForResult(prep, REQ_VPN)
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQ_VPN) return
        if (resultCode == RESULT_OK) {
            launchService()
        } else {
            Log.w(TAG, "用户拒绝了 VPN 授权")
            LanetVpnService.recordError("用户拒绝了 VPN 授权")
        }
        finish()
    }

    private fun launchService() {
        LanetVpnService.start(
            this,
            intent.getStringExtra(EXTRA_NAME) ?: "android",
            intent.getStringExtra(EXTRA_NETWORK_KEY) ?: "",
            intent.getStringArrayListExtra(EXTRA_BOOTSTRAP) ?: arrayListOf(),
            intent.getBooleanExtra(EXTRA_WANT_TUN, true),
            intent.getBooleanExtra(EXTRA_AUTO_ACCEPT, false),
            intent.getStringExtra(EXTRA_VERSION) ?: "",
        )
    }

    /** 供 Module 复用：把 JS 传来的 options JSON 转成一串启动参数。 */
    internal data class Options(
        val name: String,
        val networkKey: String,
        val bootstrap: List<String>,
        val wantTun: Boolean,
        val autoAccept: Boolean,
        val version: String,
    ) {
        companion object {
            fun parse(json: String?): Options {
                val o = if (json.isNullOrBlank()) JSONObject() else JSONObject(json)
                val seeds = mutableListOf<String>()
                when (val raw = o.opt("bootstrap")) {
                    is JSONArray -> for (i in 0 until raw.length()) {
                        raw.optString(i).takeIf { it.isNotBlank() }?.let { seeds += it }
                    }
                    is String -> raw.split('\n', ',')
                        .map { it.trim() }
                        .filter { it.isNotEmpty() }
                        .let { seeds += it }
                }
                // 兼容单值写法：connect_seed / seed
                listOf("connect_seed", "seed").forEach { k ->
                    o.optString(k, "").takeIf { it.isNotBlank() }?.let { seeds += it }
                }
                return Options(
                    name = o.optString("name", "android").ifBlank { "android" },
                    networkKey = o.optString("network_key", ""),
                    bootstrap = seeds.distinct(),
                    // want_tun 默认 true：只有在需要纯应用层调试时才关掉。
                    wantTun = o.optBoolean("want_tun", true),
                    autoAccept = o.optBoolean("auto_accept", false),
                    version = o.optString("version", ""),
                )
            }
        }
    }
}
