/**
 * Lanet Web SDK：让网页作为一个真正的 P2P 节点加入 Lanet 群组。
 *
 * 连接策略（与 Go agent/SDK 的三段隧道语义对齐）：
 *  1. WebTransport 直连对端 Go 节点（如有可达地址，最低延迟）；
 *  2. WebRTC 直连（浏览器↔Go webrtc-direct，ICE 候选经信令完成）；
 *  3. Circuit Relay v2 兜底：经 relay 中继转发（ws 连 relay → /p2p-circuit 到对端）。
 *
 * 浏览器沙箱限制：无 TUN/原始 IP 包能力，一切访问按「流」进行
 * （对应 Go SDK 的 Dial / OnStream）。
 */

import { createLibp2p } from 'libp2p'
import { noise } from '@chainsafe/libp2p-noise'
import { yamux } from '@chainsafe/libp2p-yamux'
import { webSockets } from '@libp2p/websockets'
import { webTransport } from '@libp2p/webtransport'
import { webRTC } from '@libp2p/webrtc'
import { circuitRelayTransport } from '@libp2p/circuit-relay-v2'
import { identify } from '@libp2p/identify'
import { multiaddr } from '@multiformats/multiaddr'

/** Lanet 隧道协议号（与 pkg/protocol 一致）。 */
export const PROTOCOL_TUNNEL = '/pvn/tunnel/1.0.0'

/**
 * 创建节点并入网。
 *
 * @param {object} options
 * @param {string} options.ctlURL       控制面地址，如 http://ctl.example.com:8000
 * @param {string} options.inviteCode   邀请码（凭码加入群组）
 * @param {string} [options.name]       节点名称（默认随机）
 * @param {string} [options.os]         OS 标识（默认 "browser"）
 * @param {string[]} [options.relayAddrs] 显式 relay 地址（默认从控制面自动发现）
 * @param {boolean} [options.allowPrivateAddresses]  允许拨内网地址（默认 false，遵循浏览器安全策略）
 * @param {boolean} [options.allowInsecureWebSockets] 允许拨明文 ws://（默认 false，遵循浏览器安全策略）
 * @param {object}  [options.connectionGater]        完全自定义拨号闸门（高级用法，传入即接管默认策略）
 * @returns {Promise<LanetNode>}
 */
export async function createNode (options) {
  const {
    ctlURL,
    inviteCode,
    name,
    os = 'browser',
    relayAddrs,
    allowPrivateAddresses = false,
    allowInsecureWebSockets = false,
    connectionGater
  } = options ?? {}
  if (!ctlURL) throw new Error('lanet: ctlURL is required')
  if (!inviteCode) throw new Error('lanet: inviteCode is required（网页节点暂不支持建群）')

  const base = ctlURL.replace(/\/+$/, '')

  // 1. 发现 relay 候选（控制面统一目录）。
  const relays = relayAddrs ?? await discoverRelays(base)

  // 2. 创建 libp2p 节点：ws（连 relay）+ webtransport + webrtc（直连升级）+ circuit（兜底）。
  //    WebTransport/WebRTC 依赖浏览器全局 API，Node 环境自动跳过。
  const transports = [
    webSockets({ enableWebsocketUpgrade: true }),
    circuitRelayTransport({ discoverRelays: 2 })
  ]
  if (typeof globalThis.WebTransport === 'function') {
    transports.splice(1, 0, webTransport())
  }
  if (typeof globalThis.RTCPeerConnection === 'function') {
    transports.splice(1, 0, webRTC())
  }

  // 拨号闸门：浏览器内核默认**拒绝明文 ws:// 与内网地址**（libp2p 的
  // connection-gater.browser.js）—— 公网 https 部署下这是必要的保护；但内网部署的
  // H5 页面（页面自身就在 http / 内网）其实是允许 ws:// 与内网地址的，用
  // allowPrivateAddresses / allowInsecureWebSockets 显式放开。
  // 不传任何豁免时**不注入**，完全沿用 libp2p 内置策略（Node 下全放行、浏览器下保守）。
  const dialGater = connectionGater ?? (
    (allowPrivateAddresses || allowInsecureWebSockets)
      ? {
          denyDialMultiaddr: (ma) => {
            if (!allowInsecureWebSockets && isInsecureWebSocket(ma)) return true
            return !allowPrivateAddresses && isPrivateAddress(ma)
          }
        }
      : undefined
  )

  const node = await createLibp2p({
    addresses: { listen: [] },
    transports,
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    services: { identify: identify() },
    ...(dialGater ? { connectionGater: dialGater } : {})
  })

  // 3. 连接 relay 并完成电路预约（保证可被组内成员访问）。
  for (const relay of relays) {
    try {
      await node.dial(multiaddr(relay))
      break
    } catch (err) {
      console.warn('[lanet] relay dial failed:', relay, err)
    }
  }

  const peerId = node.peerId.toString()

  // 4. 凭邀请码入组，拿到虚拟 IP。
  const joinRes = await postJSON(`${base}/v1/groups/join`, {
    invite_code: inviteCode, peer_id: peerId, name: name ?? `web-${peerId.slice(-6)}`, os
  })
  const virtualIP = joinRes?.member?.virtual_ip

  // 5. 通告可达地址（浏览器仅有 webRTC/webTransport 的 Certhash 地址）。
  const addrs = node.getMultiaddrs().map(a => a.toString())
  await postJSON(`${base}/v1/groups/announce`, { peer_id: peerId, addrs }).catch(() => {})

  return new LanetNode(node, { peerId, virtualIP, group: joinRes?.group, base, relays })
}

