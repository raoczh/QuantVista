import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'
import vm from 'node:vm'

const storage = new Map()
globalThis.localStorage = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, String(value)), removeItem: key => storage.delete(key) }
globalThis.location = { origin: 'https://quantvista.test' }
const windowEvents = new EventTarget()
globalThis.window = Object.assign(windowEvents, { setInterval: () => 1, clearInterval() {}, setTimeout: () => 2, clearTimeout() {}, focus() {} })
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

// 首次授权时 Vue computed 不能永久缓存原来的 default 权限；隐藏页面仍须
// 调用系统通知，只有系统展示失败时才保留待确认事件。
storage.clear()
Notification.permission = 'default'
notifications.length = 0
tokens.setTokens('new-permission', 'refresh-permission')
let ackAttempts = 0
const permissionMessages = []
const permissionModule = load('../src/composables/useBrowserNotifications.ts', {
  vue, '@/api/token': tokens,
  '@/api/notify': { listBrowserNotificationEvents: async () => [row], ackBrowserNotification: async () => { if (++ackAttempts === 1) throw new Error('网络暂不可用') } },
})
const granted = permissionModule.useBrowserNotificationRuntime(() => 1, { push() {} }, { info: v => permissionMessages.push(v) })
granted.start()
assert.equal(granted.enabled.value, false)
document.visibilityState = 'hidden'
Notification.permission = 'granted'
permissionModule.refreshBrowserNotificationRuntime()
await tick()
assert.equal(granted.enabled.value, true, '首次授权立即生效，无需刷新网页')
assert.equal(notifications.length, 1, '在桌面/非激活页签时仍调用系统通知')
assert.equal(permissionMessages.length, 0, '隐藏页面不以站内 toast 冒充系统弹窗')
await granted.poll()
assert.equal(ackAttempts, 2)
assert.equal(notifications.length, 1, '回执失败重试不能重复弹窗')
granted.stop()
document.visibilityState = 'visible'

// 注册 promise 返回安装中的 Worker 时，必须等待 activated 后才能 Push 订阅。
const worker = Object.assign(new EventTarget(), { state: 'installing' })
navigator.serviceWorker.register = async () => ({ installing: worker })
let activated = false
const activation = permissionModule.ensureNotificationServiceWorker().then(() => { activated = true })
await tick()
assert.equal(activated, false)
worker.state = 'activated'
worker.dispatchEvent(new Event('statechange'))
await activation
assert.equal(activated, true)

// 真实 Worker 代码：没有窗口时的后台 push 仍展示；在线入口/重复 push 共用
// 同一事件 tag 去重，独立目标阶段分别展示，非法点击路由回到站内首页。
const workerEvents = new Map(), displayed = [], activeNotifications = new Map()
let rejectDisplay = false
const workerSelf = {
  location: { origin: 'https://quantvista.test' },
  addEventListener: (name, handler) => workerEvents.set(name, handler),
  skipWaiting: async () => {},
  registration: {
    getNotifications: async ({ tag }) => activeNotifications.has(tag) ? [activeNotifications.get(tag)] : [],
    showNotification: async (title, options) => {
      if (rejectDisplay) throw new Error('系统未允许展示')
      displayed.push({ title, ...options }); activeNotifications.set(options.tag, options)
    },
  },
  clients: { claim: async () => {}, matchAll: async () => [], openWindow: async () => {} },
}
vm.runInNewContext(fs.readFileSync(new URL('../public/sw.js', import.meta.url), 'utf8'), { self: workerSelf, URL })
async function workerEvent(type, data) {
  let completion
  workerEvents.get(type)({ ...data, waitUntil: promise => { completion = promise } })
  await completion
}
const push = { event_id: 301, user_id: 1, title: '第一目标到价', level: 'urgent', route: 'https://external.test' }
await workerEvent('push', { data: { json: () => push } })
await workerEvent('push', { data: { json: () => push } })
const replies = []
await workerEvent('message', { data: { type: 'qv-show-notification', payload: push }, ports: [{ postMessage: reply => replies.push(reply) }] })
assert.equal(displayed.length, 1)
assert.equal(replies[0].ok, true)
assert.equal(displayed[0].data.route, '/')
assert.equal(displayed[0].requireInteraction, true)
await workerEvent('push', { data: { json: () => ({ ...push, event_id: 302, title: '延伸目标到价' }) } })
assert.equal(displayed.length, 2)
rejectDisplay = true
await workerEvent('message', { data: { type: 'qv-show-notification', payload: { ...push, event_id: 303 } }, ports: [{ postMessage: reply => replies.push(reply) }] })
assert.equal(replies.at(-1).ok, false, '展示失败不能确认成功')
console.log('首次授权、后台系统通知、Worker 激活及 Push/在线去重回归通过')
