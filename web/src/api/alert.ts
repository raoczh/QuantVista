import { request } from './client'

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