/** LanetNode 已入网的网页节点。 */
export class LanetNode {
  /** @param {import('libp2p').Libp2p} node */
  constructor (node, info) {
    this._node = node
    this.peerId = info.peerId
    this.virtualIP = info.virtualIP
    this.group = info.group
    this._base = info.base
    this._relays = info.relays ?? []
    this._handlers = []
    node.handle(PROTOCOL_TUNNEL, ({ stream, connection }) => {
      for (const handler of this._handlers) {
        handler(wrapStream(stream, connection))
      }
    })
  }

  /** 注册入向流回调（可多个）。 */
  onStream (handler) { this._handlers.push(handler) }

  /**
   * 按虚拟 IP 开流。内部先解析 NetMap，再依次尝试直连/电路路径。
   * @returns {Promise<LanetStream>}
   */
  async dial (virtualIP, options = {}) {
    const route = await this.resolve(virtualIP)
    if (!route) throw new Error(`lanet: virtual IP ${virtualIP} not in group netmap`)
    const target = route.peer_id
    const connection = await this._connectToPeer(target, route)
    const stream = await connection.newStream(PROTOCOL_TUNNEL, options)
    return wrapStream(stream, connection)
  }

  /** 查询 NetMap 解析虚拟 IP → { peer_id, addrs }。 */
  async resolve (virtualIP) {
    const netmap = await this.netmap()
    return (netmap?.members ?? []).find(m => m.virtual_ip === virtualIP) ?? null
  }

  /** 拉取群组 NetMap（控制面）。 */
  async netmap () {
    const data = await getJSON(`${this._base}/v1/groups/netmap?peer_id=${encodeURIComponent(this.peerId)}`)
    return data ?? null
  }

  /** 连接到对端：优先已有连接，其次通告地址，最后经 relay 电路兜底。 */
  async _connectToPeer (peerIdStr, route) {
    const existing = this._node.getConnections(peerIdStr)[0]
    if (existing) return existing

    const candidates = []
    for (const raw of (route?.addrs ?? [])) {
      try {
        const s = String(raw)
        // webrtc-direct 仅浏览器可拨；tcp 地址补 /ws 供 WebSocket 拨号。
        if (s.includes('/webrtc-direct') && typeof globalThis.RTCPeerConnection !== 'function') continue
        if (s.includes('/tcp/') && !s.includes('/ws')) {
          candidates.push(multiaddr(`${s}/ws/p2p/${peerIdStr}`))
        } else {
          candidates.push(multiaddr(`${s}/p2p/${peerIdStr}`))
        }
      } catch { /* 忽略非法地址 */ }
    }

    for (const addr of candidates) {
      try {
        return await this._node.dial(addr)
      } catch (err) {
        console.warn('[lanet] direct dial failed, trying next:', addr.toString(), err.message)
      }
    }
    // 兜底：经 relay 电路拨号（对端需持有 relay 预约）。
    for (const relay of this._relays) {
      try {
        const circuit = multiaddr(relay).encapsulate(`/p2p-circuit/p2p/${peerIdStr}`)
        return await this._node.dial(circuit)
      } catch (err) {
        console.warn('[lanet] circuit dial failed via relay:', relay, err.message)
      }
    }
    throw new Error(`lanet: 无法连接对端 ${peerIdStr}（直连与中继均失败）`)
  }

  /** 关闭节点（页面卸载/主动下线时调用）。 */
  async close () { await this._node.stop() }
}

/** LanetStream 隧道流（libp2p MessageStream 的事件式读写视图）。 */
export class LanetStream {
  constructor (stream, connection) {
    this._stream = stream
    this._connection = connection
    this._viaRelay = connection.remoteAddr.toString().includes('/p2p-circuit')
  }

  /** 是否经中继（false = 直连）。 */
  get viaRelay () { return this._viaRelay }

  /** 协商出的协议号。 */
  get protocol () { return this._stream.protocol }

  /**
   * 发送数据。返回 false 表示发送缓冲已满，需等待 drain 事件后再发。
   * @param {Uint8Array} data
   */
  send (data) { return this._stream.send(data) }

