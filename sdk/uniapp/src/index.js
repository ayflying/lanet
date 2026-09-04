/**
 * @lanet/sdk-uniapp —— Lanet ws-gateway 客户端。
 *
 * 适用于 uniapp / 微信小程序 / H5 / Node（任何有 WebSocket 的环境）。
 * 服务端为 ws-gateway（Go，app/gateway）：网关以 SDK 节点身份入群，
 * 本 SDK 通过帧协议（frame.js）与其交换 auth / dial / data / close。
 *
 * 平台适配：仅需一个 socket 工厂，返回统一形态：
 *   { send(Uint8Array), close(), onMessage(cb(Uint8Array)), onClose(cb()), onError(cb()) }
 * 默认提供 uni / 微信小程序 / H5 / Node 适配。
 */
import { encodeFrame, decodeFrame, decodeJSON, Type, Mode } from './frame.js'

/**
 * 默认 socket 工厂：按运行环境自动选择。
 * @param {string} url
 * @param {{ socketFactory?: Function }} options
 */
function defaultSocketFactory (url) {
  // uniapp / 微信小程序
  if (typeof uni !== 'undefined' && typeof uni.connectSocket === 'function') {
    return uniSocket(url)
  }
  if (typeof wx !== 'undefined' && typeof wx.connectSocket === 'function') {
    return uniSocket(url)
  }
  // 浏览器 / H5
  if (typeof WebSocket !== 'undefined') {
    return browserSocket(url)
  }
  throw new Error('未找到可用的 WebSocket 实现，请通过 options.socketFactory 注入')
}

/** uni.connectSocket / wx.connectSocket 适配。 */
function uniSocket (url) {
  const connect = typeof uni !== 'undefined' ? uni : wx
  const task = connect.connectSocket({ url, protocols: ['binary'] })
  const listeners = { message: [], close: [], error: [] }
  let opened = false
  const queue = []
  task.onOpen(() => {
    opened = true
    queue.splice(0).forEach(bytes => task.send({ data: bytes }))
  })
  task.onMessage(res => {
    let data = res.data
    // 小程序二进制帧为 ArrayBuffer；字符串按 UTF-8 兜底转字节。
    if (typeof data === 'string') data = new TextEncoder().encode(data).buffer
    listeners.message.forEach(cb => cb(data))
  })
  task.onClose(() => listeners.close.forEach(cb => cb()))
  task.onError(err => listeners.error.forEach(cb => cb(err)))
  return {
    send: bytes => {
      const ab = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)
      if (opened) task.send({ data: ab }); else queue.push(ab)
    },
    close: () => task.close({}),
    onMessage: cb => listeners.message.push(cb),
    onClose: cb => listeners.close.push(cb),
    onError: cb => listeners.error.push(cb)
  }
}

/** 浏览器 WebSocket 适配。 */
function browserSocket (url) {
  const ws = new WebSocket(url)
  ws.binaryType = 'arraybuffer'
  const listeners = { message: [], close: [], error: [] }
  let opened = false
  const queue = []
  ws.onopen = () => {
    opened = true
    queue.splice(0).forEach(bytes => ws.send(bytes))
  }
  ws.onmessage = e => listeners.message.forEach(cb => cb(e.data))
  ws.onclose = () => listeners.close.forEach(cb => cb())
  ws.onerror = e => listeners.error.forEach(cb => cb(e))
  return {
    send: bytes => { if (opened) ws.send(bytes); else queue.push(bytes) },
    close: () => ws.close(),
    onMessage: cb => listeners.message.push(cb),
    onClose: cb => listeners.close.push(cb),
    onError: cb => listeners.error.push(cb)
  }
}

/**
 * 连接网关。
 * @param {object} options
 * @param {string} options.url ws-gateway 地址，如 ws://host:8700/gateway
 * @param {string} options.inviteCode 群组邀请码（网关启动日志/控制面可查）
 * @param {string} [options.name] 客户端名称
 * @param {string} [options.mode] 'client'（默认，主动开流）| 'service'（接收入向流）
 * @param {Function} [options.socketFactory] 自定义 socket 工厂 (url) => socketLike
 * @returns {Promise<GatewayClient>}
 */
export async function createGatewayClient (options) {
  if (!options || !options.url || !options.inviteCode) {
    throw new Error('createGatewayClient: url 与 inviteCode 为必填')
  }
  const mode = options.mode || Mode.CLIENT
  const socket = (options.socketFactory || defaultSocketFactory)(options.url)
  return new Promise((resolve, reject) => {
    const client = new GatewayClient(socket, options, mode, resolve, reject)
    client._handshake()
  })
}

let nextDialID = 0

