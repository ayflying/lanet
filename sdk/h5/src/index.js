/**
 * Lanet H5 SDK —— 给 H5 页面的开箱即用直引包。
 *
 * 定位差异：
 *   - `@lanet/sdk-web` 是 ESM 源码包，浏览器要用它得配打包器，或者写 importmap
 *     把 libp2p 全家桶指到 CDN；
 *   - 本包把 Web SDK 与整条 js-libp2p 依赖链**打成单文件**，H5 页面一句
 *     `<script src="lanet-h5.min.js"></script>` 就拿到全局 `Lanet`，
 *     免 npm、免打包器、免 importmap。
 *
 * 分工（不重造轮子）：
 *   - 底层 P2P 全部复用 `@lanet/sdk-web`：建 js-libp2p 节点、凭邀请码入网、
 *     按虚拟 IP 开流、直连失败走 relay 电路兜底；
 *   - 本层只补 H5 高频需要的东西：**请求-响应一行式**、**流分帧**、
 *     超时、页面卸载自动下线。
 *
 * 浏览器沙箱限制与 Web SDK 相同：无 TUN / 原始 IP 包能力，一切访问按「流」进行，
 * 因此**不能 ping 虚拟 IP**，只能开流访问对端注册了处理器的服务（或经 PortFWD 桥接的 TCP 服务）。
 */

import { createNode, PROTOCOL_TUNNEL } from '@lanet/sdk-web'

export { PROTOCOL_TUNNEL }

/** H5 层版本号，随产物一起打包，便于线上排查「页面用的哪版」。 */
export const VERSION = '0.1.0'

/** 请求-响应默认超时（毫秒）。0 表示不超时（不推荐：对端不回写会永久挂住）。 */
const DEFAULT_REQUEST_TIMEOUT = 15000

/** 分帧解码的单帧上限，防止对端声称一个超大长度把内存撑爆。 */
const DEFAULT_MAX_FRAME = 16 * 1024 * 1024

/**
 * request 系列默认重试次数与重试间隔（毫秒）。
 *
 * 为什么需要重试：入网后，**同群其他成员要等下一次 NetMap 刷新（Go SDK 默认 15s 一轮）
 * 才能解析出你的虚拟 IP**；在那之前你发去的入向流会因「来源未知」被对端防火墙复位。
 * 所以「刚入网就发请求」的首连失败是正常时序现象，重试一两次即可通过。
 * 默认 0（不重试，保持语义直白）；长连场景建议在 connect/request 上传 2~3。
 */
const DEFAULT_REQUEST_RETRIES = 0
const DEFAULT_RETRY_DELAY = 2000
/** 超时是否也重试（默认 false：超时可能意味着对端已处理完，重发会让副作用重复）。 */
const DEFAULT_RETRY_ON_TIMEOUT = false

/** 「流被对端复位」的公共排查提示 —— 首连失败九成是 NetMap 时序，不是链路坏了。 */
const RESET_HINT = '——对端复位了流。按可能性排查：①（最常见）你刚入网，同群成员尚未刷新 NetMap'
  + '（Go SDK 默认 15s 一轮），还解析不出你的虚拟 IP，入向流被防火墙拦下 —— 稍等几秒重试，'
  + '或传 retries 选项自动重试；② 对端没有注册 /pvn/tunnel/1.0.0 的处理器；'
  + '③ 被对端的入向防火墙规则拒绝。'

/** 「拨号失败」的公共排查提示 —— 入网初期对端 NetMap 未刷新时也会长这样。 */
const DIAL_HINT = '——拨号阶段就没连上。按可能性排查：①（最常见）你刚入网，本地/对端 NetMap 尚未刷新'
  + '（Go SDK 默认 15s 一轮），解析不出目标地址 —— 稍等几秒重试，或传 retries 选项自动重试；'
  + '② 对端已离线；③ 双方都需要经中继时才可达，而中继不可用（日志会看到 relay dial failed）。'

