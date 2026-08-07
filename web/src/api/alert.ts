import { ApiRequestError, getApiErrorCode, request } from './client'

export type AlertKind =
  | 'price'
  | 'pct_change'
  | 'ma'
  | 'breakout'
  | 'volume_surge'
  | 'amplitude'
  | 'earn_date'
  | 'earn_fcst'
  // D14/D15 持仓卖出决策类：**唯一一组基于我的实际成本的提醒**。
  // symbol 可留空 = 绑定「我的全部持仓」；成本取 positions.buy_price，无需手工填价。
  | 'cost_gain' // 相对我的成本涨 ≥N%（考虑落袋）
  | 'cost_drawdown' // 相对我的成本跌 ≥N%（考虑止损）
  | 'peak_drawdown' // 自持仓期最高价回撤 ≥N%（移动止盈）
export type AlertOp = 'gte' | 'lte'
export type AlertStatus = 'active' | 'triggered' | 'paused'
export type AlertEventStatus = 'unread' | 'read' | 'dismissed'

/** 持仓卖出决策类 kind 集合（表单与展示按它分流）。 */
export const POSITION_ALERT_KINDS: AlertKind[] = ['cost_gain', 'cost_drawdown', 'peak_drawdown']
export function isPositionAlertKind(kind: string) {
  return POSITION_ALERT_KINDS.includes(kind as AlertKind)
}

export interface AlertRule {
  id: number
  user_id: number
  symbol: string
  market: string
  name: string
  kind: AlertKind
  op: AlertOp
  threshold: number
  period: number
  once: boolean
  note: string
  status: AlertStatus
  last_value: number
  last_check_date: string
  triggered_at: string | null
  trigger_msg: string
  created_at: string
  updated_at: string
}

export interface AlertInput {
  symbol?: string
  market?: string
  name?: string
  kind: AlertKind
  op: AlertOp
  threshold?: number
  period?: number
  once?: boolean
  note?: string
}

export function listAlerts(status?: string) {
  return request<AlertRule[]>({ url: '/alerts', method: 'get', params: { status } })
}

export function createAlert(input: AlertInput) {
  return request<AlertRule>({ url: '/alerts', method: 'post', data: input })
}

export function updateAlert(id: number, input: AlertInput) {
  return request<AlertRule>({ url: `/alerts/${id}`, method: 'put', data: input })
}

export function setAlertStatus(id: number, status: 'active' | 'paused') {
  return request<AlertRule>({ url: `/alerts/${id}/status`, method: 'put', data: { status } })
}

export function deleteAlert(id: number) {
  return request<{ ok: boolean }>({ url: `/alerts/${id}`, method: 'delete' })
}

export function evaluateAlerts() {
  return request<{ hits: number }>({ url: '/alerts/evaluate', method: 'post' })
}

// ---------- 命中明细事件（状态机 unread/read/dismissed） ----------

export interface AlertEvent {
  id: number
  rule_id: number
  user_id: number
  symbol: string
  market: string
  name: string
  kind: AlertKind
  message: string
  context_version: number
  context_available: boolean
  context?: AlertEventContext
  deep_link: string
  trade_date: string // 命中所属交易日（去重键的一部分；旧事件为空串）
  position_id: number // 持仓类命中的那一笔持仓（其余恒 0）
  triggered_at: string
  status: AlertEventStatus
  created_at: string
  updated_at: string
}

export interface AlertEventContext {
  version: number
  rule: {
    kind: string
    operator?: string
    threshold?: number
    period?: number
  }
  trigger: {
    field: string
    value?: number
    threshold?: number
    operator?: string
    unit?: string
    reason: string
  }
  quote?: {
    price?: number
    open?: number
    high?: number
    low?: number
    prev_close?: number
    change_pct?: number
    volume?: number
    source?: string
    as_of?: string
  }
  bar?: {
    trade_date?: string
    open?: number
    high?: number
    low?: number
    close?: number
    volume?: number
    source?: string
    sample_size?: number
  }
  indicator?: {
    name: string
    value?: number
    reference?: number
    period?: number
    unit?: string
    source?: string
    as_of?: string
  }
  position?: {
    position_id: number
    avg_cost?: number
    peak_price?: number
    peak_date?: string
    peak_from?: string
  }
  financial?: {
    fact_type: string
    report_date?: string
    appoint_date?: string
    notice_date?: string
    report_type?: string
    predict_type?: string
    predict_finance?: string
    amp_lower?: number
    amp_upper?: number
    source?: string
    as_of?: string
  }
  source?: string
  as_of?: string
  unknown?: string[]
}

export function listAlertEvents(status?: string, limit?: number) {
  return request<AlertEvent[]>({ url: '/alerts/events', method: 'get', params: { status, limit } })
}

export function getAlertEvent(id: number) {
  return request<AlertEvent>({ url: `/alerts/events/${id}`, method: 'get' })
}

export function setAlertEventStatus(id: number, status: AlertEventStatus) {
  return request<AlertEvent>({ url: `/alerts/events/${id}/status`, method: 'put', data: { status } })
}

export function readAllAlertEvents() {
  return request<{ updated: number }>({ url: '/alerts/events/read-all', method: 'put' })
}

export type AlertRequestStage =
  | 'rules'
  | 'stocks'
  | 'positions'
  | 'save'
  | 'events'
  | 'detail'
  | 'evaluate'
  | 'action'

/**
 * 提醒页只展示阶段化、可恢复的安全文案。后端原始正文可能包含内部依赖信息，
 * 因此这里仅依据受控的状态码/错误码分类，不直接拼接 error.message。
 */
export function alertRequestMessage(stage: AlertRequestStage, error: unknown): string {
  const code = getApiErrorCode(error)
  const status = error instanceof ApiRequestError ? error.status : undefined
  if (code === 'request_timeout') return '请求超时，当前内容已保留。请稍后重试。'
  if (status === 401) return '登录状态已失效，重新登录后可继续。'
  if (status === 403) return '当前账号无权执行这项操作。'
  if (stage === 'detail' && status === 404) return '这条命中记录不存在或已不可访问。'
  if (stage === 'save' && status != null && status >= 400 && status < 500) {
    return '保存未完成，请检查监控对象和参数后重试；已填写内容不会清空。'
  }
  const fallback: Record<AlertRequestStage, string> = {
    rules: '提醒规则加载失败，请重试。',
    stocks: '股票搜索失败，请重试当前关键词。',
    positions: '持仓加载失败，请重试。',
    save: '提醒保存失败，请稍后重试；已填写内容不会清空。',
    events: '命中记录加载失败，请重试。',
    detail: '命中详情加载失败，请重试。',
    evaluate: '立即检查未完成，请稍后重试。',
    action: '提醒操作未完成，请稍后重试。',
  }
  return fallback[stage]
}
