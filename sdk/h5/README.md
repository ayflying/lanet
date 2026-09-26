# @lanet/sdk-h5

Lanet **H5 直引包**：把整套 js-libp2p P2P 能力打成一个 js 文件，H5 页面
`<script>` 一引就成为一个**真正的网络成员**——不是 HTTP 网关中转，而是直连组内节点。

```html
<script src="lanet-h5.min.js"></script>
<script>
  const node = await Lanet.connect({
    ctlURL: 'https://ctl.example.com',
    inviteCode: 'grp-xxxxxxxx'
  })
  console.log('虚拟 IP =', node.virtualIP)

  // 请求-响应一行式（内部处理半关闭与 EOF 界定）
  const reply = await node.requestText('10.7.0.2', 'hello')
</script>
```

免 npm、免打包器、免 importmap。**652KB** 单文件（含 noise + yamux + ws + WebRTC + WebTransport + Circuit Relay）。

## 与 Web SDK 的关系

| | [@lanet/sdk-web](../web/README.md) | **@lanet/sdk-h5**（本包） |
|---|---|---|
| 形态 | ESM 源码包，依赖外部提供 | **单文件产物**（IIFE + ESM 两种） |
| 用法 | npm 安装 + 打包器，或 importmap 指 CDN | `<script>` 直接引 / 原生 `import` |
| 底层 P2P | js-libp2p 建节点、入网、开流、relay 兜底 | **完全复用 Web SDK**（同一份实现，不重造） |
| 增量 | — | 请求-响应一行式、流分帧、超时、页面卸载自动下线 |

一句话：**H5 SDK = Web SDK + 打包产物 + H5 便利层**。底层逻辑只有一份实现，
改 P2P 行为改 `sdk/web`，本包重新构建即可。

## 构建

本包**不发布 dist，由仓库现场构建**（与 Android 的 `lanet.aar` 同理）。

```bash
cd sdk/h5
npm install       # 只需装 esbuild；libp2p 依赖复用 sdk/web/node_modules
npm run build     # 产出 dist/，约 1 秒
```

产物：

| 文件 | 说明 |
|---|---|
| `dist/lanet-h5.min.js` | IIFE + 压缩，`<script src>` 直接用，全局 `window.Lanet` |
| `dist/lanet-h5.esm.js` | 浏览器原生 ESM（依赖已内联，无需 importmap），带 sourcemap |

前置：`sdk/web` 自己要 `npm install` 过（本包从它的 `node_modules` 复用 libp2p 依赖，
避免同一套 200+ 包装两遍）。脚本会在缺前置时给出明确提示而不是抛晦涩错误。

## 快速开始

### 最小骨架

```html
<!DOCTYPE html>
<html>
<body>
<button id="go">入网并请求</button>
<pre id="out"></pre>
<script src="lanet-h5.min.js"></script>
<script>
  document.getElementById('go').onclick = async () => {
    const out = document.getElementById('out')
    try {
      const node = await Lanet.connect({
        ctlURL: 'https://ctl.example.com',
        inviteCode: 'grp-xxxxxxxx',
        name: 'h5-demo'
      })
      out.textContent = `已入网 虚拟IP=${node.virtualIP}\n`

      const reply = await node.requestText('10.7.0.2', 'hello')
      out.textContent += `回显: ${reply}\n`
    } catch (err) {
      out.textContent += '失败: ' + err.message
    }
  }
</script>
</body>
</html>
```

### 用 ESM（Vite / webpack / 原生 module）

```js
import { connect, FrameDecoder, encodeFrame } from './lanet-h5.esm.js'
// 或仓库内开发时直接引源码（需 importmap 或打包器解析 @lanet/sdk-web）：
// import { connect } from '@lanet/sdk-h5'
```

## API

### `connect(options) → Promise<H5Node>`

