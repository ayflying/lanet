/**
 * Node 互通验证：网页 SDK（js-libp2p）↔ Go 节点（lanet Go SDK）。
 *
 * 前置：本地已起 ctl(8000) + relay(4001) + Go echo 节点（见 app/agent/cmd/pvn-web-echo）。
 * 用法：node test/interop.mjs <ctlURL> <inviteCode> <targetVirtualIP>
 * 验证点：入网 / NetMap 解析 / 直连或 relay 电路建连 / 隧道流 echo 往返。
 */
import { createNode } from '../src/index.js'

const [, , ctlURL = 'http://127.0.0.1:8000', inviteCode, targetIP = '10.7.2.2'] = process.argv
if (!inviteCode) {
  console.error('用法: node test/interop.mjs <ctlURL> <inviteCode> <targetVirtualIP>')
  process.exit(1)
}

console.log(`[interop] ctl=${ctlURL} invite=${inviteCode} target=${targetIP}`)
const node = await createNode({ ctlURL, inviteCode, name: 'js-interop' })
console.log(`[interop] 入网成功 peer=${node.peerId} virtualIP=${node.virtualIP}`)

const netmap = await node.netmap()
console.log(`[interop] netmap members=${netmap.members.length}:`,
  netmap.members.map(m => `${m.name}(${m.virtual_ip})`).join(', '))

const message = `hello-from-js-${Date.now()}`
const t0 = Date.now()
const stream = await node.dial(targetIP)
console.log(`[interop] 流已建立 protocol=${stream.protocol} viaRelay=${stream.viaRelay} 耗时=${Date.now() - t0}ms`)

const chunks = []
stream.onMessage(data => chunks.push(data))
stream.send(new TextEncoder().encode(message))
await stream.closeWrite() // Go echo 读到 EOF 后回显并关闭

// echo 是 1:1 拷贝：累积到与发送等长即完成。
await new Promise((resolve, reject) => {
  const timer = setInterval(() => {
    const total = chunks.reduce((n, c) => n + c.length, 0)
    if (total >= message.length) { clearInterval(timer); resolve() }
  }, 50)
  setTimeout(() => { clearInterval(timer); reject(new Error('echo 超时（15s）')) }, 15000)
})

const reply = new TextDecoder().decode(concat(chunks)).slice(0, message.length)
const elapsed = Date.now() - t0
console.log(`[interop] echo 往返 ${elapsed}ms: 发送=${JSON.stringify(message)} 收到=${JSON.stringify(reply)}`)
if (reply !== message) {
  console.error('[interop] FAIL: 回显不一致')
  process.exit(1)
}
console.log('[interop] PASS ✓')
await node.close()
process.exit(0)

function concat (chunks) {
  const total = chunks.reduce((n, c) => n + c.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const c of chunks) { out.set(c, offset); offset += c.length }
  return out
}
