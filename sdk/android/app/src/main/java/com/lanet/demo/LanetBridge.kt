package com.lanet.demo

import android.util.Log
import com.lanet.mobile.Node
import org.json.JSONArray
import org.json.JSONObject

/**
 * 对 gomobile 产出 AAR 的薄封装。
 *
 * 单独抽一层有两个理由：
 *  1. gomobile 生成的 Java 包名/类名/方法名随 bind 参数变化（例如
 *     `-javapkg` 会拼在 Go 包名前面、构造函数要求 Go 侧叫 `NewNode`），
 *     集中到这里改动面最小；
 *  2. 「入网 → 取虚拟 IP → 建卡 → 带 fd 重启」这条流程的语义要写在唯一一处，
 *     避免 Service 和 UI 各自理解一遍。
 *
 * 生成物对应关系（Go → Java）：
 *   package mobile                → com.lanet.mobile
 *   type Node                     → com.lanet.mobile.Node（Java 类）
 *   func NewNode() *Node          → Node() 构造函数
 *   method Status() string        → status()
 *   method Approve(string,bool)   → approve(String, boolean)（error → 抛异常）
 */
object LanetBridge {

    private const val TAG = "LanetBridge"

    @Volatile
    private var node: Node? = null

    /**
     * 启动并入网。
     *
     * @param tunFd 宿主 VpnService 建立虚拟网卡后交出的 fd；0 表示纯应用层模式
     *              （仍可用 dial/端口转发，但虚拟 IP 不能 ping）。
     */
    @Synchronized
    fun start(
        dataDir: String,
        name: String,
        networkKey: String,
        bootstrap: List<String>,
        tunFd: Int = 0,
        autoAccept: Boolean = false,
        firewallMode: String = "allow-all",
        version: String = BuildConfig.VERSION_NAME,
    ): JSONObject {
        stop()
        val cfg = JSONObject().apply {
            put("data_dir", dataDir)
            put("name", name)
            put("network_key", networkKey)
            put("bootstrap", JSONArray(bootstrap))
            // 关键：不设置渠道（channel）字段。官方桌面节点 pvn-node 构造的
            // 同样是 lanet.Config，其空渠道会被 SDK 归一化成与移动端相同的取值，
            // 因此两端天然同网；一旦在这里显式改渠道，手机就会被隔离到另一张网。
            if (tunFd > 0) put("tun_fd", tunFd)
            // 自动同意陌生节点的连接申请。生产环境应保持人工审批；这个开关主要
            // 用于受控验证——以及手机处在 NAT 后面（对端无法反向拨入、也就无法
            // 在手机侧弹审批框）时，由手机侧主动放宽。
            if (autoAccept) put("auto_accept", true)
            // 入向防火墙模式。必须与官方 pvn-node 的默认（allow-all）保持一致，
            // 否则手机会「ping 得通别人、别人 ping 不回手机」——SDK 自身的默认
            // 是 deny-all，会静默丢弃全部入向包。安全边界靠连接审批，不是靠这里。
            put("firewall_mode", firewallMode)
            put("version", version)
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

    fun members(): JSONArray = try {
        JSONArray(node?.members() ?: "[]")
    } catch (t: Throwable) {
        JSONArray("[]")
    }

    fun pending(): JSONArray = try {
        JSONArray(node?.pending() ?: "[]")
    } catch (t: Throwable) {
        JSONArray("[]")
    }

    fun peers(): JSONArray = try {
        JSONArray(node?.peers() ?: "[]")
    } catch (t: Throwable) {
        JSONArray("[]")
    }

    fun nearby(): JSONArray = try {
        JSONArray(node?.nearby() ?: "[]")
    } catch (t: Throwable) {
        JSONArray("[]")
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
     * 这一步是手机入网的**关键动作**：实测表明对端主动拨入不会在本机产生待审批，
     * 只有本机主动填对方地址才会「先信任对方」，所以 UI 必须提供这个入口，
     * 被动等待永远等不到入网。
     */
    fun connect(address: String): JSONObject {
        val n = node ?: throw IllegalStateException("节点未运行")
        return JSONObject(n.connect(address.trim()))
    }

    fun inviteCode(): String = node?.inviteCode() ?: ""

    fun seedAddrs(): JSONArray = try {
        JSONArray(node?.seedAddrs() ?: "[]")
    } catch (t: Throwable) {
        JSONArray("[]")
    }
}
