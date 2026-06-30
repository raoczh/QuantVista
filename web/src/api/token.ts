// 令牌的 localStorage 存取。独立成模块，供 axios 拦截器与 auth store 共用，避免循环依赖。
const ACCESS_KEY = 'qv-access-token'
const REFRESH_KEY = 'qv-refresh-token'

export function getAccessToken(): string {
  return localStorage.getItem(ACCESS_KEY) || ''
}

export function getRefreshToken(): string {
  return localStorage.getItem(REFRESH_KEY) || ''
}

export function setTokens(access: string, refresh: string) {
  localStorage.setItem(ACCESS_KEY, access)
  localStorage.setItem(REFRESH_KEY, refresh)
}

export function clearTokens() {
  localStorage.removeItem(ACCESS_KEY)
  localStorage.removeItem(REFRESH_KEY)
}
