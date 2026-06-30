import { request } from './client'
import type { AuthUser } from './auth'

export interface SystemSettings {
  registration_open: boolean
  github_oauth_enabled: boolean
  github_client_id: string
  has_github_secret: boolean
}

// 部分更新：仅传需要改的字段。github_client_secret 留空表示保留原值。
export interface SystemSettingsUpdate {
  registration_open?: boolean
  github_oauth_enabled?: boolean
  github_client_id?: string
  github_client_secret?: string
}

export function getSystemSettings() {
  return request<SystemSettings>({ url: '/admin/settings' })
}

export function updateSystemSettings(update: SystemSettingsUpdate) {
  return request<SystemSettings>({ url: '/admin/settings', method: 'put', data: update })
}

export function listUsers() {
  return request<AuthUser[]>({ url: '/admin/users' })
}

export function setUserStatus(id: number, status: string) {
  return request<{ ok: boolean }>({ url: `/admin/users/${id}/status`, method: 'put', data: { status } })
}
