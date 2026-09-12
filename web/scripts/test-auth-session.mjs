import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import axios from 'axios'
import * as vue from 'vue'
import * as pinia from 'pinia'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
function load(relative, dependencies = {}) {
  const source = fs.readFileSync(path.join(root, relative), 'utf8')
  const output = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, esModuleInterop: true },
  }).outputText
  const module = { exports: {} }
  new Function('require', 'module', 'exports', output)((name) => {
    assert.ok(name in dependencies, `未声明测试依赖 ${name}`)
    return dependencies[name]
  }, module, module.exports)
  return module.exports
}

const storage = new Map()
globalThis.localStorage = {
  getItem: (key) => storage.get(key) ?? null,
  setItem: (key, value) => storage.set(key, String(value)),
  removeItem: (key) => storage.delete(key),
}
globalThis.location = { origin: 'https://quantvista.test', pathname: '/positions', search: '', href: '' }
const tokens = load('src/api/token.ts')
const client = load('src/api/client.ts', { axios, './token': tokens })
const response = (config, data, status = 200) => ({ config, data, status, statusText: '', headers: {} })
function failure(config, status) {
  return new axios.AxiosError('测试请求失败', 'ERR_BAD_RESPONSE', config, null,
    response(config, { success: false, message: '测试故障' }, status))
}
function deferred() {
  let resolve
  const promise = new Promise((r) => { resolve = r })
  return { promise, resolve }
}
async function waitFor(check) {
  for (let i = 0; i < 50 && !check(); i++) await new Promise(setImmediate)
  assert.ok(check(), '请求未进入预期阶段')
}
const settle = (promise) => promise.then((value) => ({ value }), (error) => ({ error }))

// 执行真实 axios 拦截器，传输层用可控适配器，避免任何外部请求。
let gate = deferred()
let refreshCalls = 0
let replayed = 0
axios.defaults.adapter = async (config) => {
  refreshCalls++
  await gate.promise
  return response(config, { success: true, data: { access_token: 'rotated-access', refresh_token: 'rotated-refresh' } })
}
client.default.defaults.adapter = async (config) => {
  if (config.headers.Authorization === 'Bearer old-access') throw failure(config, 401)
  replayed++
  return response(config, { success: true, data: 'done' })
}
tokens.setTokens('old-access', 'old-refresh')
const first = settle(client.request({ url: '/protected', method: 'post', data: { action: 'save' } }))
const second = settle(client.request({ url: '/protected' }))
await waitFor(() => refreshCalls === 1)
gate.resolve()
assert.equal((await first).value, 'done')
assert.equal((await second).value, 'done')
assert.equal(refreshCalls, 1, '并发 401 应共享一次刷新')
assert.equal(replayed, 2)

for (const success of [true, false]) {
  gate = deferred()
  refreshCalls = 0
  replayed = 0
  location.href = ''
  axios.defaults.adapter = async (config) => {
    refreshCalls++
    await gate.promise
    return response(config, success
      ? { success: true, data: { access_token: 'late-old-access', refresh_token: 'late-old-refresh' } }
      : { success: false, message: '旧会话失效' })
  }
  tokens.setTokens('old-access', 'old-refresh')
  const oldRequest = settle(client.request({ url: '/protected', method: 'post', data: { action: 'old-account-save' } }))
  await waitFor(() => refreshCalls === 1)
  tokens.clearTokens()
  tokens.setTokens('new-account-access', 'new-account-refresh')
  gate.resolve()
  assert.ok((await oldRequest).error, '退出后的旧请求不能继续执行')
  assert.equal(tokens.getAccessToken(), 'new-account-access', '迟到刷新不能覆盖或清除新账号')
  assert.equal(replayed, 0, '旧账号的写请求不能重放到新账号')
  assert.equal(location.href, '', '旧会话失败不能把新账号跳回登录页')
}

tokens.setTokens('old-access', 'old-refresh')
axios.defaults.adapter = async (config) => { throw failure(config, 503) }
const unavailable = await settle(client.request({ url: '/protected' }))
assert.ok(unavailable.error)
assert.equal(tokens.getRefreshToken(), 'old-refresh', '登录服务临时不可用不能销毁凭证')

