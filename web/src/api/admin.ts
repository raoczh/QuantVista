import { request } from './client'
import type { AuthUser } from './auth'

export interface SystemSettings {
  registration_open: boolean
  github_oauth_enabled: boolean
  github_client_id: string
  has_github_secret: boolean
  news_collect_interval_min: number
  news_auto_llm: boolean
  llm_fallback_enabled: boolean
  llm_fallback_config_id: number
}

// 部分更新：仅传需要改的字段。github_client_secret 留空表示保留原值。
export interface SystemSettingsUpdate {
  registration_open?: boolean
  github_oauth_enabled?: boolean
  github_client_id?: string
  github_client_secret?: string
  news_collect_interval_min?: number
  news_auto_llm?: boolean
  llm_fallback_enabled?: boolean
  llm_fallback_config_id?: number
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

// ---------- 用户 AI 配额管理 ----------

export interface AdminUserQuota {
  user_id: number
  action_limit: number // 次数上限，0 = 不限
  action_used: number // 已用次数（手动触发的 AI 动作）
  token_used: number
  request_count: number
  updated_at: string
}

export function getUserQuota(id: number) {
  return request<AdminUserQuota>({ url: `/admin/users/${id}/quota` })
}

export function updateUserQuota(id: number, data: { action_limit: number; reset_used?: boolean }) {
  return request<AdminUserQuota>({ url: `/admin/users/${id}/quota`, method: 'put', data })
}

// ---------- 数据源同步日志 ----------

export interface SyncLog {
  id: number
  task: string
  market: string
  status: string // success / partial / failed
  total: number
  succeeded: number
  failed: number
  duration_ms: number
  message: string
  created_at: string
}

export function listSyncLogs(limit = 50) {
  return request<SyncLog[]>({ url: '/admin/market/sync-logs', params: { limit } })
}

// ---------- LLM 调用审计 ----------

export interface LLMCallLogItem {
  id: number
  user_id: number
  username: string
  module: string
  llm_config_id: number
  provider: string
  model: string
  endpoint_type: string
  stream: boolean
  status: string // success / error
  error_msg: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  latency_ms: number
  first_chunk_ms: number // 流式首个 data 块耗时；0=非流式；≈latency_ms 说明上游整包返回（假流式）
  request_body: string // 仅详情接口返回，列表恒为空
  response_body: string
  created_at: string
}

export interface LLMCallLogList {
  items: LLMCallLogItem[]
  total: number
}

export function listLlmCalls(params: { user_id?: number; module?: string; status?: string; page?: number; page_size?: number }) {
  return request<LLMCallLogList>({ url: '/admin/llm-calls', params })
}

export function getLlmCall(id: number) {
  return request<LLMCallLogItem>({ url: `/admin/llm-calls/${id}` })
}
