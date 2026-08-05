import type { RouteLocationRaw } from 'vue-router'
import { request } from './client'

export type TaskSource = 'analysis' | 'recommendation' | 'daily_report' | 'llm' | 'data_sync'
export type TaskStatus = 'processing' | 'success' | 'degraded' | 'failed'
export type TaskStage = 'processing' | 'finished'

export interface TaskCenterItem {
  id: string
  source: TaskSource
  source_id: number
  kind: string
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
  created_at: string
  updated_at: string
}

export interface TaskCenterQuery {
  source?: TaskSource | ''
  kind?: string
  status?: TaskStatus | ''
  limit?: number
  include_system?: boolean
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
    },
    signal,
  })
}

export const TASK_SOURCE_LABELS: Record<TaskSource, string> = {
  analysis: 'AI 分析',
  recommendation: '推荐生成',
  daily_report: '收盘日报',
  llm: '通用 AI 任务',
  data_sync: '系统数据任务',
}

export const TASK_STATUS_LABELS: Record<TaskStatus, string> = {
  processing: '运行中',
  success: '成功',
  degraded: '降级',
  failed: '失败',
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
  qa: '个股问答',
  compare: '横向对比',
  position_advice: '持仓建议',
  screener_parse: '白话策略解析',
  sync_daily_bars: '日线同步',
  backfill_calendar: '交易日历回填',
  snapshot_market: '市场快照',
  sync_market_wide: '全市场增量同步',
  init_market_history: '全市场历史初始化',
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
  return stage === 'processing' ? '执行中' : '已结束'
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
  if (task.status === 'processing') return formatTaskDuration(Math.max(0, now - created))
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
    case 'llm':
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
  if (!base || task.status === 'failed' || task.source === 'data_sync' || task.kind === 'screener_parse') return base

  const id = String(task.source_id)
  if (task.source === 'analysis') return { name: 'analysis', query: { record_id: id } }
  if (task.source === 'recommendation') return { name: 'recommendations', query: { batch_id: id } }
  if (task.source === 'daily_report') return { name: 'daily-report', query: { report_id: id } }
  if (task.source === 'llm') {
    if (task.kind === 'qa') return { name: 'qa', query: { task_id: id } }
    if (task.kind === 'compare') return { name: 'compare', query: { task_id: id } }
    if (task.kind === 'position_advice') return { name: 'positions', query: { task_id: id } }
  }
  return base
}

export function taskActionLabel(task: TaskCenterItem): string {
  if (!taskResultRoute(task)) return ''
  if (task.status === 'failed') return task.source === 'data_sync' ? '返回管理后台处理' : '返回功能重新发起'
  if (task.source === 'data_sync') return '打开管理后台'
  if (task.kind === 'screener_parse') return '返回选股'
  if (task.status === 'processing') return '查看进度'
  return '查看结果'
}

export function taskRecoveryAdvice(task: TaskCenterItem): string {
  if (task.status === 'processing') return '任务仍在后台执行，可离开页面，完成后会自动更新。'
  if (task.status === 'success') return '结果已保存，可随时打开查看。'
  if (task.status === 'degraded') return '已保留可用的降级结果，打开后请留意限制说明。'
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
