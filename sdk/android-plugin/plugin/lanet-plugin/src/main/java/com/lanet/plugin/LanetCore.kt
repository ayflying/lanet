package com.lanet.plugin

import android.util.Log
import com.lanet.mobile.Node
import org.json.JSONArray
import org.json.JSONObject

/**
 * 对 gomobile 产出 AAR 的薄封装（与 sdk/android 里那份同源）。
 *
 * 单独抽一层有两个理由：
 *  1. gomobile 生成的 Java 包名/类名/方法名随 bind 参数变化（`-javapkg` 会拼在
 *     Go 包名前面、构造函数要求 Go 侧叫 `NewNode`），集中到这里改动面最小；
 *  2. 「入网 → 取虚拟 IP → 建卡 → 带 fd 重启」这条流程的语义只写一处，
 *     避免 Service 和 Module 各自理解一遍。
 *
 * 生成物对应关系（Go → Java）：
 *   package mobile                → com.lanet.mobile
 *   type Node                     → com.lanet.mobile.Node
 *   func NewNode() *Node          → Node() 构造函数
 *   method Status() string        → status()
 */
internal object LanetCore {

    private const val TAG = "LanetPlugin"

    @Volatile
    private var node: Node? = null

    /**
     * 启动并入网。
     *
     * @param tunFd 宿主 VpnService 建立虚拟网卡后交出的 fd；0 表示纯应用层模式
     *              （仍可拨号与端口转发，但虚拟 IP 不能 ping）。
     */
    @Synchronized
    fun start(
        dataDir: String,
        name: String,
        networkKey: String,
        bootstrap: List<String>,
        tunFd: Int = 0,
        autoAccept: Boolean = false,
        firewallMode: String = FIREWALL_ALLOW_ALL,
        version: String = "",
    ): JSONObject {
        stop()
        val cfg = JSONObject().apply {
            put("data_dir", dataDir)
            put("name", name)
            put("network_key", networkKey)
            put("bootstrap", JSONArray(bootstrap))
            // 关键：不设置渠道（channel）。官方桌面节点 pvn-node 构造的同样是
            // lanet.Config，其空渠道会被 SDK 归一化成与移动端相同的取值，两端
            // 天然同网；一旦在这里显式改渠道，手机就被隔离到另一张网。
            if (tunFd > 0) put("tun_fd", tunFd)
            // 自动同意陌生节点的连接申请。手机在 NAT 后面时，对端无法反向拨入、
            // 也就无法在手机侧弹审批框，只能由手机侧主动放宽。生产环境应人工审批。
            if (autoAccept) put("auto_accept", true)
            // 入向防火墙。必须与官方 pvn-node 的默认（allow-all）一致，否则会出现
            // 「手机 ping 得通别人、别人 ping 不回手机」—— SDK 默认是 deny-all，
            // 会静默丢弃全部入向包。安全边界靠连接审批，不是靠这里。
            put("firewall_mode", firewallMode)
            if (version.isNotEmpty()) put("version", version)
        }
        Log.i(TAG, "启动节点：name=$name tunFd=$tunFd seeds=${bootstrap.size} 个")
        val n = Node()
        n.start(cfg.toString())
        node = n
        Log.i(TAG, "入网成功：${n.status()}")
        return status()
    }

    @Synchronized
    fun stop() {
        val n = node ?: return
        node = null
        try {
            n.stop()
            Log.i(TAG, "节点已停止")
        } catch (t: Throwable) {
            Log.w(TAG, "停止节点时出错", t)
        }
    }

    fun isRunning(): Boolean = node?.isRunning() ?: false

    /** 节点状态（JSON）。未运行时返回 {"running":false}。 */
    fun status(): JSONObject = try {
        JSONObject(node?.status() ?: """{"running":false}""")
    } catch (t: Throwable) {
        Log.w(TAG, "读取状态失败", t)
        JSONObject("""{"running":false}""")
    }

    fun members(): JSONArray = jsonArrayOrEmpty { node?.members() }

    fun pending(): JSONArray = jsonArrayOrEmpty { node?.pending() }

    fun peers(): JSONArray = jsonArrayOrEmpty { node?.peers() }

    fun nearby(): JSONArray = jsonArrayOrEmpty { node?.nearby() }

    fun seedAddrs(): JSONArray = jsonArrayOrEmpty { node?.seedAddrs() }

    fun inviteCode(): String = try {
        node?.inviteCode() ?: ""
    } catch (t: Throwable) {
        Log.w(TAG, "读取连接码失败", t)
        ""
    }

    /** 同意/拒绝一个待审批节点。 */
    fun approve(peerId: String, approve: Boolean) {
        val n = node ?: throw IllegalStateException("节点未运行")
        n.approve(peerId, approve)
    }

    fun remove(peerId: String) {
        node?.remove(peerId)
    }

    /**
     * 主动连接一个节点。address 支持裸 PeerID / 连接码（lanet://…）/ multiaddr。
     *
     * 这是手机入网的**关键动作**：实测对端主动拨入不会在本机产生待审批，
     * 只有本机主动填对方地址才会「先信任对方」，所以 UI 必须提供这个入口，
     * 被动等待永远等不到入网。
     */
    fun connect(address: String): JSONObject {
        val n = node ?: throw IllegalStateException("节点未运行")
        return JSONObject(n.connect(address.trim()))
    }

    private inline fun jsonArrayOrEmpty(block: () -> String?): JSONArray = try {
        JSONArray(block() ?: "[]")
    } catch (t: Throwable) {
        Log.w(TAG, "读取列表失败", t)
        JSONArray("[]")
    }

    /** 与官方 pvn-node 的默认一致：入向放行，安全边界交给连接审批。 */
    const val FIREWALL_ALLOW_ALL = "allow-all"
}