tokens.setTokens('old-access', 'old-refresh')
replayed = 0
const queuedBeforeAccountChange = settle(client.request({ url: '/protected', method: 'post', data: { action: 'old-account-write' } }))
tokens.setTokens('new-account-access', 'new-account-refresh')
assert.ok((await queuedBeforeAccountChange).error, '调用 API 时就应固定会话，不能等 axios 拦截器异步执行时才读取身份')
assert.equal(replayed, 0, '尚未进入拦截器的旧账号写请求不得发送到新账号')

const peerTokens = load('src/api/token.ts')
tokens.setTokens('old-access', 'old-refresh')
let responseGate = deferred()
let writes = 0
client.default.defaults.adapter = async (config) => {
  writes++
  await responseGate.promise
  return response(config, { success: true, data: 'committed' })
}
const rotatingEpoch = tokens.getSessionEpoch()
const committedWrite = settle(client.request({ url: '/protected', method: 'post' }))
await waitFor(() => writes === 1)
assert.equal(peerTokens.rotateTokens('old-refresh', 'peer-access', 'peer-refresh'), true)
assert.equal(tokens.getSessionEpoch(), rotatingEpoch, '跨标签同会话轮换不应改变请求所属身份')
responseGate.resolve()
assert.equal((await committedWrite).value, 'committed', '已提交的成功响应不能因跨标签刷新被误拒绝')
assert.equal(writes, 1, '成功写入不能被重放')

for (const refreshStatus of [200, 401, 503]) {
  tokens.setTokens('old-access', 'old-refresh')
  gate = deferred()
  refreshCalls = 0
  axios.defaults.adapter = async (config) => {
    refreshCalls++
    await gate.promise
    if (refreshStatus !== 200) throw failure(config, refreshStatus)
    return response(config, { success: true, data: { access_token: 'late-access', refresh_token: 'late-refresh' } })
  }
  client.default.defaults.adapter = async (config) => {
    if (config.headers.Authorization === 'Bearer old-access') throw failure(config, 401)
    assert.equal(config.headers.Authorization, 'Bearer peer-access')
    return response(config, { success: true, data: 'done' })
  }
  const refreshRace = settle(client.request({ url: '/protected' }))
  await waitFor(() => refreshCalls === 1)
  peerTokens.rotateTokens('old-refresh', 'peer-access', 'peer-refresh')
  gate.resolve()
  assert.equal((await refreshRace).value, 'done', `跨标签刷新已成功时，迟到 ${refreshStatus} 应使用新凭证`)
  assert.equal(tokens.getRefreshToken(), 'peer-refresh')
}

tokens.setTokens('old-access', 'old-refresh')
const observedEpoch = tokens.getSessionEpoch()
peerTokens.setTokens('peer-account-access', 'peer-account-refresh')
assert.notEqual(tokens.getSessionEpoch(), observedEpoch, 'storage 事件尚未派发时，也必须立即识别其他标签页的会话变化')

const storageEvents = []
globalThis.window = { addEventListener: (type, handler) => { if (type === 'storage') storageEvents.push(handler) } }
const observingTab = load('src/api/token.ts')
let externalChanges = 0
const stopObserving = observingTab.onExternalSessionChange(() => { externalChanges++ })
peerTokens.setTokens('peer-new-login', 'peer-new-refresh')
storageEvents[0]({ storageArea: localStorage, key: 'qv-session-id' })
assert.equal(externalChanges, 1, '其他标签的新登录必须通知页面重建会话')
peerTokens.rotateTokens('peer-new-refresh', 'peer-rotated-access', 'peer-rotated-refresh')
storageEvents[0]({ storageArea: localStorage, key: 'qv-access-token' })
storageEvents[0]({ storageArea: localStorage, key: 'qv-refresh-token' })
assert.equal(externalChanges, 1, '普通令牌轮换不能导致其他标签整页重载')
peerTokens.clearTokens()
storageEvents[0]({ storageArea: localStorage, key: 'qv-session-id' })
assert.equal(externalChanges, 2, '其他标签退出登录必须同步清理页面身份')
stopObserving()
delete globalThis.window

