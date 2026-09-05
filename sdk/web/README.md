# @lanet/sdk-web

Lanet 网页 SDK：让浏览器（或 Node ≥20）作为一个**真正的 P2P 节点**加入 Lanet 群组——
不是经 HTTP 网关中转，而是 js-libp2p 直连组内 Go 节点。

## 能力与边界

| 能力 | 支持 | 说明 |
|---|---|---|
| 与 Go 节点互开流 | ✅ | 与 Go SDK 的 `Dial` / `OnStream` 对等，同走 `/pvn/tunnel/1.0.0` |
| 直连优先、中继兜底 | ✅ | WebRTC / WebTransport 直连，Circuit Relay v2 兜底 |
| 自定义协议开流 | ✅ | 经 `stream.raw`（底层 MessageStream）指定协议 |
| 访问组内 TCP 服务 | ✅* | 需 Go 节点侧运行 PortFWD 桥接（Go SDK 默认支持入向） |
| TUN / ping 虚拟 IP | ❌ | 浏览器沙箱无原始 IP 包能力 |
| 建群（创建群组） | ❌ | 网页节点凭邀请码加入（建群走 Go SDK / CLI） |

## 安装

```bash
npm install @lanet/sdk-web
```

ESM 包（`"type": "module"`）。浏览器环境建议用打包器（Vite/webpack）引入；
demo 目录提供了 importmap 纯静态示例。

## 快速开始

```js
import { createNode } from '@lanet/sdk-web'

// 1. 入网：凭邀请码加入群组，拿到虚拟 IP
const node = await createNode({
  ctlURL: 'http://ctl.example.com:8000', // 控制面地址（必填）
  inviteCode: 'grp-xxxxxxxx',            // 邀请码（必填）
  name: 'web-demo'                       // 节点名称，默认 web-<PeerID后6位>
})

console.log('入网成功，虚拟 IP:', node.virtualIP)

// 2. 接收入向流（对端 Go 节点 Dial 过来）
node.onStream(async (stream) => {
  console.log('入向流, viaRelay =', stream.viaRelay, 'protocol =', stream.protocol)
  stream.onMessage(data => {
    // data: Uint8Array（消息边界不做保证，按应用协议分帧）
  })
  await stream.closeWrite() // 回写完毕半关闭，对端读到 EOF
})

// 3. 按虚拟 IP 主动开流
const stream = await node.dial('10.7.0.2')
stream.send(new TextEncoder().encode('hello'))
await stream.closeWrite() // 请求体发送完毕，等对端回包后 EOF
```

## API 参考

### `createNode(options) → Promise<LanetNode>`

| 选项 | 类型 | 必填 | 默认 | 说明 |
|---|---|:---:|---|---|
| `ctlURL` | string | ✅ | — | 控制面地址，如 `http://ctl.example.com:8000` |
| `inviteCode` | string | ✅ | — | 邀请码（网页节点不支持建群） |
| `name` | string | — | `web-xxxxxx` | NetMap 中显示的节点名 |
| `os` | string | — | `browser` | OS 标识 |
| `relayAddrs` | string[] | — | 自动发现 | 显式指定 relay multiaddr（默认从 ctl `/v1/relays/candidates` 拉取，tcp 地址自动补 `/ws`） |

### `LanetNode`

| 成员 | 返回 | 说明 |
|---|---|---|
| `peerId` | string | 本节点 PeerID |
| `virtualIP` | string \| undefined | 入群分配的虚拟 IP |
| `group` | object | 群组信息（ctl join 响应原文） |
| `onStream(handler)` | void | 注册入向流回调（可多个，按序调用） |
| `dial(virtualIP)` | Promise\<LanetStream\> | 按虚拟 IP 开流；内部先查 NetMap，直连失败经 relay 电路兜底 |
| `resolve(virtualIP)` | Promise\<NetMapMember \| null\> | 虚拟 IP → `{ peer_id, name, os, virtual_ip, addrs }` |
| `netmap()` | Promise\<NetMap\> | 拉取群组成员目录（ctl） |
| `close()` | Promise\<void\> | 关闭节点（页面卸载时调用） |

### `LanetStream`

