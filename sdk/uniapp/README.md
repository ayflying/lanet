# @lanet/sdk-uniapp

Lanet ws-gateway 客户端：让 **uniapp / 微信小程序 / H5 / App / Node** 接入 Lanet 群组网格，
与 Go / 网页节点互开流、访问网格内 TCP 服务。

底层协议与 Go（`pkg/gatewayproto`）、C#（`sdk/csharp`）字节级一致：
`[type:1][streamID:4 大端][len:4 大端][payload]`。

## 先回答关键问题：uniapp 能支持 P2P 吗？

**分端讨论：**

| uniapp 端 | 能否真 P2P | 原因 |
|---|---|---|
| **小程序**（微信/支付宝等） | ❌ 不能 | 小程序没有 `RTCPeerConnection`/WebTransport，只有受限的 `wx.connectSocket`（还要求 wss+备案域名），跑不了 libp2p 协议栈 |
| **App**（Android/iOS 原生打包） | ❌ 不能（本 SDK 内） | JS 运行在独立引擎（非浏览器），同样没有 WebRTC 数据通道；要用 P2P 需原生插件嵌 libp2p（roadmap） |
| **H5** | ✅ 可以 | H5 就是浏览器，直接改用 [@lanet/sdk-web](../web/README.md) 走真 P2P（webrtc-direct 直连 Go 节点 + relay 兜底） |

**本 SDK 的定位**：小程序/App 这类「跑不了 libp2p 的端」，经 **ws-gateway** 接入群组：

```
小程序 / App ──(WebSocket 帧协议)──→ ws-gateway ──(libp2p 隧道)──→ 网格内目标节点
```

- 这不是端到端 P2P：数据经网关**实时转发、不落盘**，路径多一跳；
- 网关本身是群内一个 Go 节点，网格内其他成员看到的是「网关节点 + 若干客户端」；
- 对业务代码透明：API 与 C# SDK 完全同构，dial 的目标仍是对端节点虚拟 IP。

## 能力边界

- ✅ 按虚拟 IP 开流、访问网格内成员的 TCP 服务（经网关 PortFWD）
- ✅ 自定义协议直开流（对端节点注册了对应协议处理器）
- ✅ service 模式接收入向流（网关同一时刻允许**一个** service 连接）
- ❌ TUN 内核组网、ping 虚拟 IP（网关是转发节点，非端到端）
- ❌ 网页节点之间直连（小程序端无此能力）

## 安装

```bash
npm install @lanet/sdk-uniapp
```

- uniapp（Vue3 / HBuilderX）：直接 import；
- 微信小程序：需要在「小程序后台 → 开发设置 → socket 合法域名」配置网关域名；
- Node：依赖 `ws` 包（已声明在 dependencies，自动可用）。

## 快速开始

### client 模式：访问网格内服务

```js
import { createGatewayClient } from '@lanet/sdk-uniapp'

// 1. 连接网关并完成邀请码鉴权
const client = await createGatewayClient({
  url: 'ws://gateway.example.com:8700/gateway', // 小程序必须 wss://
  inviteCode: 'XXXXXXXXXX',                     // 网关所属群组邀请码（网关启动日志打印）
  name: 'my-uniapp'
})

console.log('经网关入群:', client.info().group, '虚拟IP:', client.info().virtualIP)

// 2. 访问网格内 TCP 服务（目标节点虚拟 IP + 其本机端口）
const stream = await client.dial('10.7.0.3', 9999)
stream.write('hello')                 // string 自动 UTF-8；也接受 Uint8Array
stream.closeWrite()                   // 对端读到 EOF（务必调用）
const reply = await stream.readAll()  // 读到对端 EOF 为止
console.log(new TextDecoder().decode(reply))
stream.close()

// 3. 自定义协议流（对端节点注册了该协议处理器时）
const s2 = await client.dialProtocol('10.7.0.3', '/myapp/1.0.0')

// 4. 连接级回调
client.onError(err => console.error('连接错误', err))
client.onClose(() => console.warn('连接关闭，可在此重连'))
```

### service 模式：接收入向流

```js
const svc = await createGatewayClient({
  url: 'ws://gateway.example.com:8700/gateway',
  inviteCode: 'XXXXXXXXXX',
  name: 'my-uniapp-service',
  mode: 'service'
})
svc.onStream(stream => {
  console.log('入向流:', stream.protocol, 'from', stream.remotePeer)
  stream.onData(data => stream.write(data)) // echo 示例
  stream.onEOF(() => console.log('对端已半关闭'))
})
```

### 流式读（不一次 readAll）

```js
stream.onData(chunk => {
  // chunk: Uint8Array。消息边界不保证，按应用协议分帧
})
stream.onEOF(() => { /* 对端 closeWrite，读端结束 */ })
stream.onError(err => { /* 流被重置或连接中断 */ })
```

## API 参考