/**
 * 打开流事件日志（排查「请求超时 / 响应为空」时用）：
 * 浏览器控制台执行 `localStorage.lanetDebug=1` 后刷新，或 Node 里 `LANET_H5_DEBUG=1 node ...`。
 */
const DEBUG = (typeof process !== 'undefined' && !!process.env?.LANET_H5_DEBUG) ||
  (typeof localStorage !== 'undefined' && !!localStorage.getItem('lanetDebug'))

/**
 * 连接并入网 —— `@lanet/sdk-web` 的 `createNode` 的 H5 版，多一层便利封装。
 *
 * @param {object} options
 * @param {string}   options.ctlURL        控制面地址（必填），如 https://ctl.example.com
 * @param {string}   options.inviteCode    邀请码（必填；网页节点不支持建群）
 * @param {string}   [options.name]        节点名称，默认 h5-<PeerID 后 6 位>
 * @param {string[]} [options.relayAddrs]  显式指定 relay multiaddr（默认从控制面自动发现）
 * @param {Function} [options.onStream]    入向流回调（等价于入网后立刻 onStream）
 * @param {boolean}  [options.autoClose]   页面卸载（pagehide）时自动下线，默认 true
 * @param {number}   [options.requestTimeout] request 系列默认超时，默认 15000ms
 * @param {number}   [options.retries]      request 系列默认重试次数，默认 0（只重试「确定未被处理」的失败）
 * @param {number}   [options.retryDelay]   request 系列默认重试间隔，默认 2000ms
 * @param {boolean}  [options.retryOnTimeout] 超时是否也重试，默认 false（仅对幂等请求建议开启）
 * @param {boolean}  [options.allowPrivateAddresses]  允许拨内网地址，默认 false（透传 Web SDK）
 * @param {boolean}  [options.allowInsecureWebSockets] 允许拨明文 ws://，默认 false（透传 Web SDK）
 * @param {object}   [options.connectionGater]        自定义拨号闸门，默认继承 Web SDK 策略
 * @returns {Promise<H5Node>}
 */
export async function connect (options = {}) {
  const {
    onStream,
    autoClose = true,
    requestTimeout = DEFAULT_REQUEST_TIMEOUT,
    retries = DEFAULT_REQUEST_RETRIES,
    retryDelay = DEFAULT_RETRY_DELAY,
    retryOnTimeout = DEFAULT_RETRY_ON_TIMEOUT,
    ...webOptions
  } = options

  if (!webOptions.ctlURL) throw new Error('lanet-h5: ctlURL 必填（控制面地址）')
  if (!webOptions.inviteCode) throw new Error('lanet-h5: inviteCode 必填（网页节点不支持建群，请用 Go SDK/CLI 建群）')

  const webNode = await createNode({ os: 'h5', ...webOptions })
  const node = new H5Node(webNode, { autoClose, requestTimeout, retries, retryDelay, retryOnTimeout })
  if (typeof onStream === 'function') node.onStream(onStream)
  return node
}

/**
 * H5 节点：`@lanet/sdk-web` 节点的便利视图。
 *
 * 透传 dial / resolve / netmap / onStream（语义与 Web SDK 完全一致），
 * 额外提供 request 系列（请求-响应一行式）与自动下线。
 */
export class H5Node {
  constructor (webNode, options = {}) {
    this._web = webNode
    this._requestTimeout = options.requestTimeout ?? DEFAULT_REQUEST_TIMEOUT
    this._retries = options.retries ?? DEFAULT_REQUEST_RETRIES
    this._retryDelay = options.retryDelay ?? DEFAULT_RETRY_DELAY
    this._retryOnTimeout = options.retryOnTimeout ?? DEFAULT_RETRY_ON_TIMEOUT
    this._autoClose = options.autoClose !== false
    this._closed = false
    this._onPageHide = null

    /** 本节点 PeerID。 */
    this.peerId = webNode.peerId
    /** 入网分配的虚拟 IP（10.7.0.0/16 段内）。 */
    this.virtualIP = webNode.virtualIP
    /** 群组信息（控制面 join 响应原文）。 */
    this.group = webNode.group

    if (this._autoClose) this._bindAutoClose()
  }

