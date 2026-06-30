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

export function logout(refreshToken: string) {
  return request<{ ok: boolean }>({ url: '/auth/logout', method: 'post', data: { refresh_token: refreshToken } })
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

export function getSelf() {
  return request<AuthUser>({ url: '/user/self' })
}