### `createGatewayClient(options) → Promise<GatewayClient>`

| 选项 | 类型 | 必填 | 默认 | 说明 |
|---|---|:---:|---|---|
| `url` | string | ✅ | — | 网关地址 `ws://host:8700/gateway`（小程序必须 `wss://`） |
| `inviteCode` | string | ✅ | — | 群组邀请码 |
| `name` | string | — | `uniapp-client` | 客户端名称 |
| `mode` | string | — | `client` | `client`（主动开流）或 `service`（接收入向流） |
| `socketFactory` | Function | — | 自动适配 | 自定义 socket 工厂 `(url) => SocketLike`（见下） |

### `GatewayClient`

| 成员 | 说明 |
|---|---|
| `info()` | `{ virtualIP, peerID, group, mode }`（鉴权成功后有效） |
| `dial(virtualIP, port)` | PortFWD：桥接目标节点本机 TCP 服务 → Promise\<GatewayStream\> |
| `dialProtocol(virtualIP, protocol)` | 自定义协议开流 → Promise\<GatewayStream\> |
| `onStream(cb)` | 注册网格入向流回调（service 模式） |
| `onError(cb)` | 连接级错误回调 |
| `onClose(cb)` | 连接关闭回调（可在此重连） |
| `close()` | 关闭连接并中止所有流 |

### `GatewayStream`

| 成员 | 说明 |
|---|---|
| `write(data)` | 写数据（string 自动 UTF-8 / Uint8Array）；写端已关闭或流出错时抛异常 |
| `onData(cb)` | 数据到达回调（Uint8Array） |
| `onEOF(cb)` | 对端半关闭回调；**若 EOF 已发生会立即触发** |
| `onError(cb)` | 流被重置 / 连接中断 |
| `closeWrite()` | 半关闭写端：对端读到 EOF；本端仍可继续读 |
| `close()` | 完全关闭流（发 Reset） |
| `readAll()` | 便捷：收集全部数据直到 EOF（Promise\<Uint8Array\>，适合请求-响应小数据） |
| `id` / `inbound` / `protocol` / `remotePeer` / `viaRelay` | 流元信息（viaRelay 在 dial 成功后有效） |

### 自定义 socket 工厂

运行环境不在 uni / wx / 浏览器之列（React Native、嵌入式 JS 引擎等）时注入：

```ts
interface SocketLike {
  send: (data: Uint8Array) => void
  close: () => void
  onMessage: (cb: (data: Uint8Array | ArrayBuffer) => void) => void
  onClose: (cb: () => void) => void
  onError: (cb: (err: unknown) => void) => void
}

const client = await createGatewayClient({
  url: 'wss://...',
  inviteCode: 'XXXX',
  socketFactory: url => mySocketAdapter(url)
})
```

默认适配顺序：`uni.connectSocket` → `wx.connectSocket` → 浏览器 `WebSocket`。
open 前的 send 会自动排队，open 后 flush。

## 服务端部署（ws-gateway）

```bash
# ctl + relay 已部署的前提下：
go run ./app/gateway/cmd/pvn-gateway \
    -ctl http://127.0.0.1:8000 \
    -listen :8700 \
    -invite <已有群组邀请码>     # 留空则创建新群组，启动日志打印邀请码
```

小程序部署要求：

- 网关必须经 **wss + 备案域名** 访问（微信平台要求），前置 nginx/caddy 终结 TLS；
- 小程序后台配置 socket 合法域名；
- 长连接保活：小程序 `wx.connectSocket` 有空闲断开策略，业务层建议周期发业务心跳
  （本 SDK 暂未内置自动心跳，可自行扩展 DATA/PING 帧）。

## 联调验证

```bash
# 服务端：ctl + relay + gateway + go-service 节点 + TCP 回显 9999
npm install && npm test -- http://127.0.0.1:8000 <inviteCode> <targetVirtualIP>
```

实测记录（2026-09-04，本机）：鉴权 → 自定义协议 echo 往返 1ms →
PortFWD TCP 回显往返 3ms → 心跳，全链路 PASS。

## 常见问题

**Q：H5 端想要真 P2P？**
用 [@lanet/sdk-web](../web/README.md)，H5 就是浏览器环境，支持 webrtc-direct 直连 Go 节点。

**Q：dial 报「开流失败」？**
目标虚拟 IP 不在群内 / 对端离线 / 目标端口未监听；DIAL_ERR 帧的 payload 是错误文本，监听 onError 查看。

**Q：写完数据对端读不到 EOF？**
必须 `stream.closeWrite()`（发 CLOSE 帧）。这是唯一 EOF 语义。

**Q：断线重连？**
当前不内置。监听 `onClose`，重建 `createGatewayClient` 并重开业务流即可（帧协议无状态）。

**Q：readAll 卡住不返回？**
对端没 closeWrite。请求-响应场景对端回包后需半关闭；长流场景请用 onData/onEOF。