  // ------------------------------------------------------------------
  // 与 Web SDK 同形的透传
  // ------------------------------------------------------------------

  /** 底层 `@lanet/sdk-web` 节点（进阶用法：getConnections / getMultiaddrs 等）。 */
  get web () { return this._web }

  /** 注册入向流回调（可多个，按序调用）。 */
  onStream (handler) {
    this._web.onStream(handler)
    return this
  }

  /** 按虚拟 IP 开流（低层用法；只要请求-响应请用 request）。 */
  dial (virtualIP, options) {
    return this._web.dial(virtualIP, options)
  }

  /** 虚拟 IP → NetMap 成员（含 peer_id / addrs）。 */
  resolve (virtualIP) {
    return this._web.resolve(virtualIP)
  }

  /** 拉取群组 NetMap。 */
  netmap () {
    return this._web.netmap()
  }

  // ------------------------------------------------------------------
  // H5 便利层
  // ------------------------------------------------------------------

  /**
   * 请求-响应：发完即半关闭写端，等对端回写完毕（读到 EOF）返回全部响应字节。
   *
   * 适用于「一问一答、响应以 EOF 结束」的服务（最常见，Go 侧 `io.Copy` echo 即此形态）。
   * 若对端在同一流上回多条消息、不关写端，请改用 dial + FrameDecoder。
   *
   * @param {string} virtualIP 目标虚拟 IP
   * @param {string|Uint8Array|ArrayBuffer} payload 请求体
   * @param {{timeout?: number, retries?: number, retryDelay?: number, retryOnTimeout?: boolean, onStream?: (stream: object) => void}} [options]
   *        onStream 在流建立后、发送前回调，用于诊断（读 stream.viaRelay / protocol）
   * @returns {Promise<Uint8Array>} 响应字节
   */
  async request (virtualIP, payload, options = {}) {
    const retries = options.retries ?? this._retries
    const retryDelay = options.retryDelay ?? this._retryDelay
    const retryOnTimeout = options.retryOnTimeout ?? this._retryOnTimeout
    for (let attempt = 0; ; attempt++) {
      try {
        return await this._requestOnce(virtualIP, payload, options)
      } catch (err) {
        // 「写不进去 / 流被对端复位」= 请求确定没被处理，重发一定安全；
        // 超时默认不重试（对端可能已经处理完，重发会让副作用重复），幂等请求可开 retryOnTimeout。
        const retryable = err.retryable || (retryOnTimeout && err.timedOut)
        if (attempt >= retries || !retryable) throw err
        if (DEBUG) {
          console.debug(`[lanet-h5] ${virtualIP} 第 ${attempt + 1} 次尝试失败，${retryDelay}ms 后重试：${err.message}`)
        }
        // 重试前先丢掉与目标的连接：底层 dial 会优先复用已有连接，而对端连续 Reset
        // 之后那条连接可能已经半死 —— 不丢掉它就只是在死连接上反复超时。
        await this._dropPeerConnection(virtualIP, attempt + 1)
        await delay(retryDelay)
      }
    }
  }

  /**
   * 断开与目标虚拟 IP 对应节点的 p2p 连接（best-effort，失败只记日志）。
   */
  async _dropPeerConnection (virtualIP, attempt) {
    try {
      const member = await this.resolve(virtualIP)
      const peerId = member?.peer_id
      // 这里是「降级可用」的私有访问：拿不到底层 libp2p 节点就跳过，不影响重试本身。
      const lp = this._web?._node
      if (!peerId || typeof lp?.hangUp !== 'function') return
      await lp.hangUp(peerId)
      if (DEBUG) {
        console.debug(`[lanet-h5] 已断开与 ${virtualIP}（…${peerId.slice(-6)}）的连接，随后进行第 ${attempt + 1} 次重试`)
      }
    } catch (err) {
      if (DEBUG) console.debug(`[lanet-h5] 断开 ${virtualIP} 连接失败（忽略）：${err.message}`)
    }
  }

