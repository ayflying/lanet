package com.lanet.demo

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.util.Log

/**
 * VpnService 授权代理。
 *
 * 为什么需要它：`VpnService.prepare()` 会拉起系统授权弹窗，结果通过
 * `onActivityResult` 回到发起方 Activity。uni-app 这类框架的宿主 Activity
 * 通常没办法给插件留 onActivityResult 的入口，插件若直接调用 prepare()
 * 就永远收不到回调。
 *
 * 解法是让插件起一个自己的透明 Activity 来完成「prepare → 弹窗 → 拿结果」，
 * 拿到 RESULT_OK 后再把真正的 Service 起起来，然后自己 finish 掉。
 * 这个 Activity 对用户是不可见的（Theme.Translucent.NoTitleBar + noHistory）。
 */
class VpnAuthProxyActivity : Activity() {

    companion object {
        private const val TAG = "LanetVpnAuth"
        private const val REQ_VPN = 0x1601

        const val EXTRA_NAME = "name"
        const val EXTRA_NETWORK_KEY = "network_key"
        const val EXTRA_BOOTSTRAP = "bootstrap"
        const val EXTRA_WANT_TUN = "want_tun"
        const val EXTRA_AUTO_ACCEPT = "auto_accept"

        /**
         * 请求建立 VPN 并启动节点。
         *
         * 若系统已授权（或不需要授权），会直接启服务，不弹任何窗；否则弹出
         * 一次系统授权框，用户同意后继续。
         */
        fun request(
            ctx: Context,
            name: String,
            key: String,
            bootstrap: List<String>,
            wantTun: Boolean = true,
            autoAccept: Boolean = false,
        ) {
            val i = Intent(ctx, VpnAuthProxyActivity::class.java).apply {
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                putExtra(EXTRA_NAME, name)
                putExtra(EXTRA_NETWORK_KEY, key)
                putStringArrayListExtra(EXTRA_BOOTSTRAP, ArrayList(bootstrap))
                putExtra(EXTRA_WANT_TUN, wantTun)
                putExtra(EXTRA_AUTO_ACCEPT, autoAccept)
            }
            ctx.startActivity(i)
        }

        /** 判断是否已经拿到过 VPN 授权（无需弹窗）。 */
        fun isAuthorized(ctx: Context): Boolean = VpnService.prepare(ctx) == null
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
        )
    }
}
