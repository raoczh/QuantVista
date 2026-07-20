import { request } from './client'

export interface PaperAccount {
  id: number
  user_id: number
  initial_cash: number
  cash: number
}

export interface PaperHolding {
  id: number
  symbol: string
  market: string
  name: string
  quantity: number
  avg_cost: number
  price: number
  quote_ok: boolean // 仅取到当前有效（fresh）行情时为 true；否则按成本估值
  cost: number
  market_value: number
  profit_amount: number
  profit_pct: number
  quote_as_of?: string // 行情数据源时刻（含 stale 的最近已知）
  freshness_status?: string // fresh | stale | unknown
  stale_reason?: string
  last_price?: number // 最近已知价（stale 展示用，不参与估值）
}

export interface PaperOverview {
  account: PaperAccount
  holdings: PaperHolding[]
  market_value: number
  total_assets: number
  total_profit: number
  total_profit_pct: number
  realized_pnl: number
  quote_stale_count?: number // 无当前有效行情、按成本估值的持仓数
  valuation_note?: string // 部分估值说明（总资产非全实时市值）
}

export interface PaperTrade {
  id: number
  symbol: string
  market: string
  name: string
  side: 'buy' | 'sell'
  price: number
  quantity: number
  amount: number
  fee: number
  tax: number
  realized_pnl: number
  created_at: string
}

export interface TradeInput {
  symbol: string
  market: string
  name?: string
  side: 'buy' | 'sell'
  price?: number
  quantity: number
}

export function getPaperOverview() {
  return request<PaperOverview>({ url: '/paper/overview', method: 'get' })
}

export function paperTrade(input: TradeInput) {
  return request<PaperTrade>({ url: '/paper/trade', method: 'post', data: input })
}

export function getPaperTrades(limit = 50) {
  return request<PaperTrade[]>({ url: '/paper/trades', method: 'get', params: { limit } })
}

export function resetPaper(initialCash?: number) {
  return request<PaperAccount>({ url: '/paper/reset', method: 'post', data: { initial_cash: initialCash } })
}