  /**
   * request 的单次实现（不做重试）。失败时给错误对象打上 `retryable` 标记。
   * @private
   */
  async _requestOnce (virtualIP, payload, options = {}) {
    const timeout = options.timeout ?? this._requestTimeout
    // dial 失败同样属于「请求确定没被对端处理」，标记可重试：入网初期对端 NetMap 还没刷新时，
    // 解析不出目标地址 / 地址不可达导致的拨号失败，稍后重试即可穿透。
    let stream
    try {
      stream = await this.dial(virtualIP)
    } catch (err) {
      const e = new Error(`lanet-h5: 无法连接 ${virtualIP}（拨号失败）：${err.message}` + DIAL_HINT)
      e.retryable = true
      throw e
    }
    const chunks = []
    try {
      // 诊断口子：调用方据 stream.viaRelay 判断这次是直连还是走的中继。
      if (typeof options.onStream === 'function') options.onStream(stream)
      if (DEBUG) traceStreamEvents(stream, virtualIP)
      const finished = waitForRemoteEnd(stream, timeout, virtualIP)
      stream.onMessage(chunk => chunks.push(chunk))
      try {
        await sendAll(stream, payload)
      } catch (err) {
        // 对端在协商阶段就复位了流时，send 会直接抛 StreamStateError
        const e = new Error(`lanet-h5: 向 ${virtualIP} 写入失败：${err.message}` + RESET_HINT)
        e.retryable = true
        throw e
      }
      await stream.closeWrite() // 我方发送完毕，对端读到 EOF 才会开始回写
      await finished
      return concat(chunks)
    } finally {
      try { stream.abort() } catch { /* 已关闭则忽略 */ }
    }
  }

  /**
   * 请求-响应（文本）：发送字符串，按 UTF-8 解码响应。
   * @returns {Promise<string>}
   */
  async requestText (virtualIP, text, options = {}) {
    const bytes = await this.request(virtualIP, text, options)
    return new TextDecoder().decode(bytes)
  }

  /**
   * 请求-响应（JSON）：请求体 JSON 序列化发送，响应按 JSON 解析。
   * 响应为空时返回 null（对端只做动作、无回包）。
   * @returns {Promise<unknown>}
   */
  async requestJSON (virtualIP, payload, options = {}) {
    const bytes = await this.request(virtualIP, JSON.stringify(payload), options)
    const text = new TextDecoder().decode(bytes).trim()
    if (!text) return null
    return JSON.parse(text)
  }

  /**
   * 单向发送：发完半关闭写端，不等响应（适用于通知类、写入类操作）。
   */
  async sendText (virtualIP, text, options = {}) {
    const timeout = options.timeout ?? this._requestTimeout
    const stream = await this.dial(virtualIP)
    try {
      await sendAll(stream, text)
      await stream.closeWrite()
      // 给对端一点时间把数据读走再复位，避免刚写就 abort 导致丢包。
      if (timeout > 0) await delay(Math.min(200, timeout))
    } finally {
      try { stream.abort() } catch { /* 忽略 */ }
    }
  }

  /** 关闭节点并下线（幂等；页面卸载时会自动调用）。 */
  async close () {
    if (this._closed) return
    this._closed = true
    this._unbindAutoClose()
    await this._web.close()
  }

  // ------------------------------------------------------------------
  // 内部
  // ------------------------------------------------------------------

