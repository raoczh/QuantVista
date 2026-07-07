import { request } from './client'

export interface UserPreference {
  id: number
  user_id: number
  risk_level: string // conservative/balanced/aggressive
  default_market: string // cn/us/hk
  horizon_pref: string // short_term/mid_term/long_term
  default_rec_count: number
  enable_notify: boolean
  enable_daily_report: boolean
  blacklist_json: string // 候选池黑名单 [{symbol,market,reason}]
  min_candidate_amount: number // 候选池最低日成交额（元；0=不过滤）
  rec_filters_json: string // 推荐筛选默认值（RecFilters JSON；空=按类型默认）
  total_capital: number // 总投资资金（元；0=未设置，持仓 AI 不注入资金上下文）
}

// 候选池黑名单条目。
export interface BlacklistEntry {
  symbol: string
  market: string
  reason: string
}

export interface UserQuota {
  user_id: number
  action_limit: number // 次数上限，0 = 不限
  action_used: number // 已用次数（手动触发的 AI 动作）
  token_used: number // 累计 token（审计参考）
  request_count: number // LLM 调用轮次（审计参考）
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
