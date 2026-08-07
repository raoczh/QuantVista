import type { RouteLocationRaw } from 'vue-router'
import { request } from './client'

export type TaskSource = 'analysis' | 'recommendation' | 'daily_report' | 'job' | 'llm' | 'data_sync'
export type TaskStatus = 'queued' | 'running' | 'success' | 'degraded' | 'failed' | 'canceled'
export type TaskStage = 'queued' | 'running' | 'finished'

export interface JobStep {
  id: number
  sequence: number
  name: string
  status: TaskStatus
  error?: string
  error_code?: string
  started_at?: string
  finished_at?: string
}

export interface TaskCenterItem {
  id: string
  source: TaskSource
  source_id: number
  result_id?: number
  parent_id?: number
  kind: string
  owner: 'user' | 'system'
  owner_user_id?: number
  triggered_by?: number
  title: string
  target: string
  status: TaskStatus
  raw_status: string
  stage: TaskStage
  error: string
  error_code: string
  provider: string
  model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  latency_ms: number
  trace_id: string
  total: number
  succeeded: number
  failed: number
  can_cancel: boolean
  can_retry: boolean
  cancel_requested: boolean
  steps?: JobStep[]
  created_at: string
  updated_at: string
}

export interface JobRuntimeMetrics {
  buckets: Array<{ kind: string; status: TaskStatus; count: number }>
  oldest_queued_at?: string
  capacity: { workers: number; capacity: number; in_use: number; available: number }
  generated_at: string
}

export interface TaskCenterQuery {
  source?: TaskSource | ''
  kind?: string
  status?: TaskStatus | ''
  limit?: number
  include_system?: boolean
  include_steps?: boolean
}

export function listTasks(params: TaskCenterQuery = {}, signal?: AbortSignal) {
  return request<TaskCenterItem[]>({
    url: '/tasks',
    params: {
      source: params.source || undefined,
      kind: params.kind || undefined,
      status: params.status || undefined,
      limit: params.limit,
      include_system: params.include_system ? 1 : undefined,
      include_steps: params.include_steps ? 1 : undefined,
    },
    signal,
  })
}

export function getJobRuntimeMetrics(signal?: AbortSignal) {
  return request<JobRuntimeMetrics>({ url: '/admin/jobs/metrics', method: 'get', signal })
}

export const TASK_SOURCE_LABELS: Record<TaskSource, string> = {
  analysis: 'AI 分析',
  recommendation: '推荐生成',
  daily_report: '收盘日报',
  job: '统一作业',
  llm: '通用 AI 任务',
  data_sync: '系统数据任务',
}

export const TASK_STATUS_LABELS: Record<TaskStatus, string> = {
  queued: '排队中',
  running: '运行中',
  success: '成功',
  degraded: '降级',
  failed: '失败',
  canceled: '已取消',
}

export const JOB_STEP_LABELS: Record<JobStep['name'], string> = {
  queued: '进入队列',
  dispatch: '工作器分派',
  execute: '业务执行',
  persist: '结果持久化',
  collect_data: '采集证据',
  llm_analysis: '模型分析',
  trust_review: '可信度复核',
  candidate_pool: '构建候选池',
  quant_scoring: '量化评分',
  llm_selection: '模型筛选',
  quant_fallback: '量化降级',
  snapshot: '生成数据快照',
  dual_generation: '双路生成',
  finalize: '报告收敛',
}

const TASK_KIND_LABELS: Record<string, string> = {
  market: '全市场分析',
  sector: '板块分析',
  stock: '个股分析',
  watchlist: '自选分析',
  position: '持仓分析',
  short_term: '短线推荐',
  long_term: '长线推荐',
  daily_report: '收盘日报',
  analysis: 'AI 分析',
  recommendation: '选股推荐',
  qa: '个股问答',
  compare: '横向对比',
  position_advice: '持仓建议',
  screener_parse: '白话策略解析',
  sync_daily_bars: '日线同步',
  backfill_calendar: '交易日历回填',
  snapshot_market: '市场快照',
  sync_market_wide: '全市场增量同步',
  init_market_history: '全市场历史初始化',
  factor_rebuild: '因子宽表重建',
}

