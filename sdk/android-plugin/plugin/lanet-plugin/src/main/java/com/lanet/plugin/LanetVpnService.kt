package com.lanet.plugin

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.net.wifi.WifiManager
import android.os.Build
import android.os.ParcelFileDescriptor
import android.util.Log
import java.io.File

/**
 * 把 Go 节点核心接到 Android 的虚拟网卡上。
 *
 * 为什么必须走 VpnService：非 root 应用无法自行打开 /dev/net/tun，
 * `VpnService.establish()` 是唯一合法途径；它返回的 fd 由 Go 侧的
 * `tundevice.NewFromFD`（wireguard 的 `CreateUnmonitoredTUNFromFD`）接管，
 * 这与 wireguard-android 是同一套做法。
 *
 * 建卡分两阶段，原因见 [startWithTun] 的注释。
 */
class LanetVpnService : VpnService() {

    companion object {
        private const val TAG = "LanetPlugin"
        private const val CHANNEL_ID = "lanet-vpn"
        private const val NOTIFICATION_ID = 1001

        const val ACTION_START = "com.lanet.plugin.action.START"
        const val ACTION_STOP = "com.lanet.plugin.action.STOP"
        const val EXTRA_NAME = "name"
        const val EXTRA_NETWORK_KEY = "network_key"
        const val EXTRA_BOOTSTRAP = "bootstrap"
        const val EXTRA_WANT_TUN = "want_tun"
        const val EXTRA_AUTO_ACCEPT = "auto_accept"
        const val EXTRA_VERSION = "version"

        /**
         * 虚拟网段：成员分散在整个 10.7.0.0/16，掩码必须 /16（见 SDK 的
         * ConfigureTUN 注释）—— 用 /24 会让跨段流量走物理网关，
         * 症状是「成员列表看得见，但 ping 不通」。
         *
         * 注意 addRoute 的签名是 addRoute(String address, int prefixLength)：
         * 第一个参数要**纯数字地址**，不能带 "/16"，否则会在 establish() 之前抛
         * IllegalArgumentException: Not a numeric address。
         */
        private const val VNET_NETWORK = "10.7.0.0"
        private const val VNET_PREFIX = 16
        private const val MTU = 1400

        @Volatile
        var running: Boolean = false
            private set

        @Volatile
        var lastError: String? = null
            private set

        /** 虚拟 IP，供 Module 展示；未连接时为 null。 */
        @Volatile
        var virtualIp: String? = null
            private set

        /** 由授权代理在用户拒绝授权时写入，UI 才能把「为什么没起来」讲清楚。 */
        fun recordError(msg: String) {
            lastError = msg
        }

        /** 手动停服后清掉运行标记（Service 可能没走完 onDestroy）。 */
        fun resetRunning() {
            running = false
            virtualIp = null
        }

        fun start(
            ctx: Context,
            name: String,
            networkKey: String,
            bootstrap: List<String>,
            wantTun: Boolean = true,
            autoAccept: Boolean = false,
            version: String = "",
        ) {
            val i = Intent(ctx, LanetVpnService::class.java).apply {
                action = ACTION_START
                putExtra(EXTRA_NAME, name)
                putExtra(EXTRA_NETWORK_KEY, networkKey)
                putStringArrayListExtra(EXTRA_BOOTSTRAP, ArrayList(bootstrap))
                putExtra(EXTRA_WANT_TUN, wantTun)
                putExtra(EXTRA_AUTO_ACCEPT, autoAccept)
                putExtra(EXTRA_VERSION, version)
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                ctx.startForegroundService(i)
            } else {
                ctx.startService(i)
            }
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, LanetVpnService::class.java).apply { action = ACTION_STOP })
        }
    }

    private var tunInterface: ParcelFileDescriptor? = null
    private var worker: Thread? = null
    private var multicastLock: WifiManager.MulticastLock? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                teardown()
                return START_NOT_STICKY
            }
            ACTION_START -> {
                ensureChannel()
                startForeground(NOTIFICATION_ID, buildNotification("正在连接…"))
                val name = intent.getStringExtra(EXTRA_NAME) ?: "android"
                val key = intent.getStringExtra(EXTRA_NETWORK_KEY) ?: ""
                val bootstrap = intent.getStringArrayListExtra(EXTRA_BOOTSTRAP) ?: arrayListOf()
                val wantTun = intent.getBooleanExtra(EXTRA_WANT_TUN, true)
                val autoAccept = intent.getBooleanExtra(EXTRA_AUTO_ACCEPT, false)
                val version = intent.getStringExtra(EXTRA_VERSION) ?: ""
                startWorker(name, key, bootstrap, wantTun, autoAccept, version)
            }
        }
        // 被系统回收后自动重建：VPN 常驻场景下比 START_NOT_STICKY 更符合预期。
        return START_STICKY
    }

    private fun startWorker(
        name: String,
        key: String,
        bootstrap: List<String>,
        wantTun: Boolean,
        autoAccept: Boolean,
        version: String,
    ) {
        if (worker?.isAlive == true) {
            Log.i(TAG, "已有连接流程在进行，忽略重复启动请求")
            return
        }
        acquireMulticastLock()
        worker = Thread {
            try {
                val dir = dataDir()
                if (wantTun) {
                    startWithTun(dir, name, key, bootstrap, autoAccept, version)
                } else {
                    LanetCore.start(
                        dir, name, key, bootstrap,
                        tunFd = 0, autoAccept = autoAccept, version = version,
                    )
                    notify("已入网（应用层模式，虚拟 IP 不可 ping）")
                }
                lastError = null
                running = true
            } catch (t: Throwable) {
                Log.e(TAG, "连接失败", t)
                lastError = t.message ?: t.toString()
                notify("连接失败：$lastError")
                running = false
                virtualIp = null
            }
        }.also { it.start() }
    }

    /**
     * 两阶段建卡。
     *
     * 为什么不能一步到位：`Builder.addAddress` 必须在 `establish()` 之前确定
     * 虚拟 IP，而虚拟 IP 要入网后才由 SDK 派生（= f(群密钥, PeerID)）。于是：
     *
     *   阶段 1 以 tun_fd=0 入网，轮询拿到 virtual_ip；
     *   阶段 2 用该 IP 建卡（地址 + 10.7.0.0/16 路由 + MTU），拿到 fd；
     *   阶段 3 停止节点、带 fd 重启 —— 虚拟 IP 是确定性派生的，重启后不变，
     *          所以阶段 1 算出的地址与实际接管后的地址完全一致。
     *
     * 地址与路由必须在 Java 侧配：Android 上 SDK 的 ConfigureTUN 会落到它的
     * default 分支直接报 unsupported OS，SDK 侧已明确跳过（Config.TunFD > 0）。
     */
    private fun startWithTun(
        dir: String,
        name: String,
        key: String,
        bootstrap: List<String>,
        autoAccept: Boolean,
        version: String,
    ) {
        LanetCore.start(dir, name, key, bootstrap, tunFd = 0, autoAccept = autoAccept, version = version)
        notify("正在获取虚拟 IP…")
        val ip = waitForVirtualIp(30_000)
            ?: throw IllegalStateException("30 秒内未取到虚拟 IP（请核对网络密钥与引导种子）")
        Log.i(TAG, "阶段 1 完成，虚拟 IP = $ip")

        val builder = Builder()
            .setSession("lanet")
            .setMtu(MTU)
            .addAddress(ip, VNET_PREFIX)
            .addRoute(VNET_NETWORK, VNET_PREFIX)
            .setBlocking(false)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            // 虚拟网不该被系统当作计费网络来限制后台流量。
            builder.setMetered(false)
        }
        val pfd = builder.establish() ?: throw IllegalStateException("establish() 返回 null")
        tunInterface = pfd
        Log.i(TAG, "阶段 2 完成，TUN fd = ${pfd.fd}")

        LanetCore.stop()
        LanetCore.start(dir, name, key, bootstrap, tunFd = pfd.fd, autoAccept = autoAccept, version = version)
        virtualIp = ip
        Log.i(TAG, "阶段 3 完成，节点已接管虚拟网卡")
        notify("已连接：$ip")
    }

    /**
     * 节点数据目录：宿主 App 的私有目录下单独开一层。
     *
     * 身份（node.key）与地址簿（lanet.db）都落在这里，必须稳定 —— filesDir 在
     * 应用升级后保持不变，所以 PeerID 与虚拟 IP 能跨版本延续。
     */
    private fun dataDir(): String = File(filesDir, "lanet").apply { mkdirs() }.absolutePath

    private fun waitForVirtualIp(timeoutMs: Long): String? {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            val ip = try {
                LanetCore.status().optString("virtual_ip", "")
            } catch (t: Throwable) {
                Log.w(TAG, "读取状态失败", t)
                ""
            }
            if (ip.isNotEmpty()) return ip
            Thread.sleep(500)
        }
        return null
    }

    private fun teardown() {
        try {
            LanetCore.stop()
        } catch (t: Throwable) {
            Log.w(TAG, "停止节点失败", t)
        }
        try {
            tunInterface?.close()
        } catch (t: Throwable) {
            Log.w(TAG, "关闭 TUN 失败", t)
        }
        tunInterface = null
        releaseMulticastLock()
        running = false
        virtualIp = null
        worker = null
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            stopForeground(STOP_FOREGROUND_REMOVE)
        } else {
            @Suppress("DEPRECATION") stopForeground(true)
        }
        stopSelf()
    }

    override fun onDestroy() {
        teardown()
        super.onDestroy()
    }

    // ---- mDNS 组播锁 ----

    private fun acquireMulticastLock() {
        try {
            val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
            multicastLock = wm.createMulticastLock("lanet-mdns").apply {
                setReferenceCounted(false)
                acquire()
            }
        } catch (t: Throwable) {
            Log.w(TAG, "获取 MulticastLock 失败（局域网自动发现可能不可用）", t)
        }
    }

    private fun releaseMulticastLock() {
        try {
            multicastLock?.release()
        } catch (t: Throwable) {
            Log.w(TAG, "释放 MulticastLock 失败", t)
        }
        multicastLock = null
    }

    // ---- 前台通知 ----

    private fun ensureChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val mgr = getSystemService(NotificationManager::class.java)
        if (mgr.getNotificationChannel(CHANNEL_ID) == null) {
            mgr.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "Lanet 连接", NotificationManager.IMPORTANCE_LOW)
            )
        }
    }

    private fun buildNotification(text: String): Notification {
        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Notification.Builder(this, CHANNEL_ID)
        } else {
            @Suppress("DEPRECATION") Notification.Builder(this)
        }
        builder
            .setContentTitle("Lanet")
            .setContentText(text)
            .setSmallIcon(R.drawable.ic_lanet)
            .setOngoing(true)

        // 点通知回到宿主 App。插件是宿主的一部分，所以取宿主的启动 Intent；
        // 拿不到（例如被裁剪过的宿主）就退化成只有通知、点了没反应。
        val launch = packageManager.getLaunchIntentForPackage(packageName)
        if (launch != null) {
            val flags = PendingIntent.FLAG_UPDATE_CURRENT or
                (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) PendingIntent.FLAG_IMMUTABLE else 0)
            builder.setContentIntent(PendingIntent.getActivity(this, 0, launch, flags))
        }
        return builder.build()
    }

    private fun notify(text: String) {
        try {
            getSystemService(NotificationManager::class.java)
                .notify(NOTIFICATION_ID, buildNotification(text))
        } catch (t: Throwable) {
            Log.w(TAG, "更新通知失败", t)
        }
    }
}
