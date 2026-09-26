/**
 * 把 sdk/h5 打成 H5 页面可直接引用的单文件产物。
 *
 *   node build.mjs            # 产出 dist/
 *   node build.mjs --watch    # 监听源码变化，改动即重建
 *
 * 产物：
 *   dist/lanet-h5.min.js    IIFE + 压缩，`<script src>` 直接用，全局 `window.Lanet`
 *   dist/lanet-h5.esm.js    浏览器原生 ESM（依赖已内联，无需 importmap）
 *
 * 依赖解析策略（避免第二份实现、也避免重复安装）：
 *   - `@lanet/sdk-web` 裸模块名 alias 到 `../web/src/index.js` —— H5 层与 Web SDK 永远同源；
 *   - libp2p 全家桶从 `sdk/web/node_modules` 解析（本包不重复安装那 200 多个包）。
 *
 * 前置：`npm install`（只需装 esbuild 一个包）。
 */

import { build, context } from 'esbuild'
import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const WEB_DIR = path.resolve(HERE, '../web')
const WEB_ENTRY = path.resolve(WEB_DIR, 'src/index.js')
const WEB_MODULES = path.resolve(WEB_DIR, 'node_modules')
const OUT_DIR = path.resolve(HERE, 'dist')

const pkg = JSON.parse(fs.readFileSync(path.resolve(HERE, 'package.json'), 'utf8'))
const BANNER = `/*! ${pkg.name} v${pkg.version} | MIT | Lanet H5 SDK：单文件直引包，让 H5 页面成为 P2P 网络成员 */`

/** 共通配置：浏览器目标的 ESM 打包，依赖从 sdk/web 复用。 */
function baseConfig (format, { minify, sourcemap }) {
  return {
    entryPoints: [path.resolve(HERE, 'src/index.js')],
    bundle: true,
    format,
    target: ['es2020'],
    platform: 'browser',
    mainFields: ['browser', 'module', 'main'],
    // 复用 Web SDK 的 node_modules：本包不重复安装 libp2p 依赖链
    nodePaths: [WEB_MODULES],
    // H5 层与 Web SDK 同源，不复制第二份实现
    alias: { '@lanet/sdk-web': WEB_ENTRY },
    define: { 'process.env.NODE_ENV': '"production"' },
    legalComments: 'none',
    logLevel: 'warning',
    minify,
    sourcemap,
    banner: { js: BANNER }
  }
}

const TARGETS = [
  {
    label: 'IIFE（<script> 直接引，全局 window.Lanet）',
    config: {
      ...baseConfig('iife', { minify: true, sourcemap: false }),
      globalName: 'Lanet',
      outfile: path.join(OUT_DIR, 'lanet-h5.min.js')
    }
  },
  {
    label: 'ESM（浏览器原生 import，依赖已内联）',
    config: {
      ...baseConfig('esm', { minify: false, sourcemap: true }),
      outfile: path.join(OUT_DIR, 'lanet-h5.esm.js')
    }
  }
]

function checkEnvironment () {
  if (!fs.existsSync(WEB_ENTRY)) {
    fail(`找不到 Web SDK 源码：${WEB_ENTRY}\n` +
      '本包必须放在本仓库的 sdk/h5 下构建（它复用 sdk/web 的实现与依赖）。')
  }
  if (!fs.existsSync(WEB_MODULES)) {
    fail(`找不到 ${WEB_MODULES}\n` +
      '请先在 sdk/web 执行 `npm install`（H5 打包复用它的 libp2p 依赖）。')
  }
  const esbuildDir = path.resolve(HERE, 'node_modules/esbuild')
  if (!fs.existsSync(esbuildDir)) {
    fail('缺少 esbuild。请在 sdk/h5 执行 `npm install`（只需装这一个包）。')
  }
}

function fail (message) {
  console.error(`\n[lanet-h5] 构建中止：${message}\n`)
  process.exit(1)
}

function sha16 (file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex').slice(0, 16)
}

function report () {
  console.log('\n[lanet-h5] 产物：')
  for (const { config } of TARGETS) {
    const out = config.outfile
    if (!fs.existsSync(out)) continue
    const size = (fs.statSync(out).size / 1024).toFixed(0)
    console.log(`   ${path.relative(HERE, out).replace(/\\/g, '/')}  ${size}KB  sha256=${sha16(out)}`)
  }
  console.log('\n   H5 页面用法：')
  console.log('     <script src="lanet-h5.min.js"></script>')
  console.log('     <script>Lanet.connect({ ctlURL, inviteCode }).then(n => ...)</script>')
  console.log('   或：import { connect } from "./lanet-h5.esm.js"')
  console.log('')
}

async function main () {
  checkEnvironment()
  const watch = process.argv.includes('--watch')

  fs.rmSync(OUT_DIR, { recursive: true, force: true })
  fs.mkdirSync(OUT_DIR, { recursive: true })

  if (watch) {
    const contexts = []
    for (const { label, config } of TARGETS) {
      const ctx = await context({
        ...config,
        plugins: [{
          name: 'report',
          setup (b) {
            b.onEnd(result => {
              if (result.errors.length) {
                console.error(`[lanet-h5] ${label} 构建失败`)
              } else {
                report()
              }
            })
          }
        }]
      })
      await ctx.watch()
      contexts.push(ctx)
    }
    console.log('[lanet-h5] 监听中（Ctrl+C 退出）')
    return
  }

  for (const { config } of TARGETS) {
    await build(config)
  }
  report()
}

main().catch(err => {
  console.error(err)
  process.exit(1)
})