export function taskSourceLabel(source: TaskSource): string {
  return TASK_SOURCE_LABELS[source] || source
}

export function taskKindLabel(kind: string): string {
  return TASK_KIND_LABELS[kind] || kind || '其他任务'
}

export function taskStatusLabel(status: TaskStatus): string {
  return TASK_STATUS_LABELS[status] || status
}

export function taskStageLabel(stage: TaskStage): string {
  if (stage === 'queued') return '等待分派'
  if (stage === 'running') return '执行中'
  return '已结束'
}

export function formatTaskTime(value: string): string {
  if (!value) return '-'
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '-'
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function formatTaskCompactTime(value: string): string {
  if (!value) return '-'
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '-'
  const now = new Date()
  const sameDay =
    date.getFullYear() === now.getFullYear() &&
    date.getMonth() === now.getMonth() &&
    date.getDate() === now.getDate()
  return date.toLocaleString('zh-CN', {
    hour12: false,
    month: sameDay ? undefined : '2-digit',
    day: sameDay ? undefined : '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function formatTaskDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const minutes = Math.floor(ms / 60_000)
  const seconds = Math.floor((ms % 60_000) / 1000)
  if (minutes < 60) return `${minutes}m ${seconds}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

export function taskDurationText(task: TaskCenterItem, now = Date.now()): string {
  if (task.latency_ms > 0) return formatTaskDuration(task.latency_ms)
  const created = new Date(task.created_at).getTime()
  if (!Number.isFinite(created)) return '-'
  if (task.status === 'queued' || task.status === 'running') return formatTaskDuration(Math.max(0, now - created))
  const updated = new Date(task.updated_at).getTime()
  return Number.isFinite(updated) ? formatTaskDuration(Math.max(0, updated - created)) : '-'
}

export function taskProgressText(task: TaskCenterItem): string {
  if (task.total <= 0) return '-'
  const parts = [`${task.succeeded}/${task.total}`]
  if (task.failed > 0) parts.push(`失败 ${task.failed}`)
  return parts.join(' · ')
}

function baseTaskRoute(task: TaskCenterItem): RouteLocationRaw | null {
  switch (task.source) {
    case 'analysis':
      return { name: 'analysis' }
    case 'recommendation':
      return { name: 'recommendations' }
    case 'daily_report':
      return { name: 'daily-report' }
    case 'data_sync':
      return { name: 'admin' }
    case 'job':
    case 'llm':
      if (task.kind === 'analysis') return { name: 'analysis' }
      if (task.kind === 'recommendation') return { name: 'recommendations' }
      if (task.kind === 'daily_report') return { name: 'daily-report' }
      if (task.kind === 'qa') return { name: 'qa' }
      if (task.kind === 'compare') return { name: 'compare' }
      if (task.kind === 'position_advice') return { name: 'positions' }
      if (task.kind === 'screener_parse') return { name: 'screener' }
      return null
  }
}

/**
 * 任务深链只携带定位 ID，不携带或重放原始请求。失败任务回功能页重新发起，
 * screener_parse 始终只回选股页，避免伪造不可恢复的结果入口。
 */
export function taskResultRoute(task: TaskCenterItem): RouteLocationRaw | null {
  const base = baseTaskRoute(task)
  if (!base || task.status === 'failed' || task.status === 'canceled' || task.source === 'data_sync' || task.kind === 'screener_parse') return base

  const id = String(task.result_id || task.source_id)
  if (task.source === 'analysis') return { name: 'analysis', query: { record_id: id } }
  if (task.source === 'recommendation') return { name: 'recommendations', query: { batch_id: id } }
  if (task.source === 'daily_report') return { name: 'daily-report', query: { report_id: id } }
  if (task.source === 'llm' || task.source === 'job') {
    if (task.kind === 'analysis') return { name: 'analysis', query: { record_id: id } }
    if (task.kind === 'recommendation') return { name: 'recommendations', query: { batch_id: id } }
    if (task.kind === 'daily_report') return { name: 'daily-report', query: { report_id: id } }
    if (task.kind === 'qa') return { name: 'qa', query: { task_id: id } }
    if (task.kind === 'compare') return { name: 'compare', query: { task_id: id } }
    if (task.kind === 'position_advice') return { name: 'positions', query: { task_id: id } }
  }
  return base
}

export function taskActionLabel(task: TaskCenterItem): string {
  if (!taskResultRoute(task)) return ''
  if (task.status === 'failed' || task.status === 'canceled') return task.source === 'data_sync' ? '返回管理后台处理' : '返回对应功能'
  if (task.source === 'data_sync') return '打开管理后台'
  if (task.kind === 'screener_parse') return '返回选股'
  if (task.status === 'queued' || task.status === 'running') return '查看任务'
  return '查看结果'
}

export function taskRecoveryAdvice(task: TaskCenterItem): string {
  if (task.status === 'queued') return '任务正在等待工作器分派。'
  if (task.status === 'running') return task.cancel_requested ? '已请求取消，正在等待执行器协作收敛。' : '任务正在后台执行。'
  if (task.status === 'success') return '结果已保存，可随时打开查看。'
  if (task.status === 'degraded') return '已保留可用的降级结果，打开后请留意限制说明。'
  if (task.status === 'canceled') return '任务已取消，未生成新的业务结果。'
  if (task.source === 'data_sync') return '返回管理后台的数据健康区域检查数据源后重新触发同步。'

  switch (task.error_code) {
    case 'stale_quote':
      return '返回原功能，刷新行情或确认按历史数据解释后重新发起。'
    case 'insufficient_fresh_quotes':
      return '返回原功能，补充至少两只有效行情的标的后重新发起。'
    case 'quota_exhausted':
      return 'AI 配额已用尽；额度恢复后返回原功能重新发起。'
    case 'quota_unavailable':
      return '配额服务暂不可用；稍后返回原功能重新发起。'
    case 'llm_unavailable':
      return '先在设置中配置可用模型，再返回原功能重新发起。'
    case 'llm_response_incomplete':
      return '模型响应不完整；检查模型输出上限后返回原功能重新发起。'
    case 'llm_content_filtered':
      return '内容被上游策略拦截；调整输入后返回原功能重新发起。'
    case 'llm_output_invalid':
      return '模型输出未通过校验；调整输入或模型后返回原功能重新发起。'
    case 'market_closed':
      return '今日休市；请在下一个交易日返回收盘日报重新发起。'
    case 'market_calendar_unknown':
      return '交易日历暂不可用；数据恢复后返回原功能重新发起。'
    case 'report_window_not_open':
      return '请在交易日 15:35 后返回收盘日报重新发起。'
    case 'task_timeout':
    case 'task_stale':
      return '后台任务已超时收敛；返回原功能重新发起。'
    case 'task_panic':
    case 'result_encode_failed':
      return '后台处理异常；返回原功能重新发起，并保留错误码用于排查。'
    default:
      return '返回对应功能页检查输入与配置后重新发起。'
  }
}

export interface JobRun {
  id: number
  kind: string
  parent_id?: number
  status: TaskStatus
  result_type?: string
  result_id?: number
  error?: string
  error_code?: string
  cancel_requested: boolean
  steps?: JobStep[]
  created_at: string
  updated_at: string
}

export function getJob(id: number) {
  return request<JobRun>({ url: `/tasks/${id}` })
}

export function cancelJob(id: number) {
  return request<JobRun>({ url: `/tasks/${id}/cancel`, method: 'post' })
}

export function retryJob(id: number) {
  return request<JobRun>({ url: `/tasks/${id}/retry`, method: 'post' })
}