| 选项 | 类型 | 必填 | 默认 | 说明 |
|---|---|:---:|---|---|
| `ctlURL` | string | ✅ | — | 控制面地址，如 `https://ctl.example.com` |
| `inviteCode` | string | ✅ | — | 邀请码（网页节点不支持建群） |
| `name` | string | — | `h5-xxxxxx` | NetMap 中显示的节点名 |
| `relayAddrs` | string[] | — | 自动发现 | 显式指定 relay multiaddr |
| `onStream` | Function | — | — | 入向流回调（等价于入网后 `node.onStream`） |
| `autoClose` | boolean | — | `true` | 页面 `pagehide` 时自动下线 |
| `requestTimeout` | number | — | `15000` | `request` 系列默认超时（毫秒） |
| `retries` | number | — | `0` | `request` 系列默认重试次数（见「首连失败与重试」） |
| `retryDelay` | number | — | `2000` | 重试间隔（毫秒） |
| `retryOnTimeout` | boolean | — | `false` | 超时是否也重试（仅建议对幂等请求开启） |
| `allowPrivateAddresses` | boolean | — | `false` | 允许拨内网地址（见「浏览器沙箱」） |
| `allowInsecureWebSockets` | boolean | — | `false` | 允许拨明文 `ws://`（见「浏览器沙箱」） |
| `connectionGater` | object | — | — | 完全自定义拨号闸门（高级用法，传入即接管默认策略） |

### `H5Node`

| 成员 | 返回 | 说明 |
|---|---|---|
| `peerId` / `virtualIP` / `group` | — | 本节点身份与所入群组 |
| `request(ip, payload, opts?)` | `Promise<Uint8Array>` | **请求-响应**：发完半关闭，等对端 EOF 后返回全部响应字节 |
| `requestText(ip, text, opts?)` | `Promise<string>` | 同上，UTF-8 文本进出 |
| `requestJSON(ip, obj, opts?)` | `Promise<unknown>` | 同上，JSON 进出（响应空则 `null`） |
| `sendText(ip, text, opts?)` | `Promise<void>` | 单向发送，不等响应 |
| `dial(ip, opts?)` | `Promise<LanetStream>` | 低层开流（要自定义分帧/多消息时用） |
| `resolve(ip)` | `Promise<NetMapMember\|null>` | 虚拟 IP → 成员信息 |
| `netmap()` | `Promise<{members}>` | 拉取群组成员目录 |
| `onStream(handler)` | `this` | 注册入向流回调（可多个） |
| `close()` | `Promise<void>` | 关闭节点（幂等，页面卸载会自动调用） |
| `web` | — | 底层 Web SDK 节点（进阶） |

`request` 系列的 `opts.onStream(stream)` 在流建立后回调，可读 `stream.viaRelay`
判断本次是**直连**还是**经中继**（demo 里用它显示链路）。

### 分帧工具

libp2p 流是**字节流，没有消息边界**——一次 `send` 可能被对面拆成多次 `message`
事件，也可能多条粘在一起。约定 4 字节大端长度前缀即可按条收发：

```js
// 发送侧
stream.send(encodeFrame({ cmd: 'ping' }))

// 接收侧：喂入任意切分的块，每凑齐一帧回调一次
const decoder = new FrameDecoder(frame => console.log('收到一帧', frame))
stream.onMessage(chunk => decoder.push(chunk))
```

| 成员 | 说明 |
|---|---|
| `encodeFrame(payload)` | 4 字节大端长度前缀编码（string / Uint8Array / ArrayBuffer 均可） |
| `new FrameDecoder(onFrame, {maxFrameSize})` | 增量解码，默认单帧上限 16MB（超过抛错，防对端塞超大长度撑爆内存） |
| `decoder.push(chunk)` | 喂入任意切分的块 |
| `decoder.reset()` / `decoder.pending` | 清空残留 / 当前残留字节数 |

## 请求-响应是怎么界定的

`request` 内部四步：**开流 → 发送 → 半关闭写端 → 等对端写完后收尾**。

「对端写完了」在 libp2p 3.x 里对应的事件是 **`remoteCloseWrite`**（对端半关闭了写端，
Go 侧 `CloseWrite` / `io.Copy` 收尾即此形态）。

⚠️ **不能用 `close` 代替**：`close` 在本端调用 `closeWrite()` 之后、或对端复位流时
**都会触发**，把它当成「对端写完了」会在数据还没回来时就收尾 —— 本包开发期实测到过
「59ms 就返回空响应」正是这个原因。同理 `reset` 是对端复位流，属异常路径。

