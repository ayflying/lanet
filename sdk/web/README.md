# @lanet/sdk-web

Lanet 网页 SDK：让浏览器作为一个**真正的 P2P 节点**加入 Lanet 群组——
不是通过 HTTP 网关，而是 js-libp2p 直连组内 Go 节点。

## 能力与边界

| 能力 | 支持 | 说明 |
|---|---|---|
| 与 Go 节点互开流 | ✅ | 与 Go SDK 的 `Dial` / `OnStream` 对等 |
| 直连优先、中继兜底 | ✅ | WebTransport / WebRTC 直连，Circuit Relay v2 兜底 |
| 访问组内 TCP 服务 | ✅* | 需 Go 节点侧 portfwd 桥接（roadmap） |
| TUN / ping 虚拟 IP | ❌ | 浏览器沙箱无原始 IP 包能力 |
| 建群（创建群组） | ❌ | 网页节点凭邀请码加入（建群建议走 Go SDK/CLI） |

## 安装

```bash
npm install @lanet/sdk-web
```

## 快速开始

```js
import { createNode } from '@lanet/sdk-web'

const node = await createNode({
  ctlURL: 'http://ctl.example.com:8000',
  inviteCode: 'grp-xxxxxxxx',
  name: 'web-demo'
})

console.log('入网成功，虚拟 IP:', node.virtualIP)

// 接收入向流（对端 Go 节点 Dial 过来）
node.onStream(async (stream) => {
  console.log('入向流, viaRelay =', stream.viaRelay)
  stream.onMessage(data => { /* Uint8Array 分片，自行累积 */ })
  await stream.closeWrite() // 对端可读到 EOF
})

// 按虚拟 IP 开流
const stream = await node.dial('100.64.0.2')
stream.send(new TextEncoder().encode('hello'))
await stream.closeWrite()

const chunks = []
stream.onMessage(data => chunks.push(data))
```

### LanetStream API

| 成员 | 说明 |
|---|---|
| `send(uint8Array)` | 写数据；返回 false 表示缓冲满，等 `raw` 的 drain 事件 |
| `onMessage(cb)` | 注册到达回调（Uint8Array，边界不保证，需累积） |
| `closeWrite()` | 半关闭写端，对端读到 EOF |
| `abort()` | 立即复位流 |
| `viaRelay` | 是否经中继（false = 直连） |
| `protocol` | 流上协商的协议号 |
| `raw` | 底层 libp2p MessageStream（事件式 API，进阶） |

## 工作原理

1. **发现中继**：从控制面 `/v1/relays/candidates` 拉取 relay 列表（tcp 地址自动补 `/ws`）；
2. **建立节点**：js-libp2p（WebSockets + WebRTC + Circuit Relay v2 兜底 + noise 加密 + yamux 复用；
   WebTransport/WebRTC 按运行环境自动启用，Node 下自动跳过）；
3. **入网**：凭邀请码调用 ctl `/v1/groups/join`，以浏览器节点自己的 PeerID 拿到虚拟 IP；
4. **通告**：把浏览器可达地址通告到控制面；
5. **开流**：解析 NetMap 后优先直拨对端通告地址（ws 优先，浏览器附加 webrtc-direct），
   全部失败经 relay 电路（`/p2p-circuit`）兜底，与 Go 节点同走 `/pvn/tunnel/1.0.0` 协议。

浏览器与 Go 节点之间的连接建立无需独立信令服务器：Go 侧启用 `webrtc-direct`
传输层（SDK / agent 的 `WebRTC: true`），relay 同时监听 tcp 与 ws。

## demo

```bash
cd sdk/web && npx serve .    # 打开 http://localhost:3000/demo/
```

demo 页面支持：入网、按虚拟 IP 对 Go 节点做 echo 往返测试、显示链路类型（直连/中继）与耗时。
联调建议先起一个 Go echo 节点：`go run ./app/agent/cmd/pvn-web-echo`（启动日志会打印邀请码）。

Node 环境快速验证（无需浏览器）：

```bash
node test/interop.mjs http://127.0.0.1:8000 <邀请码> <目标虚拟IP>
```

## 服务端要求

- ctl / relay 已部署（compose 即可）；relay 需开放 WebSocket（4001/tcp 已同时支持）；
- 浏览器页面若为 HTTPS，ctl 与 relay 需配 TLS（本地 http 联调无此要求）；
- ctl 已内置 CORS 放行（网页 SDK 跨域调用 join/netmap/announce）。

## 已验证链路（2026-09-04，本机实测）

| 路径 | 结果 |
|---|---|
| Node js-libp2p → Go 节点 ws 直连（127.0.0.1） | ✅ echo 往返 ~100ms |
| 入网 / NetMap / 邀请码 | ✅ |
| relay 电路兜底（circuit relay v2 over ws） | ✅ 可用（LAN 直连优先生效） |

已知限制：Go 节点处于 NAT 后（AutoNAT 判定不可达）时，libp2p 会在
`host.Addrs()` 中过滤未确认可达的 webrtc-direct 地址——此时浏览器经
relay 电路连接，公网部署（AutoNAT 确认可达）后 webrtc-direct 直连地址
会自动出现在通告中。