  /** 注册数据到达回调（data 已归一化为 Uint8Array，消息边界不做保证，需自行累积）。 */
  onMessage (cb) {
    this._stream.addEventListener('message', evt => {
      const data = evt.data instanceof Uint8Array ? evt.data : evt.data.subarray()
      cb(data)
    })
  }

  /** 半关闭写端：对端可读到 EOF。 */
  async closeWrite () { await this._stream.close() }

  /** 立即复位流（不等对端）。 */
  abort () { this._stream.abort(new Error('lanet: closed by user')) }

  /** 底层 libp2p MessageStream（进阶用法）。 */
  get raw () { return this._stream }
}

function wrapStream (stream, connection) {
  return new LanetStream(stream, connection)
}

// --------------------------------------------------------------------
// 拨号闸门：浏览器默认安全策略与内网部署的豁免
// --------------------------------------------------------------------

/**
 * 把 multiaddr 按 `/` 拆成协议段（无值协议如 ws / quic-v1 同样成段）。
 *
 * 不用正则：multiaddr 的段结构天然就是 `/` 分隔，直接拆分比正则更稳且更好读。
 */
function addrParts (ma) {
  return ma.toString().split('/').filter(Boolean)
}

/**
 * multiaddr 是否为「明文 WebSocket」：有 ws 段且无 wss 段。
 *
 * 浏览器内核（libp2p 的 connection-gater.browser.js）默认拒拨明文 ws，用于防止
 * https 页面降级到不安全连接；内网 http 部署时可按需放开。
 */
function isInsecureWebSocket (ma) {
  const parts = addrParts(ma)
  return parts.includes('ws') && !parts.includes('wss')
}

/**
 * multiaddr 指向的 IP 是否属于「浏览器默认不允许直拨」的段：
 * 回环、RFC1918 私网、链路本地、CGNAT 与 IPv6 ULA / 回环 / 链路本地。
 * dns4 / dns6 不做判定（交给浏览器自行解析与拦截）。
 */
function isPrivateAddress (ma) {
  const parts = addrParts(ma)
  const i4 = parts.indexOf('ip4')
  if (i4 >= 0 && parts[i4 + 1] != null) return isPrivateIPv4(parts[i4 + 1])
  const i6 = parts.indexOf('ip6')
  if (i6 >= 0 && parts[i6 + 1] != null) return isPrivateIPv6(parts[i6 + 1])
  return false
}

/** IPv4 是否属于不可公网直达的段。 */
function isPrivateIPv4 (raw) {
  const nums = String(raw).split('.').map(Number)
  if (nums.length !== 4) return false
  if (nums.some(n => !Number.isInteger(n) || n < 0 || n > 255)) return false
  const [a, b] = nums
  if (a === 0 || a === 10 || a === 127) return true      // 0/8、10/8、127/8
  if (a === 172 && b >= 16 && b <= 31) return true       // 172.16/12
  if (a === 192 && b === 168) return true                // 192.168/16
  if (a === 169 && b === 254) return true                // 169.254/16（链路本地）
  if (a === 100 && b >= 64 && b <= 127) return true      // 100.64/10（CGNAT）
  return false
}

/** IPv6 是否属于不可公网直达的段（zone id 会被剥离，如 fe80::1%eth0）。 */
function isPrivateIPv6 (raw) {
  const s = String(raw).toLowerCase().split('%')[0]
  if (s === '::1' || s === '::') return true
  if (s.startsWith('fc') || s.startsWith('fd')) return true      // fc00::/7（ULA）
  if (s.startsWith('fe8') || s.startsWith('fe9')) return true    // fe80::/10
  if (s.startsWith('fea') || s.startsWith('feb')) return true    // fe80::/10
  return false
}

async function discoverRelays (base) {
  const data = await getJSON(`${base}/v1/relays/candidates?limit=2`)
  return (data?.candidates ?? []).flatMap(c => (c.addrs ?? []).map(a => {
    // 候选地址自带 /p2p/<id> 后缀；浏览器/Node 只能经 WebSocket 拨号，
    // tcp 地址需在 /p2p 组件之前插入 /ws（如 /tcp/4001/p2p/x → /tcp/4001/ws/p2p/x）。
    if (a.includes('/tcp/') && !a.includes('/ws')) {
      const idx = a.indexOf('/p2p/')
      return idx >= 0 ? `${a.slice(0, idx)}/ws${a.slice(idx)}` : `${a}/ws`
    }
    return a
  }))
}


async function postJSON (url, payload) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  })
  const body = await res.json().catch(() => null)
  if (!res.ok || !body || body.code !== 0) {
    throw new Error(`lanet: ${url} failed: ${body?.message ?? res.status}`)
  }
  return body.data
}

async function getJSON (url) {
  const res = await fetch(url)
  const body = await res.json().catch(() => null)
  if (!res.ok || !body || body.code !== 0) {
    throw new Error(`lanet: ${url} failed: ${body?.message ?? res.status}`)
  }
  return body.data
}
