// 令牌的 localStorage 存取。独立成模块，供 axios 拦截器与 auth store 共用，避免循环依赖。
const ACCESS_KEY = 'qv-access-token'
const REFRESH_KEY = 'qv-refresh-token'
const SESSION_KEY = 'qv-session-id'
let sessionEpoch = 0
let observedSession = localStorage.getItem(SESSION_KEY) || ''
let notifiedSession = observedSession
const externalSessionListeners = new Set<() => void>()

function rememberStoredTokens() {
  observedSession = localStorage.getItem(SESSION_KEY) || ''
}

function markNewSession() {
  // 只用于区分登录轮次；令牌刷新不改变此标记，也不要求其他标签重载页面。
  const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  localStorage.setItem(SESSION_KEY, id)
  rememberStoredTokens()
  notifiedSession = id
  sessionEpoch++
}

export function onExternalSessionChange(listener: () => void): () => void {
  externalSessionListeners.add(listener)
  return () => externalSessionListeners.delete(listener)
}

// 登录/退出改变会话身份；同一会话内的令牌轮换不改变身份。
export function getSessionEpoch(): number {
  // storage 事件可能还在队列中；请求开始/提交结果时直接对照共享存储。
  if (observedSession !== (localStorage.getItem(SESSION_KEY) || '')) {
    sessionEpoch++
    rememberStoredTokens()
  }
  return sessionEpoch
}

export function getAccessToken(): string {
  return localStorage.getItem(ACCESS_KEY) || ''
}

export function getRefreshToken(): string {
  return localStorage.getItem(REFRESH_KEY) || ''
}

export function setTokens(access: string, refresh: string) {
  localStorage.setItem(ACCESS_KEY, access)
  localStorage.setItem(REFRESH_KEY, refresh)
  markNewSession()
}

export function rotateTokens(expectedRefresh: string, access: string, refresh: string): boolean {
  if (getRefreshToken() !== expectedRefresh) return false
  localStorage.setItem(ACCESS_KEY, access)
  localStorage.setItem(REFRESH_KEY, refresh)
  rememberStoredTokens()
  return true
}

export function clearTokens() {
  localStorage.removeItem(ACCESS_KEY)
  localStorage.removeItem(REFRESH_KEY)
  markNewSession()
}

if (typeof window !== 'undefined') {
  window.addEventListener('storage', (event) => {
    if (event.storageArea !== localStorage) return
    if (event.key === null || event.key === ACCESS_KEY || event.key === REFRESH_KEY || event.key === SESSION_KEY) getSessionEpoch()
    if (event.key === null || event.key === SESSION_KEY) {
      const id = localStorage.getItem(SESSION_KEY) || ''
      if (event.key !== null && id === notifiedSession) return
      notifiedSession = id
      externalSessionListeners.forEach((listener) => listener())
    }
  })
}