因此 `request` 适用于**一问一答、响应以 EOF 结束**的服务（最常见，Go 侧 `io.Copy` echo 即此形态）。
若对端在同一流上回多条消息且不关写端，请改用 `dial` + `FrameDecoder`，或直接用 `dial` 自己控制。

超时（默认 15s）是必须的兜底：对端不回写时若不超时，Promise 会永久挂住。

## 首连失败与重试（NetMap 时序）

刚入网时，**同群其他成员要等下一轮 NetMap 刷新才能解析出你的虚拟 IP**
（Go SDK 默认 15s 一轮）。在那之前你发去的入向流会因「来源未知」被对端防火墙复位
（对端 Go 日志形如 `onstream 拒绝：来源=（…）不在放行规则内`），`dial` 也可能因
解析不出目标地址而失败。

这是**时序现象，不是链路故障**，重试即可穿透：

```js
const node = await Lanet.connect({
  ctlURL, inviteCode,
  retries: 5,           // 失败后重试次数（默认 0）
  retryDelay: 5000,     // 间隔；比对端 NetMap 刷新周期（15s）取宽裕些更稳
  requestTimeout: 8000,
  retryOnTimeout: true  // echo 这类幂等请求可以连超时一起重试
})
```

重试默认只覆盖「**请求确定没被对端处理**」的失败（拨号失败 / 写不进去 / 流被复位），
重发一定安全。**超时默认不重试** —— 对端可能已经处理完，重发会让副作用重复，
确认幂等再开 `retryOnTimeout`。重试前本包会主动断开与目标的连接，避免在
「被复位后已半死」的连接上反复超时。

## 能力与边界

| 能力 | 支持 | 说明 |
|---|---|---|
| 入网拿到虚拟 IP、与 Go 节点互开流 | ✅ | 与 Go SDK 的 `Dial` / `OnStream` 对等，同走 `/pvn/tunnel/1.0.0` |
| 直连优先、中继兜底 | ✅ | WebRTC / WebTransport 直连，Circuit Relay v2 兜底 |
| 访问组内 TCP 服务 | ✅* | 需对端 Go 节点运行 PortFWD 桥接 |
| 内网部署（拨私网 / 明文 ws） | ✅* | 需显式开 `allowPrivateAddresses` / `allowInsecureWebSockets`（见「部署要求」） |
| 请求-响应 / 分帧 | ✅ | 本包增量能力 |
| **ping 虚拟 IP / TUN** | ❌ | 浏览器沙箱无原始 IP 包能力，一切访问只能按「流」进行 |
| 建群 | ❌ | 网页节点凭邀请码加入（建群走 Go SDK / CLI） |
| 身份持久化 | ❌ | 每次刷新页面是**新 PeerID**（无本地密钥存储设计，避免 H5 场景引入额外信任问题） |

## 部署要求

与 [Web SDK](../web/README.md) 完全一致：

- ctl / relay 已部署（relay 需开放 WebSocket，4001/tcp 已同时支持）；
- ctl 内置 CORS 放行；
- **页面为 HTTPS 时 ctl / relay 也必须 HTTPS/WSS**（浏览器混合内容策略），
  本地 `http://localhost` 联调无此要求；
- WebTransport 直连要求对端 TLS 证书有效；仅 WebSocket 链路无此要求。

### 浏览器沙箱：默认只允许「公网 + wss」

浏览器内核有一道默认闸门（libp2p 的 `connection-gater.browser.js`）：**拒绝拨明文
`ws://`、拒绝拨内网地址**。前者防止 https 页面降级到不安全连接，后者与浏览器的
Private Network Access 策略呼应。

⚠️ **注意构建条件**：本包用 esbuild 以 `platform: 'browser'` 打包，会解析到**上面这份
浏览器闸门**（Node 直跑源码时用的是「全放行」的另一份）。所以**用 Node 跑源码验证通过，
不代表浏览器里能通** —— 判据要对齐真实浏览器行为。

按部署形态二选一：