| 成员 | 说明 |
|---|---|
| `send(uint8Array)` | 写数据。**返回 `false` 表示发送缓冲已满**，应停发并等待 `raw` 的 `drain` 事件（背压处理） |
| `onMessage(cb)` | 数据到达回调（已归一化为 `Uint8Array`；**消息边界不保证**，需自行累积分帧） |
| `closeWrite()` | 半关闭写端：对端读到 EOF。本端读不受影响 |
| `abort()` | 立即复位流（不等对端） |
| `viaRelay` | 是否经中继（`false` = 直连） |
| `protocol` | 流上协商的协议号 |
| `raw` | 底层 libp2p MessageStream（事件式 API，进阶：`drain`/`close` 事件、自定义协议开流） |

### 请求-响应模式模板

```js
async function request(node, virtualIP, payload) {
  const stream = await node.dial(virtualIP)
  const chunks = []
  const done = new Promise(resolve => {
    stream.onMessage(d => chunks.push(d))
    stream.raw.addEventListener('close', resolve) // 对端回写完毕
  })
  stream.send(payload)
  await stream.closeWrite()          // 我方发送完毕
  await done
  stream.abort()
  return concat(chunks)              // 回包（按需分帧解析）
}
```

## 工作原理

1. **发现中继**：从控制面 `/v1/relays/candidates` 拉取 relay 列表（tcp 地址自动补 `/ws`）；
2. **建立节点**：js-libp2p（WebSockets + WebRTC + Circuit Relay v2 兜底 + noise 加密 + yamux 复用；
   WebTransport/WebRTC 按运行环境自动启用，Node 下自动跳过）；
3. **入网**：凭邀请码调用 ctl `/v1/groups/join`，以浏览器节点自己的 PeerID 拿到虚拟 IP；
4. **通告**：把浏览器可达地址通告到控制面；
5. **开流**：解析 NetMap 后优先直拨对端通告地址（ws 优先，浏览器附加 webrtc-direct），
   全部失败经 relay 电路（`/p2p-circuit`）兜底，与 Go 节点同走 `/pvn/tunnel/1.0.0` 协议。

浏览器与 Go 节点之间的连接建立**无需独立信令服务器**：Go 侧（SDK / agent）启用
`webrtc-direct` 传输层（默认开启，certhash 自带于通告地址中），relay 同时监听 tcp 与 ws。

## 部署要求

- ctl / relay 已部署（compose 即可）；relay 需开放 WebSocket（4001/tcp 已同时支持）；
- ctl 已内置 CORS 放行（网页 SDK 跨域调用 join/netmap/announce）；
- **页面为 HTTPS 时，ctl 与 relay 也必须 HTTPS/WSS**（浏览器混合内容策略），
  本地 `http://localhost` 联调无此要求；
- WebTransport 直连要求 relay/Go 节点侧 TLS 证书有效；仅 WebSocket 链路无此要求。

## 联调与验证

```bash
# 1. 起 Go echo 节点（启动日志打印邀请码）
go run ./app/agent/cmd/pvn-web-echo

# 2a. 浏览器 demo
cd sdk/web && npx serve .    # 打开 http://localhost:3000/demo/
#    demo 支持：入网、对 Go 节点 echo 往返测试、显示链路类型（直连/中继）与耗时

# 2b. Node 快速验证（无需浏览器）
node test/interop.mjs http://127.0.0.1:8000 <邀请码> <目标虚拟IP>
```

## 已验证链路（2026-09-04，本机实测）

| 路径 | 结果 |
|---|---|
| Node js-libp2p → Go 节点 ws 直连（127.0.0.1） | ✅ echo 往返 ~100ms |
| 入网 / NetMap / 邀请码 | ✅ |
| relay 电路兜底（circuit relay v2 over ws） | ✅ 可用（LAN 直连优先生效） |

## 已知限制与排查

- **Go 节点在 NAT 后时 webrtc-direct 地址被过滤**：libp2p 会从 `host.Addrs()`
  中过滤未确认可达的地址，此时浏览器走 relay 电路连接；公网部署（AutoNAT 确认可达）
  后直连地址会自动出现在通告中。
- **`send` 返回 false 后继续发**：数据会丢。必须等 `raw` 的 `drain` 事件。
- **onMessage 收到的数据不完整 / 粘包**：libp2p 流无消息边界，按应用协议分帧
  （长度前缀等）。demo 的 echo 场景例外（单请求单响应 + EOF 界定）。
- **入网报 CORS / mixed content**：见上方部署要求。
