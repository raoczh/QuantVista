import { request } from './client'
import type { PortfolioCurve } from './position'

export interface PaperAccount {
  id: number
  account_id: number
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
  remaining_cost?: number
  cost_basis_estimated?: boolean
  cost_basis_note?: string
  valuation_unavailable_reason?: string
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
  currency_unavailable_reason?: string // 非空时现金、资产及盈亏汇总不可用。
  realized_unavailable_reason?: string
  valuation_unavailable_reason?: string
}

export interface PaperTrade {
  id: number
  symbol: string
  market: string
  name: string
  side: 'buy' | 'sell' | 'adjust'
  price: number
  quantity: number
  amount: number
  fee: number
  tax: number
  realized_pnl: number
  trade_date: string // 业务发生日；除权补跑时可能早于 created_at
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

export function getPaperOverview(accountId?: number) {
  return request<PaperOverview>({ url: '/paper/overview', method: 'get', params: { account_id: accountId } })
}

export function paperTrade(input: TradeInput, accountId?: number) {
  return request<PaperTrade>({ url: '/paper/trade', method: 'post', data: input, params: { account_id: accountId } })
}

export function getPaperTrades(limit = 50, accountId?: number) {
  return request<PaperTrade[]>({ url: '/paper/trades', method: 'get', params: { limit, account_id: accountId } })
}

export function resetPaper(initialCash?: number, accountId?: number) {
  return request<PaperAccount>({ url: '/paper/reset', method: 'post', data: { initial_cash: initialCash }, params: { account_id: accountId } })
}

// B7 模拟盘资产曲线（读每交易日 16:20 落库的快照；partial 点表示当日有标的无有效行情）。
export function getPaperCurve(days = 90, signal?: AbortSignal, accountId?: number) {
  return request<PortfolioCurve>({ url: '/paper/curve', method: 'get', params: { days, account_id: accountId }, signal })
}