  _bindAutoClose () {
    if (typeof globalThis.addEventListener !== 'function') return
    this._onPageHide = () => { this.close().catch(() => {}) }
    // pagehide 比 beforeunload 可靠：移动端 Safari / 后台切换都会触发。
    globalThis.addEventListener('pagehide', this._onPageHide)
  }

  _unbindAutoClose () {
    if (this._onPageHide && typeof globalThis.removeEventListener === 'function') {
      globalThis.removeEventListener('pagehide', this._onPageHide)
    }
    this._onPageHide = null
  }
}

/**
 * 4 字节大端长度前缀分帧编码。
 *
 * 为什么需要：libp2p 流是**字节流，没有消息边界**，一次 send 的 1 条消息可能被对面
 * 拆成多次 message 事件、也可能多条粘在一起。约定一个长度前缀就能稳定地按「条」收发。
 *
 * @param {string|Uint8Array|ArrayBuffer} payload
 * @returns {Uint8Array}
 */
export function encodeFrame (payload) {
  const body = toBytes(payload)
  const out = new Uint8Array(4 + body.length)
  new DataView(out.buffer, out.byteOffset, 4).setUint32(0, body.length, false)
  out.set(body, 4)
  return out
}

/**
 * 增量分帧解码器：喂入任意切分的字节块，每凑齐一帧回调一次。
 *
 * ```js
 * const decoder = new FrameDecoder(frame => console.log('第', ++n, '条', frame))
 * stream.onMessage(chunk => decoder.push(chunk))
 * ```
 */
export class FrameDecoder {
  /**
   * @param {(frame: Uint8Array) => void} onFrame 每凑齐一帧的回调
   * @param {{maxFrameSize?: number}} [options] 单帧上限，默认 16MB
   */
  constructor (onFrame, options = {}) {
    if (typeof onFrame !== 'function') throw new Error('lanet-h5: FrameDecoder 需要 onFrame 回调')
    this._onFrame = onFrame
    this._max = options.maxFrameSize ?? DEFAULT_MAX_FRAME
    this._buf = new Uint8Array(0)
  }

  /** 喂入一块数据（可任意切分）。 */
  push (chunk) {
    const data = toBytes(chunk)
    if (!data.length) return this

    const merged = new Uint8Array(this._buf.length + data.length)
    merged.set(this._buf, 0)
    merged.set(data, this._buf.length)

    let offset = 0
    while (merged.length - offset >= 4) {
      const len = new DataView(merged.buffer, merged.byteOffset + offset, 4).getUint32(0, false)
      if (len > this._max) {
        throw new Error(`lanet-h5: 帧长度 ${len} 超过上限 ${this._max}（协议不匹配或对端异常）`)
      }
      if (merged.length - offset - 4 < len) break // 半帧，等下一块
      const frame = merged.subarray(offset + 4, offset + 4 + len)
      offset += 4 + len
      this._onFrame(frame)
    }
    // slice 而非 subarray：避免长期持有这一整块的底层 ArrayBuffer。
    this._buf = merged.slice(offset)
    return this
  }

  /** 清空未凑齐的残留（重连 / 复位时用）。 */
  reset () {
    this._buf = new Uint8Array(0)
  }

  /** 当前残留（半帧）字节数。 */
  get pending () {
    return this._buf.length
  }
}

// --------------------------------------------------------------------
// 内部工具
// --------------------------------------------------------------------

function toBytes (payload) {
  if (payload instanceof Uint8Array) return payload
  if (typeof payload === 'string') return new TextEncoder().encode(payload)
  if (payload instanceof ArrayBuffer) return new Uint8Array(payload)
  if (ArrayBuffer.isView(payload)) {
    return new Uint8Array(payload.buffer, payload.byteOffset, payload.byteLength)
  }
  throw new Error('lanet-h5: 仅支持 string / Uint8Array / ArrayBuffer / TypedArray')
}

function concat (chunks) {
  const total = chunks.reduce((n, c) => n + c.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const c of chunks) {
    out.set(c, offset)
    offset += c.length
  }
  return out
}

