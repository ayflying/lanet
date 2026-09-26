/**
 * H5 SDK 互通验证。
 *
 * 两种模式：
 *   1) 不带参数 —— 只跑**分帧工具自测**（离线，不需要网络与 ctl）：
 *        node test/interop.mjs
 *   2) 带参数 —— 全链路：入网 → NetMap → requestText/requestJSON 真实往返：
 *        node test/interop.mjs <ctlURL> <inviteCode> <targetVirtualIP>
 *
 * 前置（模式 2）：ctl(8000) + relay(4001) + Go echo 节点
 *   go run ./app/agent/cmd/pvn-web-echo
 */

import { connect, encodeFrame, FrameDecoder, PROTOCOL_TUNNEL, VERSION } from '../src/index.js'

const [, , ctlURL = 'http://127.0.0.1:8000', inviteCode, targetIP] = process.argv

let failed = 0
function check (name, ok, detail = '') {
  console.log(`  ${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  ' + detail : ''}`)
  if (!ok) failed++
}

// ------------------------------------------------------------------
// 一、分帧工具自测（离线）
// ------------------------------------------------------------------
console.log(`\n[interop] H5 SDK v${VERSION}  协议=${PROTOCOL_TUNNEL}`)
console.log('\n[1] 分帧工具自测（encodeFrame / FrameDecoder）')

{
  const frames = ['第一帧', 'x'.repeat(5000), '尾帧']
  const wire = []
  for (const f of frames) wire.push(encodeFrame(f))
  const stream = concat(wire)

  // 场景 A：任意切分（模拟 libp2p 把一条消息拆成多次 message 事件）
  const got = []
  const decoder = new FrameDecoder(f => got.push(new TextDecoder().decode(f)))
  for (let i = 0; i < stream.length; i += 7) decoder.push(stream.subarray(i, i + 7))
  check('半包/粘包按条切分', got.length === frames.length && got.every((v, i) => v === frames[i]),
    `收到 ${got.length} 帧`)

  // 场景 B：整块喂入
  const got2 = []
  const d2 = new FrameDecoder(f => got2.push(new TextDecoder().decode(f)))
  d2.push(stream)
  check('整块喂入', got2.length === frames.length && d2.pending === 0)

  // 场景 C：残留半帧时 pending 正确
  const d3 = new FrameDecoder(() => {})
  d3.push(encodeFrame('hello').subarray(0, 6))
  check('半帧残留计数', d3.pending === 6, `pending=${d3.pending}`)
  d3.reset()
  check('reset 清空残留', d3.pending === 0)

  // 场景 D：超长帧应被拒绝（防对端塞一个巨大长度撑爆内存）
  let threw = false
  try {
    const d4 = new FrameDecoder(() => {}, { maxFrameSize: 16 })
    const bad = new Uint8Array(4 + 32)
    new DataView(bad.buffer).setUint32(0, 32, false)
    d4.push(bad)
  } catch { threw = true }
  check('超长帧拒绝', threw)
}

if (!inviteCode) {
  console.log('\n[interop] 未提供邀请码，只跑了分帧自测。全链路用法：')
  console.log('  node test/interop.mjs <ctlURL> <inviteCode> <targetVirtualIP>')
  process.exit(failed ? 1 : 0)
}

// ------------------------------------------------------------------
// 二、全链路（需要 ctl + relay + Go echo 节点）
// ------------------------------------------------------------------
console.log(`\n[2] 全链路  ctl=${ctlURL} target=${targetIP}`)

// retries：入网后同群成员要等下一轮 NetMap 刷新（Go SDK 默认 15s）才能解析出本节点的虚拟 IP，
// 在此之前发去的入向流会因「来源未知」被对端防火墙复位 —— 这不是链路故障，重试即可穿透。
// retryDelay 取得比对端 NetMap 刷新周期（Go SDK 默认 15s）更宽裕，让重试能真正跨过刷新点；
// retryOnTimeout 对 echo 这种幂等请求是安全的（超时也不怕重发）。
const node = await connect({
  ctlURL,
  inviteCode,
  name: 'h5-interop',
  retries: 6,
  retryDelay: 4000,
  requestTimeout: 8000,
  retryOnTimeout: true
})
console.log(`  入网成功 peerId=${node.peerId} virtualIP=${node.virtualIP}`)
check('入网拿到虚拟 IP', !!node.virtualIP, node.virtualIP)

const netmap = await node.netmap()
console.log('  NetMap:', (netmap?.members ?? []).map(m => `${m.name}(${m.virtual_ip})`).join(', '))
check('NetMap 非空', (netmap?.members ?? []).length > 0, `${netmap?.members?.length ?? 0} 个成员`)

if (!targetIP) {
  console.log('  （未提供 targetVirtualIP，跳过回流测试）')
} else {
  // requestText：封装层内部完成「发送 → 半关闭 → 等对端 EOF → 收集响应」
  // （若首连撞上 NetMap 未同步，retries 会自动兜住，最终仍应 PASS）
  {
    const msg = `h5-hello-${Date.now()}`
    const t0 = Date.now()
    let viaRelay = null
    const reply = await node.requestText(targetIP, msg, { onStream: s => { viaRelay = s.viaRelay } })
    check('requestText 往返一致', reply === msg,
      `${Date.now() - t0}ms 链路=${viaRelay ? '中继' : '直连'} 回显=${JSON.stringify(reply.slice(0, 40))}`)
  }

  // requestJSON：对象进出
  {
    const payload = { cmd: 'ping', ts: Date.now() }
    const t0 = Date.now()
    const reply = await node.requestJSON(targetIP, payload)
    const ok = reply && reply.cmd === payload.cmd && reply.ts === payload.ts
    check('requestJSON 对象往返', !!ok, `${Date.now() - t0}ms 响应=${JSON.stringify(reply)?.slice(0, 60)}`)
  }
}

await node.close()
console.log(`\n[interop] ${failed ? `存在 ${failed} 项失败` : '全部通过'}`)
process.exit(failed ? 1 : 0)

function concat (chunks) {
  const total = chunks.reduce((n, c) => n + c.length, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const c of chunks) { out.set(c, offset); offset += c.length }
  return out
}
