import { request } from './client'

export type AlertKind = 'price' | 'pct_change' | 'ma' | 'breakout'
export type AlertOp = 'gte' | 'lte'
export type AlertStatus = 'active' | 'triggered' | 'paused'

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
