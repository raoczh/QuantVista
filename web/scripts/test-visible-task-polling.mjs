import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'

const source = fs.readFileSync(new URL('../src/composables/useVisibleTaskPolling.ts', import.meta.url), 'utf8')
const output = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS } }).outputText
const tick = () => new Promise(setImmediate)
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done }); return { promise, resolve } }
const failures = []
const check = (name, actual, expected) => { if (actual !== expected) failures.push({ name, actual, expected }) }

function create() {
  const mounted = [], unmount = [], timers = new Map(), storage = new Map(), requests = []
  let timerID = 0, epoch = 1, refreshes = 0, tokenRefreshes = 0
  const document = Object.assign(new EventTarget(), { visibilityState: 'visible' })
  const window = { setTimeout: fn => { timers.set(++timerID, fn); return timerID }, clearTimeout: id => timers.delete(id) }
  const sessionStorage = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) }
  const module = { exports: {} }
  const fetch = (url, options) => { const pending = deferred(); requests.push({ url, options, ...pending }); return pending.promise }
  new Function('require', 'module', 'exports', 'document', 'window', 'sessionStorage', 'fetch', output)(name => {
    if (name === 'vue') return { ...vue, onMounted: callback => mounted.push(callback), onBeforeUnmount: callback => unmount.push(callback) }
    if (name === '@/api/token') return { getAccessToken: () => 'local-test', getSessionEpoch: () => epoch }
    if (name === '@/api/client') return { refreshAccessToken: async () => { tokenRefreshes++; return true } }
    throw new Error(`未知依赖 ${name}`)
  }, module, module.exports, document, window, sessionStorage, fetch)
  const scope = vue.effectScope(), enabled = vue.ref(true)
  const api = scope.run(() => module.exports.useVisibleTaskPolling(async () => { refreshes++ }, () => false, { idleIntervalMs: 30_000, enabled: () => enabled.value }))
  mounted.forEach(callback => callback())
  return {
    api, requests, storage, enabled,
    get refreshes() { return refreshes },
    get tokenRefreshes() { return tokenRefreshes },
    switchSession() { epoch++ },
    close() { unmount.splice(0).forEach(callback => callback()); scope.stop() },
  }
}

function streamResponse() {
  let controller
  const body = new ReadableStream({ start(value) { controller = value } })
  return { response: new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }), send: text => controller.enqueue(new TextEncoder().encode(text)), end: () => controller.close() }
}

for (const transition of ['close', 'switchSession']) {
  const runtime = create(), stream = streamResponse()
  await tick()
  runtime[transition]()
  stream.send('id: 81\ndata: {}\n\n')
  stream.end()
  runtime.requests[0].resolve(stream.response)
  await tick()
  check(`${transition} 后迟到事件不触发旧页面刷新`, runtime.refreshes, 1)
  check(`${transition} 后迟到事件不推进续传游标`, runtime.storage.size, 0)
  runtime.close()
}

const expired = create()
await tick()
expired.switchSession()
expired.requests[0].resolve(new Response('', { status: 401 }))
await tick()
check('旧会话的 401 不刷新新会话令牌', expired.tokenRefreshes, 0)
check('旧会话的 401 不使用新凭证重连', expired.requests.length, 1)
expired.close()

const connected = create(), stream = streamResponse()
connected.requests[0].resolve(stream.response)
await tick()
stream.send('id: 9\r')
stream.send('\ndata: {}\r\n\r\n')
await tick()
check('当前会话正常事件刷新任务摘要', connected.refreshes, 2)
check('正常 CRLF 事件保存续传游标', [...connected.storage.values()][0], '9')
connected.switchSession()
stream.send('id: 10\ndata: {}\n\n')
stream.end()
await tick()
check('连接期间切换会话后不消费旧事件', connected.refreshes, 2)
check('连接期间切换会话后不保存旧游标', [...connected.storage.values()][0], '9')
connected.close()

console.log(JSON.stringify({ failures }, null, 2))
assert.deepEqual(failures, [], '任务事件流必须复验生命周期和原会话')
console.log('任务事件流的会话、卸载与正常续传回归通过')
