import { request } from './client'

export interface Position {
  id: number
  user_id: number
  symbol: string
  market: string
  name: string
  position_type: string // short_term / long_term
  status: string // holding / closed
  currency: string
  buy_price: number
  buy_date: string
  quantity: number
  buy_fee: number
  buy_tax: number
  buy_reason: string
  user_note: string
  plan_stop_loss: number
  plan_take_profit: number
  checklist_json: string
  sell_price: number
  sell_date: string
  sell_fee: number
  sell_tax: number
  sell_reason: string
  review_note: string
  sell_planned: string // yes/no/partial
  ai_verdict: string // right/wrong/mixed/unused
  lesson_learned: string
  // 富化字段
  current_price: number
  quote_ok: boolean
  cost: number
  market_value: number
  profit_amount: number
  profit_pct: number
  realized: boolean
  held_trade_days: number // 已持有交易日（按交易日历）
  short_term_review: boolean // 短线持仓超阈值，建议复盘
}

export interface PositionInput {
  symbol?: string
  market?: string
  name?: string
  position_type?: string
  currency?: string
  buy_price?: number
  buy_date?: string
  quantity?: number
  buy_fee?: number
  buy_tax?: number
  buy_reason?: string
  user_note?: string
  plan_stop_loss?: number
  plan_take_profit?: number
  checklist_json?: string
}

export interface CloseInput {
  sell_price: number
  sell_date?: string
  sell_fee?: number
  sell_tax?: number
  sell_reason?: string
  review_note?: string
  sell_planned?: string
  ai_verdict?: string
  lesson_learned?: string
}

export function listPositions(status: 'holding' | 'closed' | 'all' = 'all') {
  return request<Position[]>({ url: '/positions', params: { status } })
}

export function createPosition(input: PositionInput) {
  return request<Position>({ url: '/positions', method: 'post', data: input })
}

export function updatePosition(id: number, input: PositionInput) {
  return request<Position>({ url: `/positions/${id}`, method: 'put', data: input })
}

export function closePosition(id: number, input: CloseInput) {
  return request<Position>({ url: `/positions/${id}/close`, method: 'post', data: input })
}

export function deletePosition(id: number) {
  return request<{ ok: boolean }>({ url: `/positions/${id}`, method: 'delete' })
}
