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
  quote_ok: boolean
  cost: number
  market_value: number
  profit_amount: number
  profit_pct: number
}

export interface PaperOverview {
  account: PaperAccount
  holdings: PaperHolding[]
  market_value: number
  total_assets: number
  total_profit: number
  total_profit_pct: number
  realized_pnl: number
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
