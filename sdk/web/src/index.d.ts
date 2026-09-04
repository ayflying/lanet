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
