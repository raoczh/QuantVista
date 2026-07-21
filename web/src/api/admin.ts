import { request, HEAVY_TIMEOUT } from './client'
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
  llm_accuracy_contract: boolean
  site_base_url: string
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
  llm_accuracy_contract?: boolean
  site_base_url?: string
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

// ---------- P1 数据健康总览与补跑 ----------

export interface DataHealthItem {
  key: string
  name: string
  expected_date: string // 按交易日历应有的最新日期
  observed_date: string // 库内实际最新日期（空=无数据）
  lag_open_days: number // 落后开市日数（-1=日历不可用无法判定）
  tolerance_open_days: number
  status: string // ok / behind / empty / unknown
  coverage?: string
  last_run?: SyncLog
  note?: string
}

export interface DataHealthReport {
  generated_at: string
  items: DataHealthItem[]
}

export function getDataHealth() {
  return request<DataHealthReport>({ url: '/admin/data-health' })
}

// 补跑入口（既有管理端接口）：全市场增量 / 历史初始化 / 日线批量同步 / 情绪快照 /
// 因子宽表重建 / 日历回填。均异步或幂等，返回启动标志。
export function triggerWideSync() {
  return request<{ started: boolean }>({ url: '/admin/market/wide-sync', method: 'post' })
}
export function triggerWideInit() {
  return request<{ started: boolean }>({ url: '/admin/market/wide-init', method: 'post' })
}
export function triggerSyncBars() {
  return request<{ started: boolean }>({ url: '/admin/market/sync-bars', method: 'post', timeout: HEAVY_TIMEOUT })
}
export function triggerSnapshot() {
  return request<unknown>({ url: '/admin/market/snapshot', method: 'post', timeout: HEAVY_TIMEOUT })
}
export function triggerFactorRebuild() {
  return request<{ started: boolean }>({ url: '/admin/market/factor-rebuild', method: 'post' })
}
export function triggerBackfillCalendar() {
  return request<unknown>({ url: '/admin/market/backfill-calendar', method: 'post', timeout: HEAVY_TIMEOUT })
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

// S3-4 因子 RankIC 验证报表（管理端只读）。
export interface ICHorizonAgg {
  mean_ic: number
  icir: number
  win_rate_pct: number
  days: number
}

export interface FactorICStat {
  key: string
  name: string
  group: string
  horizons: Record<string, ICHorizonAgg>
}

export interface FactorICReport {
  trade_date: string
  dates: string[]
  universe: number
  st_skipped: number
  adjust_suspect: number
  min_cross: number
  stats: FactorICStat[] | null
  notes: string[]
  elapsed_ms: number
  generated_at: string
}

// 默认取进程内缓存；refresh=true 全量重算（数秒级，服务端全局互斥）。
export function getFactorIC(refresh = false) {
  return request<FactorICReport>({
    url: '/admin/market/factor-ic',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// S3-5 walk-forward 评估基线报表（管理端只读）。
export interface WFSpec {
  train: number
  val: number
  test: number
  step: number
  purge: number
  embargo: number
}

export interface WFMetricFields {
  signals: number
  picked: number
  trades: number
  skipped: number
  pending: number
  precision_net_pct: number
  median_net_pct: number
  avg_net_pct: number
  severe_loss_pct: number
  alpha_sample: number
  precision_alpha_pct: number
  median_alpha_pct: number
}

export interface WFSegRow extends WFMetricFields {
  fold: number // 0=全折合并
  segment: string // val / test
  strategy: string
  strategy_name: string
  hold: number
}

export interface WFFoldView {
  fold: number
  train_range: [string, string]
  val_range: [string, string]
  test_range: [string, string]
  val_signals: number
  test_signals: number
}

export interface WFMonthlyItem {
  symbol: string
  name: string
  score: number
  status: string
  net_pct?: number
  alpha_pct?: number
}

export interface WFMonthlyRow extends WFMetricFields {
  month: string
  signal_date: string
  strategy: string
  strategy_name: string
  hold: number
  items: WFMonthlyItem[] | null
}

export interface WFSectionReport {
  rec_type: string // short_term / long_term
  holds: number[]
  strategies: string[] | null
  spec: WFSpec
  target_spec: WFSpec
  adapted: boolean
  spec_note: string
  folds: WFFoldView[] | null
  rows: WFSegRow[] | null
  monthly: WFMonthlyRow[] | null
}

export interface WalkForwardReport {
  trade_date: string
  top_k: number
  universe: number
  st_skipped: number
  adjust_suspect: number
  sections: WFSectionReport[] | null
  notes: string[]
  elapsed_ms: number
  generated_at: string
}

// 默认取进程内缓存；refresh=true 全量重算（每信号日一次全市场 as-of 重算，数十秒级）。
export function getWalkForward(refresh = false) {
  return request<WalkForwardReport>({
    url: '/admin/market/walk-forward',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}
