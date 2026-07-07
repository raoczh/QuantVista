import { request } from './client'

export type AlertKind = 'price' | 'pct_change' | 'ma' | 'breakout' | 'volume_surge' | 'amplitude' | 'earn_date' | 'earn_fcst'
export type AlertOp = 'gte' | 'lte'
export type AlertStatus = 'active' | 'triggered' | 'paused'
export type AlertEventStatus = 'unread' | 'read' | 'dismissed'

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
  triggered_at: string
  status: AlertEventStatus
  created_at: string
  updated_at: string
}

export function listAlertEvents(status?: string, limit?: number) {
  return request<AlertEvent[]>({ url: '/alerts/events', method: 'get', params: { status, limit } })
}

export function setAlertEventStatus(id: number, status: AlertEventStatus) {
  return request<AlertEvent>({ url: `/alerts/events/${id}/status`, method: 'put', data: { status } })
}

export function readAllAlertEvents() {
  return request<{ updated: number }>({ url: '/alerts/events/read-all', method: 'put' })
}
