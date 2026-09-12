import { request } from './client'

export interface AuthUser {
  id: number
  username: string
  display_name: string
  github_id: string
  email: string
  avatar_url: string
  role: string // user / admin
  status: string
  last_login_at?: string
}

export interface TokenPair {
  access_token: string
  refresh_token: string
  expires_at: number
  user: AuthUser
}

export interface SetupStatus {
  initialized: boolean
  github_oauth_enabled: boolean
  registration_open: boolean
}

export function getSetupStatus() {
  return request<SetupStatus>({ url: '/setup/status' })
}

export function createAdmin(username: string, password: string) {
  return request<TokenPair>({ url: '/setup/admin', method: 'post', data: { username, password } })
}

export function loginByPassword(username: string, password: string) {
  return request<TokenPair>({ url: '/auth/login', method: 'post', data: { username, password } })
}

export async function logout(refreshToken: string): Promise<{ ok: boolean }> {
  if (!refreshToken) return { ok: true }
  // 吊销对象由旧 refresh token 固定，不随新的登录身份变化；页面卸载也应继续清理。
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), 20_000)
  try {
    const response = await fetch('/api/auth/logout', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refresh_token: refreshToken }),
      keepalive: true,
      signal: controller.signal,
    })
    const body = await response.json()
    if (!response.ok || !body?.success) throw new Error(body?.message || '退出登录失败')
    return body.data
  } finally {
    clearTimeout(timer)
  }
}

export function getGithubAuthURL(redirectURI: string) {
  return request<{ url: string }>({ url: '/oauth/github/url', params: { redirect_uri: redirectURI } })
}

export function githubCallback(code: string, state: string, redirectURI: string) {
  return request<TokenPair>({
    url: '/oauth/github',
    method: 'post',
    data: { code, state, redirect_uri: redirectURI },
  })
}

// ---- 移动流（App 内发起 GitHub 登录，阶段 B）----
// state 走服务端一次性存储 + PKCE；回调换一次性短码，深链回 App 后凭 verifier 兑换。

export function getGithubAuthURLMobile(redirectURI: string, codeChallenge: string) {
  return request<{ url: string }>({
    url: '/oauth/github/url',
    params: { redirect_uri: redirectURI, mode: 'mobile', code_challenge: codeChallenge },
  })
}

export function githubMobileCallback(code: string, state: string, redirectURI: string) {
  return request<{ auth_code: string }>({
    url: '/oauth/github/mobile-callback',
    method: 'post',
    data: { code, state, redirect_uri: redirectURI },
  })
}

export function githubMobileExchange(authCode: string, codeVerifier: string) {
  return request<TokenPair>({
    url: '/oauth/github/mobile-exchange',
    method: 'post',
    data: { auth_code: authCode, code_verifier: codeVerifier },
  })
}

export function getSelf() {
  return request<AuthUser>({ url: '/user/self' })
}

// GitHub 绑定/解绑（authed）。绑定的授权地址复用 getGithubAuthURL。
export function bindGithub(code: string, state: string, redirectURI: string) {
  return request<AuthUser>({
    url: '/user/github/bind',
    method: 'post',
    data: { code, state, redirect_uri: redirectURI },
  })
}

export function unbindGithub() {
  return request<AuthUser>({ url: '/user/github/bind', method: 'delete' })
}