/** GatewayClient 已鉴权的网关客户端。 */
export class GatewayClient {
  /**
   * @param {object} socket 平台适配后的 socket
   * @param {object} options createGatewayClient 选项
   * @param {string} mode 连接模式
   * @param {Function} resolve Promise resolve（鉴权成功）
   * @param {Function} reject Promise reject（鉴权失败）
   */
  constructor (socket, options, mode, resolve, reject) {
    this._socket = socket
    this._options = options
    this._mode = mode
    this._resolve = resolve
    this._reject = reject
    this._authenticated = false
    this._info = null
    this._streams = new Map() // streamId -> GatewayStream
    this._inboundHandlers = []
    this._closed = false

    socket.onMessage(data => {
      let frame
      try {
        frame = decodeFrame(data)
      } catch {
        return
      }
      this._handleFrame(frame)
    })
    socket.onClose(() => this._onSocketClosed())
    socket.onError(err => {
      if (!this._authenticated) this._reject(new Error('WebSocket 连接失败'))
      else if (this._errorHandler) this._errorHandler(err)
    })
  }

  _handshake () {
    const auth = encodeFrame(Type.AUTH, 0, JSON.stringify({
      invite_code: this._options.inviteCode,
      name: this._options.name || 'uniapp-client',
      mode: this._mode
    }))
    this._socket.send(auth)
  }

  _handleFrame (frame) {
    switch (frame.type) {
      case Type.AUTH_OK: {
        const info = decodeJSON(frame) || {}
        this._authenticated = true
        this._info = {
          virtualIP: info.virtual_ip || '',
          peerID: info.peer_id || '',
          group: info.group || '',
          mode: info.mode || this._mode
        }
        this._resolve(this)
        break
      }
      case Type.AUTH_ERR: {
        const err = decodeJSON(frame) || {}
        this._reject(new Error(`网关鉴权失败: ${err.error || '未知错误'}`))
        this.close()
        break
      }
      case Type.DIAL_OK: {
        const st = this._streams.get(frame.streamId)
        if (st) st._markOpen(decodeJSON(frame) || {})
        break
      }
      case Type.DIAL_ERR: {
        const st = this._streams.get(frame.streamId)
        const message = new TextDecoder().decode(frame.payload)
        if (st) st._markError(new Error(message))
        else this._emitError(new Error(`dial 失败: ${message}`))
        break
      }
      case Type.DATA: {
        const st = this._streams.get(frame.streamId)
        if (st) st._emitData(frame.payload)
        break
      }
      case Type.CLOSE: {
        const st = this._streams.get(frame.streamId)
        if (st) st._emitEOF()
        break
      }
      case Type.RESET: {
        const st = this._streams.get(frame.streamId)
        if (st) st._markError(new Error('流被对端重置'))
        break
      }
      case Type.STREAM_OPEN: {
        const info = decodeJSON(frame) || {}
        const st = new GatewayStream(this, frame.streamId, true)
        st.protocol = info.protocol || ''
        st.remotePeer = info.remote_peer || ''
        this._streams.set(frame.streamId, st)
        this._inboundHandlers.forEach(cb => {
          try { cb(st) } catch (e) { console.error('[lanet] onStream 回调异常', e) }
        })
        break
      }
      case Type.PONG:
        break
      default:
        break
    }
  }

  _onSocketClosed () {
    this._closed = true
    this._streams.forEach(st => st._markError(new Error('连接已关闭')))
    this._streams.clear()
    if (!this._authenticated && this._reject) {
      this._reject(new Error('网关连接在鉴权前关闭'))
    }
    if (this._closeHandler) this._closeHandler()
  }

  _emitError (err) {
    if (this._errorHandler) this._errorHandler(err)
    else console.error('[lanet]', err)
  }

  /** 入网信息 {virtualIP, peerID, group, mode}。 */
  info () { return { ...this._info } }

  /** 注册网格入向流处理器（service 模式）。 */
  onStream (cb) { this._inboundHandlers.push(cb) }

  /** 注册连接级错误回调。 */
  onError (cb) { this._errorHandler = cb }

  /** 注册连接关闭回调。 */
  onClose (cb) { this._closeHandler = cb }

  /**
   * 访问网格内 TCP 服务（经网关 PortFWD 转发到目标节点）。
   * @param {string} virtualIP 目标节点虚拟 IP
   * @param {number} port 目标节点侧 TCP 端口（目标节点为 SDK 节点时指其本机端口）
   * @returns {Promise<GatewayStream>}
   */
  dial (virtualIP, port) {
    return this._dial({ ip: virtualIP, port })
  }