// 两个独立客户端模拟两个标签；输家的刷新 401 会先于赢家的成功响应返回。
const navigatorDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
let lockTail = Promise.resolve()
Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { locks: {
  request: (_name, callback) => {
    const operation = lockTail.then(callback)
    lockTail = operation.catch(() => undefined)
    return operation
  },
} } })
const peerClient = load('src/api/client.ts', { axios, './token': peerTokens })
tokens.setTokens('old-access', 'old-refresh')
gate = deferred()
refreshCalls = 0
axios.defaults.adapter = async (config) => {
  refreshCalls++
  if (refreshCalls > 1) throw failure(config, 401)
  await gate.promise
  return response(config, { success: true, data: { access_token: 'rotated-access', refresh_token: 'rotated-refresh' } })
}
client.default.defaults.adapter = peerClient.default.defaults.adapter = async (config) => {
  if (config.headers.Authorization === 'Bearer old-access') throw failure(config, 401)
  return response(config, { success: true, data: 'done' })
}
const pairedRequests = [settle(client.request({ url: '/protected' })), settle(peerClient.request({ url: '/protected' }))]
await waitFor(() => refreshCalls > 0)
await new Promise(setImmediate)
gate.resolve()
for (const operation of pairedRequests) assert.equal((await operation).value, 'done', '另一标签较早的刷新 401 不得清除成功轮换')
assert.equal(refreshCalls, 1, '跨标签锁应覆盖刷新及凭证发布，等待者复用新令牌')

// 没有 Web Locks 且没有成功同行时，等待窗口结束后仍正常确认失效并清理监听。
Object.defineProperty(globalThis, 'navigator', { configurable: true, value: {} })
const refreshStorageListeners = new Set()
globalThis.window = { addEventListener: (_event, handler) => refreshStorageListeners.add(handler), removeEventListener: (_event, handler) => refreshStorageListeners.delete(handler) }
const realSetTimeout = globalThis.setTimeout
globalThis.setTimeout = (callback, ms, ...args) => realSetTimeout(callback, ms === 20000 ? 10 : ms, ...args)
tokens.setTokens('old-access', 'old-refresh')
axios.defaults.adapter = async (config) => { throw failure(config, 401) }
const expiredWithoutPeer = await settle(client.request({ url: '/protected' }))
assert.ok(expiredWithoutPeer.error)
assert.equal(tokens.getAccessToken(), '')
assert.equal(refreshStorageListeners.size, 0, '等待结束必须释放 storage 监听')
globalThis.setTimeout = realSetTimeout
delete globalThis.window
if (navigatorDescriptor) Object.defineProperty(globalThis, 'navigator', navigatorDescriptor)
else delete globalThis.navigator

const authAPI = load('src/api/auth.ts', { './client': client })
const nativeFetch = globalThis.fetch
let logoutInput
globalThis.fetch = async (url, input) => {
  assert.equal(url, '/api/auth/logout')
  logoutInput = input
  return { ok: true, json: async () => ({ success: true, data: { ok: true } }) }
}
tokens.setTokens('old-access', 'old-refresh')
const logoutInFlight = settle(authAPI.logout(tokens.getRefreshToken()))
tokens.setTokens('next-access', 'next-refresh')
assert.deepEqual((await logoutInFlight).value, { ok: true }, '旧会话吊销不应因为随后登录另一个账号而被取消')
assert.equal(JSON.parse(logoutInput.body).refresh_token, 'old-refresh')
assert.equal(logoutInput.keepalive, true, '页面卸载后也应完成旧令牌吊销')
assert.equal(tokens.getRefreshToken(), 'next-refresh')
globalThis.fetch = nativeFetch

