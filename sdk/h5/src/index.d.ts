/**
 * Type definitions for @lanet/sdk-h5
 *
 * H5 单文件直引包：`<script src="dist/lanet-h5.min.js"></script>` 后用全局 `Lanet`，
 * 或 `import { connect } from '@lanet/sdk-h5'`。
 */

export declare const PROTOCOL_TUNNEL: string
export declare const VERSION: string

/** 隧道流（与 `@lanet/sdk-web` 的 LanetStream 同形）。 */
export interface LanetStream {
  /** 是否经中继转发（false = 直连）。 */
  readonly viaRelay: boolean
  /** 流上协商的协议号。 */
  readonly protocol: string
  /** 写数据；返回 false 表示发送缓冲已满，需等 `raw` 的 drain 事件。 */
  send(data: Uint8Array): boolean
  /** 注册数据到达回调（消息边界不保证，需自行累积分帧）。 */
  onMessage(cb: (data: Uint8Array) => void): void
  /** 半关闭写端：对端读到 EOF。 */
  closeWrite(): Promise<void>
  /** 立即复位流（不等对端）。 */
  abort(): void
  /** 底层 libp2p MessageStream（进阶用法）。 */
  readonly raw: import('libp2p').Stream
}

/** NetMap 成员条目。 */
export interface NetMapMember {
  peer_id: string
  name: string
  os: string
  virtual_ip: string
  addrs: string[]
}

export interface ConnectOptions {
  /** 控制面地址（必填），如 `https://ctl.example.com`。 */
  ctlURL: string
  /** 邀请码（必填；网页节点不支持建群）。 */
  inviteCode: string
  /** 节点名称，默认 `h5-<PeerID 后 6 位>`。 */
  name?: string
  /** 显式指定 relay multiaddr（默认从控制面自动发现）。 */
  relayAddrs?: string[]
  /** 入向流回调（等价于入网后立刻 onStream）。 */
  onStream?: (stream: LanetStream) => void
  /** 页面卸载（pagehide）时自动下线，默认 true。 */
  autoClose?: boolean
  /** request 系列默认超时（毫秒），默认 15000。 */
  requestTimeout?: number
  /**
   * request 系列默认重试次数，默认 0。
   *
   * 只重试「请求确定未被处理」的失败（写不进去 / 流被复位）—— 例如刚入网时同群成员
   * 还没刷新 NetMap（Go SDK 默认 15s 一轮）导致入向流被防火墙复位。超时不重试。
   */
  retries?: number
  /** request 系列默认重试间隔（毫秒），默认 2000。 */
  retryDelay?: number
  /** 超时是否也重试，默认 false。仅推荐对幂等请求开启。 */
  retryOnTimeout?: boolean
  /**
   * 允许拨内网地址，默认 false（透传给 Web SDK）。
   * 浏览器内核默认拒拨内网地址；内网部署的 H5 页面（自身就在内网）浏览器实际允许，
   * 需要显式开启，否则入网后拨号会全被拦下。
   */
  allowPrivateAddresses?: boolean
  /**
   * 允许拨明文 ws://，默认 false（透传给 Web SDK）。
   * 浏览器默认只允许 wss://；H5 页面自身走 http 时浏览器允许 ws://，需要显式开启。
   */
  allowInsecureWebSockets?: boolean
  /** 完全自定义拨号闸门（高级用法，透传给 Web SDK）；传入即接管默认策略。 */
  connectionGater?: {
    denyDialPeer?(peerId: unknown): boolean
    denyDialMultiaddr?(multiaddr: unknown): boolean
  }
}

/** 请求-响应可选项。 */
export interface RequestOptions {
  /** 超时毫秒；0 表示不超时。 */
  timeout?: number
  /** 重试次数（覆盖 connect 时的默认值）。只对「确定未被处理」的失败生效。 */
  retries?: number
  /** 重试间隔毫秒（覆盖 connect 时的默认值）。 */
  retryDelay?: number
  /** 超时是否也重试（覆盖 connect 时的默认值）。 */
  retryOnTimeout?: boolean
  /** 流建立后、发送前回调，用于诊断（读 `stream.viaRelay` / `protocol`）。 */
  onStream?: (stream: LanetStream) => void
}

export declare class H5Node {
  /** 本节点 PeerID。 */
  readonly peerId: string
  /** 入网分配的虚拟 IP。 */
  readonly virtualIP?: string
  /** 群组信息（控制面 join 响应原文）。 */
  readonly group: unknown
  /** 底层 `@lanet/sdk-web` 节点（进阶用法）。 */
  readonly web: unknown

  onStream(handler: (stream: LanetStream) => void): this
  dial(virtualIP: string, options?: object): Promise<LanetStream>
  resolve(virtualIP: string): Promise<NetMapMember | null>
  netmap(): Promise<{ members: NetMapMember[] } | null>

  /** 请求-响应：发完半关闭，等对端回写完毕（EOF）返回全部响应字节。 */
  request(
    virtualIP: string,
    payload: string | Uint8Array | ArrayBuffer,
    options?: RequestOptions
  ): Promise<Uint8Array>
  /** 请求-响应（文本）。 */
  requestText(virtualIP: string, text: string, options?: RequestOptions): Promise<string>
  /** 请求-响应（JSON）；响应为空返回 null。 */
  requestJSON<T = unknown>(virtualIP: string, payload: unknown, options?: RequestOptions): Promise<T | null>
  /** 单向发送，不等响应。 */
  sendText(virtualIP: string, text: string, options?: RequestOptions): Promise<void>
  /** 关闭节点并下线（幂等）。 */
  close(): Promise<void>
}

/** 增量分帧解码器（4 字节大端长度前缀）。 */
export declare class FrameDecoder {
  constructor(onFrame: (frame: Uint8Array) => void, options?: { maxFrameSize?: number })
  /** 喂入任意切分的字节块。 */
  push(chunk: Uint8Array): this
  /** 清空未凑齐的残留。 */
  reset(): void
  /** 当前残留（半帧）字节数。 */
  readonly pending: number
}

/** 4 字节大端长度前缀分帧编码。 */
export declare function encodeFrame(payload: string | Uint8Array | ArrayBuffer): Uint8Array

/** 连接并入网。 */
export declare function connect(options: ConnectOptions): Promise<H5Node>

declare const Lanet: {
  connect: typeof connect
  H5Node: typeof H5Node
  FrameDecoder: typeof FrameDecoder
  encodeFrame: typeof encodeFrame
  PROTOCOL_TUNNEL: string
  VERSION: string
  version: string
}

export default Lanet
