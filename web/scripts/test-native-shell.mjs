import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const source = fs.readFileSync(path.join(root, 'src/lib/nativeShell.ts'), 'utf8')
const output = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const listeners = new Map()
const routes = []
const app = {
  addListener: async (event, callback) => { listeners.set(event, callback) },
  getLaunchUrl: async () => ({ url: 'https://quantvista.test/positions?position_id=7' }),
}
const module = { exports: {} }
new Function('require', 'module', 'exports', output)((name) => {
  assert.equal(name, '@capacitor/app')
  return { App: app }
}, module, module.exports)
const { nativeLinkRoute, setupNativeShell } = module.exports
const origin = 'https://quantvista.test'
assert.equal(nativeLinkRoute('https://outside.test/positions', origin), null, '外部域名不能变成站内导航')
assert.equal(nativeLinkRoute('quantvista://oauth/callback-evil?code=x', origin), null)
assert.equal(nativeLinkRoute('quantvista://oauth/callback', origin), null)
assert.equal(nativeLinkRoute('quantvista://oauth/callback?code=a%2Bb', origin), '/login/callback?mode=mobile-exchange&code=a%2Bb')
assert.equal(nativeLinkRoute(`${origin}/positions?position_id=7#details`, origin), '/positions?position_id=7#details')

globalThis.location = { origin }
const originalNow = Date.now
let now = 10000
Date.now = () => now
try {
  await setupNativeShell({ push: (route) => { routes.push(route) } })
  const open = listeners.get('appUrlOpen')
  const url = `${origin}/positions?position_id=7`
  open({ url })
  assert.equal(routes.length, 1, '冷启动与热启动的同次投递只导航一次')
  now += 3000
  open({ url })
  assert.equal(routes.length, 2, '稍后再次点击相同通知必须能重新导航')
  open({ url: 'https://outside.test/positions' })
  assert.equal(routes.length, 2)
} finally {
  Date.now = originalNow
}
console.log('移动深链边界与重复通知导航回归通过')
