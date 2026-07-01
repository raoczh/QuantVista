import { request } from './client'

export interface UserPreference {
  id: number
  user_id: number
  risk_level: string // conservative/balanced/aggressive
  default_market: string // cn/us/hk
  horizon_pref: string // short_term/long_term
  default_rec_count: number
  enable_notify: boolean
}

export interface UserQuota {
  user_id: number
  token_limit: number
  token_used: number
  request_count: number
}

export function getPreference() {
  return request<UserPreference>({ url: '/user/preference' })
}

export function updatePreference(p: Partial<UserPreference>) {
  return request<UserPreference>({ url: '/user/preference', method: 'put', data: p })
}

export function getQuota() {
  return request<UserQuota>({ url: '/user/quota' })
}

export function changePassword(oldPassword: string, newPassword: string) {
  return request<{ ok: boolean }>({
    url: '/user/password',
    method: 'put',
    data: { old_password: oldPassword, new_password: newPassword },
  })
}
