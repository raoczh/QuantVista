import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'

const storage = new Map()
globalThis.localStorage = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, String(value)), removeItem: key => storage.delete(key) }
globalThis.location = { origin: 'https://quantvista.test' }
const windowEvents = new EventTarget()
globalThis.window = Object.assign(windowEvents, { setInterval: () => 1, clearInterval() {}, focus() {} })
globalThis.document = new EventTarget()
Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { serviceWorker: new EventTarget() } })
const notifications = []
globalThis.Notification = class {
  static permission = 'granted'
  constructor(title) { this.title = title; notifications.push(this) }
  close() {}
}
window.Notification = Notification
function load(file, dependencies = {}) {
  const source = fs.readFileSync(new URL(file, import.meta.url), 'utf8')
  const output = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS } }).outputText
  const module = { exports: {} }
  new Function('require', 'module', 'exports', output)(name => {
    if (!(name in dependencies)) throw new Error(`未知测试依赖 ${name}`)
    return dependencies[name]
  }, module, module.exports)
  return module.exports
}
const tokens = load('../src/api/token.ts')
const tick = () => new Promise(setImmediate)
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done }); return { promise, resolve } }
const row = { delivery_id: 11, event: { id: 21, user_id: 1, title: '账户一通知', body: '本地测试', route: '/positions' } }
const failures = []
for (const action of ['stop', 'switch']) {
  notifications.length = 0
  tokens.setTokens('access-a', 'refresh-a')
  const pending = deferred(), messages = [], acknowledgments = []
  let owner = 1
  const runtime = load('../src/composables/useBrowserNotifications.ts', {
    vue, '@/api/token': tokens,
    '@/api/notify': { listBrowserNotificationEvents: () => pending.promise, ackBrowserNotification: (...args) => { acknowledgments.push(args); return Promise.resolve() } },
  }).useBrowserNotificationRuntime(() => owner, { push() {} }, { info: value => messages.push(value) })
  runtime.start()
  if (action === 'stop') runtime.stop()
  else { owner = 2; tokens.setTokens('access-b', 'refresh-b') }
  pending.resolve([row])
  await tick()
  runtime.stop()
  if (notifications.length || messages.length || acknowledgments.length) failures.push({ action, notifications: notifications.length, messages: messages.length, acknowledgments: acknowledgments.length })
}
notifications.length = 0
tokens.setTokens('access-a', 'refresh-a')
const messages = [], routes = []
const runtime = load('../src/composables/useBrowserNotifications.ts', {
  vue, '@/api/token': tokens,
  '@/api/notify': { listBrowserNotificationEvents: async () => [], ackBrowserNotification: async () => {} },
}).useBrowserNotificationRuntime(() => 1, { push: value => routes.push(value) }, { info: value => messages.push(value) })
runtime.start()
navigator.serviceWorker.dispatchEvent(new MessageEvent('message', { data: { type: 'qv-browser-notification', payload: { user_id: 2, title: '其他账户通知', route: '/positions', focus: true } } }))
runtime.stop()
if (messages.length || routes.length) failures.push({ action: 'foreign_worker', messages: messages.length, routes: routes.length })
assert.deepEqual(failures, [], '停止轮询或切换会话后不得展示、确认或导航旧通知')

// 正常通知仍须展示并确认；点击已展示通知时也必须复验原会话。
notifications.length = 0
tokens.setTokens('access-valid', 'refresh-valid')
const validMessages = [], validRoutes = [], validAcks = []
const valid = load('../src/composables/useBrowserNotifications.ts', {
  vue, '@/api/token': tokens,
  '@/api/notify': { listBrowserNotificationEvents: async () => [row], ackBrowserNotification: async (...args) => { validAcks.push(args) } },
}).useBrowserNotificationRuntime(() => 1, { push: value => validRoutes.push(value) }, { info: value => validMessages.push(value) })
valid.start()
await tick()
assert.equal(notifications.length, 1)
assert.deepEqual(validMessages, [row.event.title])
assert.equal(validAcks.length, 1)
notifications[0].onclick()
assert.deepEqual(validRoutes, ['/positions'])
tokens.setTokens('another-login', 'another-refresh')
notifications[0].onclick()
navigator.serviceWorker.dispatchEvent(new MessageEvent('message', { data: { type: 'qv-browser-notification', payload: { user_id: 1, title: '旧会话推送', route: '/alerts', focus: true } } }))
assert.deepEqual(validRoutes, ['/positions'], '旧会话创建的通知点击和推送消息不能导航新会话')
assert.deepEqual(validMessages, [row.event.title])
valid.stop()
console.log('浏览器通知停止、会话隔离与推送归属回归通过')
