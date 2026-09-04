/**
 * Lanet ws-gateway 帧协议编解码（与 pkg/gatewayproto 字节级一致）。
 *
 * 一条二进制 WS 消息 = 一个帧：
 *   [type:1][streamID:4 大端][payload 长度:4 大端][payload]
 */

export const Type = {
  AUTH: 0x01,
  AUTH_OK: 0x02,
  AUTH_ERR: 0x03,
  DIAL: 0x04,
  DIAL_OK: 0x05,
  DIAL_ERR: 0x06,
  DATA: 0x07,
  CLOSE: 0x08,
  RESET: 0x09,
  PING: 0x0a,
  PONG: 0x0b,
  STREAM_OPEN: 0x0c
}

export const Mode = { CLIENT: 'client', SERVICE: 'service' }

/**
 * 编码帧。payload 接受 Uint8Array / ArrayBuffer / string（UTF-8）。
 * @returns {Uint8Array}
 */
export function encodeFrame (type, streamId = 0, payload = null) {
  let bytes
  if (payload == null) {
    bytes = new Uint8Array(0)
  } else if (typeof payload === 'string') {
    bytes = new TextEncoder().encode(payload)
  } else if (payload instanceof ArrayBuffer) {
    bytes = new Uint8Array(payload)
  } else if (payload instanceof Uint8Array) {
    bytes = payload
  } else {
    bytes = new Uint8Array(payload)
  }
  const buf = new Uint8Array(9 + bytes.length)
  const dv = new DataView(buf.buffer)
  buf[0] = type
  dv.setUint32(1, streamId >>> 0)
  dv.setUint32(5, bytes.length)
  buf.set(bytes, 9)
  return buf
}

/**
 * 解码帧。data 接受 Uint8Array / ArrayBuffer（uniapp 各端二进制消息的常见形态）。
 * @returns {{ type: number, streamId: number, payload: Uint8Array }}
 */
export function decodeFrame (data) {
  const u8 = data instanceof Uint8Array ? data : new Uint8Array(data)
  if (u8.length < 9) throw new Error('帧不完整（不足 9 字节头）')
  const dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength)
  const len = dv.getUint32(5)
  if (u8.length - 9 < len) throw new Error(`帧不完整：需要 ${len} 字节，实际 ${u8.length - 9}`)
  return {
    type: u8[0],
    streamId: dv.getUint32(1),
    payload: u8.subarray(9, 9 + len)
  }
}

/** payload → JSON 对象（空载荷返回 null）。 */
export function decodeJSON (frame) {
  if (!frame.payload || frame.payload.length === 0) return null
  return JSON.parse(new TextDecoder().decode(frame.payload))
}
