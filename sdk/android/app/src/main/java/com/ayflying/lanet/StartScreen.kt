package com.ayflying.lanet

import android.content.Context
import android.text.InputType
import android.view.Gravity
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

/**
 * 启动页（第一个界面）的视图构建器。
 *
 * 职责边界很窄：**只负责「让节点跑起来」**——本机名称、网络密钥、引导节点、一键启动。
 * 启动成功后本 Activity 会切到 [ConsoleWebView] 承载的网页控制台，所有成员管理、
 * 防火墙、端口转发、连接码分享都在那边完成。
 *
 * 为什么启动页必须留在原生而不是也丢进网页：
 * Android 的 VpnService 授权（`android.net.VpnService.prepare`）会弹一个**系统对话框**，
 * 它只能由 Activity 通过 startActivityForResult 拿到结果，网页里没有这个能力。
 * 除此之外这里不实现任何业务界面，避免和网页控制台重复。
 */
class StartScreen(context: Context) {

    private val ctx = context

    val nameInput: EditText = EditText(ctx).apply {
        hint = "本机名称，如 pixel-8"
        inputType = InputType.TYPE_CLASS_TEXT
        // 单行 + 限制最大宽度：多行输入框在小屏上会被撑高，把下面的按钮挤到屏幕外。
        maxLines = 1
    }
    val keyInput: EditText = EditText(ctx).apply {
        hint = "网络密钥（留空 = 本机专属网）"
        inputType = InputType.TYPE_CLASS_TEXT
        maxLines = 1
    }
    val seedInput: EditText = EditText(ctx).apply {
        hint = "引导节点地址，多个用逗号分隔（可空）"
        inputType = InputType.TYPE_CLASS_TEXT
        maxLines = 1
    }
    val startBtn: Button = Button(ctx).apply { text = "建立虚拟网卡并入网" }

    /**
     * 构建整页视图。
     *
     * 布局要点（这些都是真机踩出来的，别随手改回去）：
     *  - 最外层是 ScrollView，**并且**给内容加 `clipToPadding=false` + 上下留白，
     *    否则第一行输入框的标题会被状态栏压住、滚到顶也露不全——现象就是「顶部
     *    输入框超框、划不上去」。
     *  - 用 `fitsSystemWindows` 让内容避开状态栏/导航栏，而不是硬写 padding：
     *    刘海屏、手势条、折叠屏的内边距各不相同，写死的值必然在某个机型上出错。
     *  - 输入框统一单行，长内容横向滚动；纵向多行会让整页高度不确定。
     */
    fun build(): ViewGroup {
        val root = LinearLayout(ctx).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20), dp(16), dp(20), dp(24))
        }

        root.addView(TextView(ctx).apply {
            text = "Lanet"
            textSize = 22f
            setGravity(Gravity.START)
        })
        root.addView(TextView(ctx).apply {
            text = "建立虚拟网卡并加入网络"
            textSize = 13f
            setPadding(0, dp(4), 0, dp(16))
        })

        root.addView(fieldLabel("本机名称"))
        root.addView(nameInput)
        root.addView(fieldLabel("网络密钥"))
        root.addView(keyInput)
        root.addView(fieldLabel("引导节点（bootstrap，填已在网成员的地址/连接码）"))
        root.addView(seedInput)

        root.addView(startBtn, LinearLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.WRAP_CONTENT,
        ).apply { topMargin = dp(20) })

        root.addView(TextView(ctx).apply {
            text = "入网后进入控制台：成员、防火墙、端口转发、连接码都在页签里。"
            textSize = 12f
            setPadding(0, dp(12), 0, 0)
        })

        return ScrollView(ctx).apply {
            isFillViewport = true
            // 让内容避开状态栏/导航栏（刘海屏、手势条各机型不同，写死 padding 必错）。
            fitsSystemWindows = true
            addView(root, ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            ))
        }
    }

    private fun fieldLabel(t: String) = TextView(ctx).apply {
        text = t
        textSize = 12f
        setPadding(0, dp(12), 0, dp(2))
    }

    private fun dp(v: Int): Int = (v * ctx.resources.displayMetrics.density).toInt()
}