  /**
   * 打开自定义协议流（对端节点需注册了该协议处理器）。
   * @param {string} virtualIP 目标节点虚拟 IP
   * @param {string} protocol 协议 ID，如 /pvn/tunnel/1.0.0
   * @returns {Promise<GatewayStream>}
   */
  dialProtocol (virtualIP, protocol) {
    return this._dial({ ip: virtualIP, protocol })
  }

  _dial (payload) {
    if (!this._authenticated) return Promise.reject(new Error('尚未鉴权'))
    if (this._closed) return Promise.reject(new Error('连接已关闭'))
    const id = ++nextDialID
    return new Promise((resolve, reject) => {
      const st = new GatewayStream(this, id, false)
      st._pending = { resolve, reject }
      this._streams.set(id, st)
      this._socket.send(encodeFrame(Type.DIAL, id, JSON.stringify(payload)))
    })
  }

  /** 发送原始帧（内部）。 */
  _send (frame) {
    if (this._closed) return
    this._socket.send(frame)
  }

  /** 关闭连接。 */
  close () {
    if (this._closed) return
    this._closed = true
    this._streams.forEach(st => st._markError(new Error('连接已关闭')))
    this._streams.clear()
    try { this._socket.close() } catch { /* 忽略 */ }
  }
}

let nextLocalID = 0

/** GatewayStream 一条双向流（网关侧网格流的客户端视图）。 */
export class GatewayStream {
  /**
   * @param {GatewayClient} client
   * @param {number} streamId
   * @param {boolean} inbound 是否为网格入向流
   */
  constructor (client, streamId, inbound) {
    this.client = client
    this.id = streamId
    this.inbound = inbound
    this.protocol = ''
    this.remotePeer = ''
    this._open = inbound // 入向流天然已建立
    this._localWriteClosed = false // 本端已半关闭写端
    this._remoteWriteClosed = false // 对端已半关闭写端（读端 EOF）
    this._eofFired = false
    this._error = null
    this._pending = null
    this._dataHandlers = []
    this._eofHandlers = []
    this._errorHandlers = []
  }

  _markOpen (info) {
    this._open = true
    this.viaRelay = !!info.via_relay
    if (this._pending) {
      this._pending.resolve(this)
      this._pending = null
    }
  }

  _markError (err) {
    if (this._error) return
    this._error = err
    if (this._pending) {
      this._pending.reject(err)
      this._pending = null
    }
    this._errorHandlers.forEach(cb => { try { cb(err) } catch { /* 忽略 */ } })
    this.client._streams.delete(this.id)
  }

  _emitData (bytes) {
    this._dataHandlers.forEach(cb => { try { cb(bytes) } catch { /* 忽略 */ } })
  }

  _emitEOF () {
    if (this._eofFired) return
    this._remoteWriteClosed = true
    this._eofFired = true
    this._eofHandlers.forEach(cb => { try { cb() } catch { /* 忽略 */ } })
  }

  /** 写数据（string 自动 UTF-8 编码）。 */
  write (data) {
    if (this._error) throw this._error
    if (this._localWriteClosed) throw new Error('写端已关闭')
    this.client._send(encodeFrame(Type.DATA, this.id, data))
  }

  /** 注册数据回调。 */
  onData (cb) { this._dataHandlers.push(cb) }

  /** 注册对端半关闭（EOF）回调。 */
  onEOF (cb) {
    if (this._eofFired) cb()
    else this._eofHandlers.push(cb)
  }

  /** 注册错误/重置回调。 */
  onError (cb) { this._errorHandlers.push(cb) }

  /** 半关闭写端：对端读到 EOF。本端仍可继续读取对端数据。 */
  closeWrite () {
    if (this._localWriteClosed) return
    this._localWriteClosed = true
    this.client._send(encodeFrame(Type.CLOSE, this.id))
  }

  /** 完全关闭流。 */
  close () {
    this.client._send(encodeFrame(Type.RESET, this.id))
    this.client._streams.delete(this.id)
    this._localWriteClosed = true
  }

  /** 便捷：收集全部数据直到 EOF（测试/小数据用）。 */
  readAll () {
    return new Promise((resolve, reject) => {
      const chunks = []
      this.onData(b => chunks.push(b))
      this.onEOF(() => resolve(concatBytes(chunks)))
      this.onError(reject)
    })
  }
}

/** 合并 Uint8Array 列表。 */
function concatBytes (chunks) {
  const total = chunks.reduce((n, c) => n + c.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const c of chunks) {
    out.set(c, offset)
    offset += c.length
  }
  return out
}

export { Type, Mode }
export default { createGatewayClient, GatewayClient, GatewayStream, Type, Mode }
