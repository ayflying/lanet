/**
 * ws-gateway 互通测试（Node 运行环境）。
 *
 * 前置：本地已启动 ctl / relay / gateway / go-service 节点 / TCP 回显 9999。
 * 用法：node test/gateway.mjs <ctlURL> <inviteCode> <targetVirtualIP>
 */
import { createGatewayClient } from '../src/index.js'
import { encodeFrame } from '../src/frame.js'
import { WebSocket } from 'ws'

const [, , ctlURL = 'http://127.0.0.1:8000', invite = '', target = ''] = process.argv
if (!invite || !target) {
  console.error('用法: node test/gateway.mjs <ctlURL> <inviteCode> <targetVirtualIP>')
  process.exit(2)
}

/** Node ws 适配（生产环境无需此步：uniapp/H5 自动适配）。 */
function nodeSocketFactory (url) {
  const sock = new WebSocket(url)
  const listeners = { message: [], close: [], error: [] }
  let opened = false
  const queue = []
  sock.onopen = () => {
    opened = true
    queue.splice(0).forEach(bytes => sock.send(bytes))
  }
  sock.onmessage = e => listeners.message.forEach(cb => cb(e.data))
  sock.onclose = () => listeners.close.forEach(cb => cb())
  sock.onerror = e => listeners.error.forEach(cb => cb(e))
  return {
    send: bytes => { if (opened) sock.send(bytes); else queue.push(bytes) },
    close: () => sock.close(),
    onMessage: cb => listeners.message.push(cb),
    onClose: cb => listeners.close.push(cb),
    onError: cb => listeners.error.push(cb)
  }
}

function assert (cond, label) {
  if (!cond) {
    console.error(`FAIL: ${label}`)
    process.exit(1)
  }
  console.log(`PASS: ${label}`)
}

async function main () {
  // 1. 鉴权入网（client 模式）。
  const client = await createGatewayClient({
    url: 'ws://127.0.0.1:8700/gateway',
    inviteCode: invite,
    name: 'node-test-client',
    socketFactory: nodeSocketFactory
  })
  assert(client.info().virtualIP.startsWith('100.64.'),
    `鉴权通过（经网关身份入群 group=${client.info().group} gw_ip=${client.info().virtualIP}）`)

  // 2. 自定义协议直开流：对 go-service 节点的 /pvn/tunnel/1.0.0（echo）。
  const echoStream = await client.dialProtocol(target, '/pvn/tunnel/1.0.0')
  const t0 = Date.now()
  const sent = new TextEncoder().encode('hello from gateway client')
  echoStream.write(sent)
  echoStream.closeWrite()
  const echoed = await echoStream.readAll()
  assert(new TextDecoder().decode(echoed) === 'hello from gateway client',
    `自定义协议 echo 往返 ${Date.now() - t0}ms（viaRelay=${echoStream.viaRelay}）`)

  // 3. 端口转发：目标节点本机 9999 的 TCP 回显服务。
  const pf = await client.dial(target, 9999)
  const t1 = Date.now()
  pf.write(new TextEncoder().encode('portfwd works'))
  pf.closeWrite()
  const back = await pf.readAll()
  assert(new TextDecoder().decode(back) === 'portfwd works',
    `PortFWD -> ${target}:9999 TCP 回显往返 ${Date.now() - t1}ms（viaRelay=${pf.viaRelay}）`)

  // 4. 心跳。
  const pong = await new Promise(resolve => {
    const orig = client._handleFrame.bind(client)
    client._handleFrame = f => {
      if (f.type === 0x0b) return resolve(true)
      return orig(f)
    }
    client._send(encodeFrame(0x0a, 0, 'ping'))
    setTimeout(() => resolve(false), 3000)
  })
  assert(pong, '心跳 Ping/Pong')

  client.close()
  console.log('\n=== ws-gateway 全链路 PASS ===')
  process.exit(0)
}

main().catch(err => {
  console.error('FAIL:', err.message)
  process.exit(1)
})
