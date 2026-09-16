package com.lanet.plugin

import android.content.ContentProvider
import android.content.ContentValues
import android.content.Context
import android.database.Cursor
import android.net.Uri
import android.util.Log

/**
 * 只为在进程启动时把 Application Context 交给 [LanetNode]。
 *
 * 背景：Unity 侧通过 `AndroidJavaClass.CallStatic` 调静态方法时，参数签名是按
 * **传入对象的实际类**推导的。把 `UnityPlayerActivity` 传给声明为 `Context` 的
 * 形参会推导出 `(Lcom/unity3d/player/UnityPlayerActivity;…)`，与真实签名
 * `(Landroid/content/Context;…)` 不匹配，JNI 直接 NoSuchMethodError；而让 C#
 * 退到 AndroidJNI 手写签名串既啰嗦又易错。
 *
 * 于是把 Context 的获取搬到 Java 侧：系统会在 `Application.onCreate` **之前**
 * 创建所有已声明的 ContentProvider，这里就能拿到 applicationContext 并缓存。
 * C# 侧只需调零参数或单 String 参数的方法（`startAuto` / `stopAuto` /
 * `isAuthorizedAuto`），签名永远正确。这也是 AndroidX Startup 的常规做法。
 *
 * 用 applicationContext 启动授权 Activity 是合法的——[VpnAuthProxyActivity.request]
 * 已带 `FLAG_ACTIVITY_NEW_TASK`，所以不依赖宿主 Activity 是否存活。
 */
class LanetInitProvider : ContentProvider() {

    override fun onCreate(): Boolean {
        return try {
            LanetNode.attach(context?.applicationContext)
            true
        } catch (t: Throwable) {
            Log.w(TAG, "初始化 Application Context 失败", t)
            false
        }
    }

    // 本 Provider 不提供数据访问，以下方法全部返回空值。
    override fun query(
        uri: Uri,
        projection: Array<out String>?,
        selection: String?,
        selectionArgs: Array<out String>?,
        sortOrder: String?,
    ): Cursor? = null

    override fun getType(uri: Uri): String? = null

    override fun insert(uri: Uri, values: ContentValues?): Uri? = null

    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = 0

    override fun update(
        uri: Uri,
        values: ContentValues?,
        selection: String?,
        selectionArgs: Array<out String>?,
    ): Int = 0

    private companion object {
        const val TAG = "LanetPlugin"
    }
}
