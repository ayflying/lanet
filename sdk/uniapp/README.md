# @lanet/sdk-uniapp

Lanet ws-gateway 客户端：让 **uniapp / 微信小程序 / H5 / Node** 接入群组网格，
与 Go / 网页节点互开流、访问网格内 TCP 服务。

底层协议与 Go（`pkg/gatewayproto`）、C#（`sdk/csharp`）字节级一致。

## 能力边界（与网页 SDK 一致）

- ✅ 按虚拟 IP 开流、访问网格内成员的 TCP 服务（经网关 PortFWD）
- ✅ 自定义协议直开流（对端注册了对应协议处理器）
- ✅ service 模式接收入向流（网关同一时刻允许一个 service 连接）
- ❌ TUN 内核组网、ping 虚拟 IP（网关是转发节点，非端到端）

## 快速开始

```js
import { createGatewayClient } from '@lanet/sdk-uniapp'

const client = await createGatewayClient({
  url: 'ws://gateway.example.com:8700/gateway', // 或 wss://（小程序必须）
  inviteCode: 'XXXXXXXXXX',                      // 网关所属群组邀请码
  name: 'my-uniapp'
})

console.log('经网关入群:', client.info().group)

// 访问网格内 TCP 服务（目标节点虚拟 IP + 其本机端口）
const stream = await client.dial('100.64.0.3', 9999)
stream.write('hello')
stream.closeWrite()             // 对端读到 EOF
const reply = await stream.readAll()
console.log(new TextDecoder().decode(reply))
stream.close()

// 自定义协议流（对端节点注册了对应协议处理器时）
const s2 = await client.dialProtocol('100.64.0.3', '/pvn/tunnel/1.0.0')

// service 模式（接收网格内节点连入）
const svc = await createGatewayClient({
  url: 'ws://gateway.example.com:8700/gateway',
  inviteCode: 'XXXXXXXXXX', mode: 'service'
})
svc.onStream(stream => {
  stream.onData(data => stream.write(data)) // echo
})
```

## 平台适配

默认自动识别运行环境（uni / wx / 浏览器）。其他环境（Node、
React Native 等）通过 `socketFactory` 注入，只需满足：

```ts
interface SocketLike {
  send: (data: Uint8Array) => void
  close: () => void
  onMessage: (cb: (data: Uint8Array | ArrayBuffer) => void) => void
  onClose: (cb: () => void) => void
  onError: (cb: (err: unknown) => void) => void
}
```

## 小程序部署要求

- 网关必须经 **wss + 备案域名** 访问（微信平台要求）
- 在小程序后台配置 socket 合法域名

## 联调验证

```bash
# 服务端：ctl + relay + gateway + go-service 节点 + TCP 回显 9999
npm install && npm test -- http://127.0.0.1:8000 <inviteCode> <targetVirtualIP>
```

实测记录（2026-09-04，本机）：鉴权 → 自定义协议 echo 往返 1ms →
PortFWD TCP 回显往返 3ms → 心跳，全链路 PASS。