/** 发送并处理背压：send 返回 false 表示缓冲已满，需等 drain 再继续。 */
async function sendAll (stream, payload) {
  const data = toBytes(payload)
  if (!data.length) return
  if (stream.send(data) === false) await waitDrain(stream.raw)
}

function waitDrain (raw) {
  return new Promise(resolve => {
    const done = () => {
      raw.removeEventListener('drain', done)
      raw.removeEventListener('close', done)
      raw.removeEventListener('remoteCloseWrite', done)
      resolve()
    }
    raw.addEventListener('drain', done)
    // 兜底：流若在 drain 之前就结束了，别让 Promise 永久悬挂。
    raw.addEventListener('close', done)
    raw.addEventListener('remoteCloseWrite', done)
  })
}

/**
 * 等对端把响应写完后收尾 —— 只认 `remoteCloseWrite`。
 *
 * 事件名的坑（实测踩过两次，写在这里免得再踩）：
 *   - libp2p 的事件叫 **`remoteCloseWrite`**（对端半关闭写端，即 Go 侧 `CloseWrite()`，
 *     对应 TCP 的 FIN），**不叫 `remoteClose`** —— 写错名字就永远收不到，只能等超时；
 *   - **不能认 `close`**：本端 `closeWrite()` 之后、以及对端 reset 之后，`close` 都会来，
 *     前者会让我们在对端回写之前就收尾（实测 59ms 返回空响应、真回包被丢弃），
 *     后者则会把失败伪装成成功。
 *
 * 异常路径：`reset`（对端复位流）与 `error` 直接失败；超时兜底。
 */
function waitForRemoteEnd (stream, timeout, virtualIP) {
  return new Promise((resolve, reject) => {
    const raw = stream.raw
    let timer = null

    const cleanup = () => {
      raw.removeEventListener('remoteCloseWrite', onEnd)
      raw.removeEventListener('reset', onError)
      raw.removeEventListener('error', onError)
      if (timer) clearTimeout(timer)
    }
    const onEnd = () => { cleanup(); resolve() }
    const onError = evt => {
      cleanup()
      const reason = evt?.error?.message ?? evt?.detail?.message ?? '流被对端复位'
      const e = new Error(`lanet-h5: 请求 ${virtualIP} 失败：${reason}` + RESET_HINT)
      e.retryable = true
      reject(e)
    }

    raw.addEventListener('remoteCloseWrite', onEnd)
    raw.addEventListener('reset', onError)
    raw.addEventListener('error', onError)

    if (timeout > 0) {
      timer = setTimeout(() => {
        cleanup()
        const e = new Error(`lanet-h5: 请求 ${virtualIP} 超时（${timeout}ms）——对端未半关闭写端（CloseWrite）或不可达。`
          + '若对端只回一条消息且不关写端，请改用 dial + FrameDecoder。')
        e.retryable = false
        e.timedOut = true
        reject(e)
      }, timeout)
    }
  })
}

/** 打印流事件序列（LANET_H5_DEBUG 打开时），用于定位「响应为空 / 超时」。 */
function traceStreamEvents (stream, virtualIP) {
  const raw = stream.raw
  for (const evt of ['message', 'drain', 'close', 'remoteCloseWrite', 'remoteCloseRead', 'reset', 'error']) {
    raw.addEventListener(evt, () => {
      console.debug(`[lanet-h5] ${virtualIP} 流事件: ${evt}`)
    })
  }
}

function delay (ms) {
  return new Promise(resolve => setTimeout(resolve, ms))
}

/** 默认导出：IIFE 产物下即全局 `window.Lanet`。 */
const Lanet = {
  connect,
  H5Node,
  FrameDecoder,
  encodeFrame,
  PROTOCOL_TUNNEL,
  VERSION,
  version: VERSION
}

export default Lanet
