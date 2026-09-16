/**
 * lanet 原生插件的 JS 封装。
 *
 * 三个约定（与 Kotlin 侧 LanetVpnModule 对应）：
 *  1. 插件只在 **App 平台**可用，H5/小程序里 `uni.requireNativePlugin` 返回空 ——
 *     所有导出函数都做了降级，不会抛异常，调用方用 `available()` 判断即可。
 *  2. 同步查询方法返回 **JSON 文本**（Kotlin 侧一律走 String，避免依赖宿主的
 *     fastjson 序列化实现），这里统一 parse 成对象/数组。
 *  3. 异步方法统一回调 `{ok, stage, data, error}`，这里 Promise 化成
 *     resolve(payload) / reject(Error)。
 */

const PLUGIN_ID = 'lanet-vpn'

let cached
let resolved = false

/** 取原生模块句柄（只解析一次）。不可用时返回 null。 */
export function plugin() {
    if (!resolved) {
        resolved = true
        try {
            cached = typeof uni !== 'undefined' && uni.requireNativePlugin
                ? uni.requireNativePlugin(PLUGIN_ID)
                : null
        } catch (e) {
            console.warn('[lanet] requireNativePlugin 失败', e)
            cached = null
        }
        if (!cached) {
            console.warn(
                '[lanet] 原生插件不可用：需要 App 平台 + 已把 nativeplugins/lanet-vpn 打进包（云打包或自定义基座）'
            )
        }
    }
    return cached || null
}

/** 插件是否可用。UI 用它决定要不要置灰按钮。 */
export function available() {
    return !!plugin()
}

function parse(text, fallback) {
    if (!text) return fallback
    if (typeof text === 'object') return text
    try {
        return JSON.parse(text)
    } catch (e) {
        console.warn('[lanet] JSON 解析失败', e, text)
        return fallback
    }
}

// ---------------------------------------------------------------- 同步查询

/** 节点状态；未运行时是 {running:false}。就绪后含 peer_id / virtual_ip / firewall 等。 */
export function status() {
    const p = plugin()
    return p ? parse(p.status(), { running: false }) : { running: false }
}

export function isRunning() {
    const p = plugin()
    return p ? !!p.isRunning() : false
}

export function members() {
    const p = plugin()
    return p ? parse(p.members(), []) : []
}

/** 待审批的陌生节点。 */
export function pending() {
    const p = plugin()
    return p ? parse(p.pending(), []) : []
}

/** 已信任的地址簿条目。 */
export function peers() {
    const p = plugin()
    return p ? parse(p.peers(), []) : []
}

export function nearby() {
    const p = plugin()
    return p ? parse(p.nearby(), []) : []
}

/** 本机对外广播的地址列表。 */
export function seedAddrs() {
    const p = plugin()
    return p ? parse(p.seedAddrs(), []) : []
}

/** 本机连接码 `lanet://<PeerID>@<addr>,…`，发给别人即可让对端连进来。 */
export function inviteCode() {
    const p = plugin()
    return p ? p.inviteCode() || '' : ''
}

/** 是否已获得系统 VPN 授权。 */
export function isAuthorized() {
    const p = plugin()
    return p ? !!p.isAuthorized() : false
}

/** 最近一次失败原因；无错误时空串。 */
export function lastError() {
    const p = plugin()
    return p ? p.lastError() || '' : ''
}

export function version() {
    const p = plugin()
    return p ? p.version() || '' : ''
}

// ---------------------------------------------------------------- 异步操作

function callAsync(name, args) {
    return new Promise((resolve, reject) => {
        const p = plugin()
        if (!p) {
            reject(new Error('原生插件 lanet-vpn 不可用（需 App 平台且已云打包/自定义基座）'))
            return
        }
        try {
            p[name](...args, (res) => {
                if (res && res.ok) {
                    resolve(res)
                } else {
                    reject(new Error((res && res.error) || `${name} 失败`))
                }
            })
        } catch (e) {
            reject(e)
        }
    })
}

/**
 * 启动并入网。
 *
 * options：
 *   name         节点名（展示用）
 *   network_key  网络密钥，**必填**；留空会退化成按 PeerID 派生本机专属网
 *   bootstrap    种子地址数组，如 ['/ip4/1.2.3.4/tcp/4001/p2p/12D3Koo…']
 *   seed         单个种子（等价 bootstrap 一项）
 *   auto_accept  是否自动同意陌生节点（手机在 NAT 后建议 true）
 *   want_tun     默认 true；false = 纯应用层模式，虚拟 IP 不可 ping
 *   version      上报的版本号（选填）
 *
 * resolve 的 payload.stage 可能是：
 *   'awaiting_permission' —— 已弹出系统 VPN 授权框，同意后服务自动启动
 *   'starting'            —— 直接启动了（原先就授权过）
 * 两种情况都要继续轮询 status() 直到 running，或用 startAndWait()。
 */
export function start(options) {
    return callAsync('start', [JSON.stringify(options || {})])
}

export function stop() {
    return callAsync('stop', [])
}

/**
 * 主动连接一个节点。
 *
 * address 支持裸 PeerID / 连接码 `lanet://…` / multiaddr。
 *
 * 这是本机入网的关键动作：对端主动拨入**不会**在本机产生待审批，只有本机
 * 主动填对方地址才会「先信任对方」。所以 UI 必须提供这个入口。
 */
export function connect(address) {
    return callAsync('connect', [address])
}

/** 同意（或拒绝）一个待审批节点。 */
export function approve(peerId, ok = true) {
    return callAsync('approve', [JSON.stringify({ peer_id: peerId, approve: ok })])
}

/** 移除成员（撤信任 + 出地址簿 + 记墓碑）。 */
export function remove(peerId) {
    return callAsync('remove', [peerId])
}

// ---------------------------------------------------------------- 便捷封装

export function sleep(ms) {
    return new Promise((r) => setTimeout(r, ms))
}

/**
 * 启动并等到真正入网（拿到虚拟 IP）才 resolve。
 *
 * 之所以需要它：VPN 授权弹窗是异步的，插件在 `stage='awaiting_permission'` 时
 * 就已经返回了，真正的「连上」要等几秒到几十秒。轮询比跨 Activity 保存 JS 回调
 * 可靠得多。
 */
export async function startAndWait(options, { timeout = 60000, interval = 800 } = {}) {
    const res = await start(options)
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
        const s = status()
        if (s && s.running && s.virtual_ip) {
            return { ...res, status: s }
        }
        await sleep(interval)
    }
    const why = lastError()
    throw new Error('等待入网超时' + (why ? `：${why}` : '（状态里始终没有虚拟 IP，请核对网络密钥与种子地址）'))
}
