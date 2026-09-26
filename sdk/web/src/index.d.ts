Type definitions for @lanet/sdk-web

export declare const PROTOCOL_TUNNEL: string

export interface LanetStream {
  readonly viaRelay: boolean
  closeWrite(): Promise<void>
  close(): Promise<void>
  readonly raw: import('libp2p').Stream
}

export interface NetMapMember {
  peer_id: string
  name: string
  os: string
  virtual_ip: string
  addrs: string[]
}

export interface CreateNodeOptions {
  ctlURL: string
  inviteCode: string
  name?: string
  os?: string
  relayAddrs?: string[]
  /**
   * 允许拨内网地址，默认 false。
   * libp2p 浏览器内核默认拒拨内网地址（connection-gater.browser.js）；而内网部署的
   * H5 / 网页（页面自身就在内网）浏览器实际是允许的，需要显式开启。
   */
  allowPrivateAddresses?: boolean
  /**
   * 允许拨明文 ws://，默认 false。
   * 浏览器默认只允许 wss://；页面自身走 http 时浏览器允许 ws://，需要显式开启。
   */
  allowInsecureWebSockets?: boolean
  /** 完全自定义拨号闸门（高级用法）；传入即接管默认策略。 */
  connectionGater?: {
    denyDialPeer?(peerId: unknown): boolean
    denyDialMultiaddr?(multiaddr: unknown): boolean
  }
}

export declare class LanetNode {
  readonly peerId: string
  readonly virtualIP: string | undefined
  readonly group: unknown
  onStream(handler: (stream: LanetStream) => void): void
  dial(virtualIP: string, options?: object): Promise<LanetStream>
  resolve(virtualIP: string): Promise<NetMapMember | null>
  netmap(): Promise<{ members: NetMapMember[] } | null>
  close(): Promise<void>
}

export declare function createNode(options: CreateNodeOptions): Promise<LanetNode>