- **公网 https 部署**：ctl 用 `https://`、relay 暴露 `wss://` 且用公网可解析地址，
  按默认策略即可（推荐）。
- **内网 / http 部署**（H5 页面自身就在内网、或页面走 http）：浏览器**实际允许**
  `ws://` 与内网地址，但需要显式开豁免，否则入网后**所有拨号都会被拦下**
  （日志表现为 `DialDeniedError: The connection gater denied all addresses in the dial request`）：

  ```js
  const node = await Lanet.connect({
    ctlURL, inviteCode,
    allowPrivateAddresses: true,    // 允许拨内网地址
    allowInsecureWebSockets: true   // 允许拨明文 ws://
  })
  ```

两个开关默认 `false`，**别无脑打开**：公网页面开着会失去混合内容保护。需要完全自控时
可传 `connectionGater` 直接接管（传入即覆盖默认策略，需自带 `denyDialMultiaddr`）。

## 联调与验证

```bash
# 1. 起三件套（另开三个终端）
go run ./app/ctl                                          # 控制面 :8000
PVN_RELAY_CTL=http://127.0.0.1:8000 go run ./app/relay     # 中继 :4001
go run ./app/agent/cmd/pvn-web-echo                        # echo 节点（打印邀请码）

# 2a. 分帧工具自测（离线，不需要上面任何服务）
cd sdk/h5 && node test/interop.mjs

# 2b. 全链路（入网 + NetMap + 请求-响应真实往返）
node test/interop.mjs http://127.0.0.1:8000 <邀请码> <echo节点虚拟IP>

# 2c. 浏览器 demo（两种）
npx serve .        # 打开 /demo/index.html（用 dist）或 /demo/importmap.html（零构建走 CDN）
```

demo 页支持：入网、`requestText` / `requestJSON`、低层 `dial`（显示链路与协议号）、
NetMap 成员列表；`dist` 不存在时页面会直接告诉你该跑什么命令，不会静默失败。

## 已知限制与排查

- **`request` 超时**：对端没回写、或回写后没关写端。先确认对端是「一问一答 + EOF」形态，
  否则改用 `dial` + `FrameDecoder`；再看 `opts.onStream` 拿到的 `viaRelay` 判断链路。
- **`send` 返回 false 后继续发**：数据可能丢。必须等 `raw` 的 `drain`（本包的 `request`
  已内置背压处理）。
- **`onMessage` 收到的数据不完整 / 粘包**：见上文分帧工具，别自己拼字节。
- **入网报 CORS / mixed content**：见「部署要求」。
- **`dist` 不存在**：`cd sdk/h5 && npm install && npm run build`；
  demo 页会显式提示，或改用 `demo/importmap.html`（零构建，依赖走 esm.sh CDN）。
- **`FrameDecoder` 抛「帧长度超过上限」**：协议不匹配（对端发的不是本包的分帧格式）
  或对端异常。确认对端也用 `encodeFrame`。
- **所有拨号都被拒（`DialDeniedError: … connection gater denied all addresses`）**：
  撞上浏览器默认闸门（拒明文 `ws://` + 拒内网地址）。按部署形态决定是改走 `wss` /
  公网地址，还是显式开豁免 —— 见「部署要求 · 浏览器沙箱」。
- **刚入网第一次请求就失败（被复位 / 拨号失败）**：对端 NetMap 还没刷新，见
  「首连失败与重试」，配 `retries` 即可。

## 与其它 SDK 的选择

| 我要… | 用哪个 |
|---|---|
| H5 页面/静态站点，不想装 npm | **本包**（`<script>` 直引） |
| 前端工程（Vite/webpack），要 tree-shaking | [@lanet/sdk-web](../web/README.md) |
| uni-app 编译到 H5 | 本包（uni-app 的 H5 产物就是网页） |
| 小程序 / App（跑不了 libp2p） | [@lanet/sdk-uniapp](../uniapp/README.md)（经 ws-gateway） |
| 让手机**真入网**（可 ping 虚拟 IP） | [Android 原生插件](../android-plugin/README.md) |
| 后端服务互联 | [Go SDK](../go/lanet/README.md) |
