import { GatewayClient, GatewayStream } from './index.js'

export declare function createGatewayClient (options: {
  url: string
  inviteCode: string
  name?: string
  mode?: 'client' | 'service'
  socketFactory?: (url: string) => {
    send: (data: Uint8Array) => void
    close: () => void
    onMessage: (cb: (data: Uint8Array | ArrayBuffer) => void) => void
    onClose: (cb: () => void) => void
    onError: (cb: (err: unknown) => void) => void
  }
}): Promise<GatewayClient>

export declare class GatewayStream {
  id: number
  inbound: boolean
  protocol: string
  remotePeer: string
  viaRelay?: boolean
  write (data: Uint8Array | string): void
  onData (cb: (data: Uint8Array) => void): void
  onEOF (cb: () => void): void
  onError (cb: (err: Error) => void): void
  closeWrite (): void
  close (): void
  readAll (): Promise<Uint8Array>
}

export declare class GatewayClient {
  info (): { virtualIP: string, peerID: string, group: string, mode: string }
  onStream (cb: (stream: GatewayStream) => void): void
  onError (cb: (err: unknown) => void): void
  onClose (cb: () => void): void
  dial (virtualIP: string, port: number): Promise<GatewayStream>
  dialProtocol (virtualIP: string, protocol: string): Promise<GatewayStream>
  close (): void
}

export { Type, Mode } from './frame.js'
export default GatewayClient
