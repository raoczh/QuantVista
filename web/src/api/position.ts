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
  recommendation_id: number // 来源推荐（0=手动建仓）
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
  near_stop_loss: boolean // 现价距计划止损 ≤3%（未破）
  below_stop_loss: boolean // 现价已跌破计划止损
  last_analyzed_at: string | null // 该标的最近一次个股 AI 分析时间
  analysis_stale: boolean // 持仓中从未分析或距上次分析超过 7 天
}

export interface PortfolioOverview {
  holding_count: number
  total_cost: number
  total_value: number
  total_profit: number
  profit_pct: number
  realized_profit: number // 已平仓累计已实现盈亏
  win_count: number // 盈利仓数（持仓中）
  lose_count: number // 亏损仓数（持仓中）
  short_value: number // 短线市值
  long_value: number // 长线市值
  top_symbol: string
  top_name: string
  top_weight_pct: number // 最大单一持仓占比 %
  signals: string[] // 风控信号（集中度/止损/未分析）
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
  recommendation_id?: number // 来源推荐（一键建仓带入）
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

// 写接口（建仓/改仓/平仓）返回裸持仓模型，不含列表接口才回填的行情/收益富化字段。
export type PositionBase = Omit<
  Position,
  | 'current_price'
  | 'quote_ok'
  | 'cost'
  | 'market_value'
  | 'profit_amount'
  | 'profit_pct'
  | 'realized'
  | 'held_trade_days'
  | 'short_term_review'
  | 'near_stop_loss'
  | 'below_stop_loss'
  | 'last_analyzed_at'
  | 'analysis_stale'
>

export function listPositions(status: 'holding' | 'closed' | 'all' = 'all') {
  return request<Position[]>({ url: '/positions', params: { status } })
}

export function getPortfolioOverview() {
  return request<PortfolioOverview>({ url: '/positions/overview' })
}

export function createPosition(input: PositionInput) {
  return request<PositionBase>({ url: '/positions', method: 'post', data: input })
}

export function updatePosition(id: number, input: PositionInput) {
  return request<PositionBase>({ url: `/positions/${id}`, method: 'put', data: input })
}

export function closePosition(id: number, input: CloseInput) {
  return request<PositionBase>({ url: `/positions/${id}/close`, method: 'post', data: input })
}

export function deletePosition(id: number) {
  return request<{ ok: boolean }>({ url: `/positions/${id}`, method: 'delete' })
}