// 运行真实认证 store：页面离开或回调更换时，必须在写入身份前拒收旧结果。
const sessionValues = new Map()
globalThis.sessionStorage = {
  getItem: key => sessionValues.get(key) ?? null,
  setItem: (key, value) => sessionValues.set(key, String(value)),
  removeItem: key => sessionValues.delete(key),
}
let authGate = deferred(), authCalls = 0, cleanupFails = false
const nativePreferences = new Map([['qv_pkce_verifier', 'local-verifier']])
const controlledAuth = () => { authCalls++; return authGate.promise }
const { useAuthStore } = load('src/stores/auth.ts', {
  pinia, vue, axios, '@/api/client': client, '@/api/token': tokens,
  '@/api/auth': { ...authAPI, createAdmin: controlledAuth, loginByPassword: controlledAuth,
    githubCallback: controlledAuth, githubMobileExchange: controlledAuth, bindGithub: controlledAuth,
    getGithubAuthURL: controlledAuth },
  '@/lib/pkce': { generateCodeVerifier: () => 'local-verifier', codeChallengeS256: async () => 'local-challenge' },
  '@capacitor/preferences': { Preferences: {
    get: async ({ key }) => ({ value: nativePreferences.get(key) ?? null }),
    set: async ({ key, value }) => { nativePreferences.set(key, value) },
    remove: async ({ key }) => { if (cleanupFails) throw new Error('本地清理失败'); nativePreferences.delete(key) },
  } },
})
const authenticatedPair = { access_token: 'local-auth-success', refresh_token: 'local-auth-refresh', user: { id: 81, username: 'review' } }
for (const method of ['createAdmin', 'loginPassword', 'finishGithubLogin', 'finishMobileExchange', 'finishGithubBind']) {
  pinia.setActivePinia(pinia.createPinia())
  const auth = useAuthStore()
  tokens.clearTokens()
  authGate = deferred()
  authCalls = 0
  nativePreferences.set('qv_pkce_verifier', 'local-verifier')
  let current = true
  const args = method === 'finishMobileExchange' ? ['code'] : ['user-or-code', 'password-or-state']
  const operation = settle(auth[method](...args, () => current))
  await waitFor(() => authCalls === 1)
  current = false
  nativePreferences.set('qv_pkce_verifier', 'new-attempt-verifier')
  authGate.resolve(method === 'finishGithubBind' ? authenticatedPair.user : authenticatedPair)
  assert.equal((await operation).error?.code, 'auth_attempt_canceled', `${method} 必须拒收失效页面的结果`)
  assert.equal(tokens.getAccessToken(), '')
  assert.equal(auth.user, null)
  assert.equal(nativePreferences.get('qv_pkce_verifier'), 'new-attempt-verifier', '旧兑换不能清理新流程的 verifier')
}
pinia.setActivePinia(pinia.createPinia())
const loginAuth = useAuthStore()
authGate = deferred()
authCalls = 0
location.href = ''
let loginCurrent = true
const startLogin = settle(loginAuth.startGithubLogin(() => loginCurrent))
await waitFor(() => authCalls === 1)
loginCurrent = false
authGate.resolve({ url: 'https://github.invalid/local-only' })
assert.equal((await startLogin).error?.code, 'auth_attempt_canceled')
assert.equal(location.href, '', '离开登录页后不能被迟到授权地址导航带走')

for (const changedSession of [false, true]) {
  authGate = deferred()
  authCalls = 0
  location.href = ''
  let bindCurrent = true
  const startBind = settle(loginAuth.startGithubBind(() => bindCurrent))
  await waitFor(() => authCalls === 1)
  if (changedSession) tokens.setTokens('another-session-access', 'another-session-refresh')
  else bindCurrent = false
  authGate.resolve({ url: 'https://github.invalid/local-only' })
  assert.ok((await startBind).error, '页面或登录会话变化后应取消旧绑定跳转')
  assert.equal(location.href, '')
  assert.equal(sessionStorage.getItem('qv_oauth_bind'), null, '失效绑定不得留下影响后续登录的意图标记')
}

tokens.clearTokens()
authGate = deferred()
authCalls = 0
cleanupFails = true
nativePreferences.set('qv_pkce_verifier', 'local-verifier')
const successfulExchange = settle(loginAuth.finishMobileExchange('local-code'))
await waitFor(() => authCalls === 1)
authGate.resolve(authenticatedPair)
assert.equal((await successfulExchange).error, undefined, '双令牌已经生效后，清理本地 verifier 失败不能改报登录失败')
assert.equal(tokens.getAccessToken(), 'local-auth-success')
assert.equal(loginAuth.user.id, 81)

console.log('认证会话隔离与刷新故障回归通过')
