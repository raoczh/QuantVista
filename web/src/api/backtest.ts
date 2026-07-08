import { request } from './client'
import type { CondNode } from './screener'

// M2 回测时光机：条件树策略历史回测 + 历史推荐批次回验（纯本地计算无 LLM）。

export interface BacktestRequest {
  strategy_key?: string
  strategy_id?: number
  tree?: CondNode
  lookback_days?: number
  signal_count?: number
  hold_days?: number[]
  per_stock_cap?: number
  top_per_day?: number
  include_st?: boolean
}

export interface BacktestTrade {
  symbol: string
  name: string
  signal_date: string
  buy_date: string
  sell_date: string
  buy_price: number
  sell_price: number
  return_pct: number
  alpha_pct?: number
  deferred?: number
  forced?: boolean
}

export interface BacktestHoldStat {
  hold_days: number
  trades: number
  win_rate: number
  avg_return_pct: number
  median_return_pct: number
  best_pct: number
  worst_pct: number
  avg_alpha_pct: number
  bench_avg_pct: number
  alpha_sample: number
  skip_limit_up: number
  skip_cash: number
  skip_suspend: number
  deferred: number
  forced: number
  pending: number
  best_trades: BacktestTrade[] | null
  worst_trades: BacktestTrade[] | null
}

export interface BacktestDayStat {
  date: string
  matched: number
  taken: number
  avg_returns: Record<string, number>
  traded_by_hold: Record<string, number>
}

export interface BacktestResult {
  strategy: string
  conditions: string[] | null
  trade_date: string
  signal_dates: string[]
  universe: number
  adjust_suspect: number
  st_skipped: number
  stats: BacktestHoldStat[]
  days: BacktestDayStat[]
  notes: string[] | null
  elapsed_ms: number
}

export interface BatchHoldCell {
  status: string
  return_pct: number
  alpha_pct?: number
}

export interface BatchPickRow {
  batch_id: number
  batch_title: string
  type: string
  signal_date: string
  symbol: string
  name: string
  action: string
  holds: Record<string, BatchHoldCell>
}

export interface AlphaBucket {
  label: string
  count: number
}

export interface BatchHoldStat {
  hold_days: number
  trades: number
  win_rate: number
  avg_return_pct: number
  avg_alpha_pct: number
  alpha_sample: number
  pending: number
  skipped: number
  no_data: number
  alpha_hist: AlphaBucket[]
}

export interface BatchBacktestResult {
  batches: number
  picks: number
  stats: BatchHoldStat[]
  rows: BatchPickRow[] | null
  notes: string[] | null
}

export function runBacktest(req: BacktestRequest) {
  return request<BacktestResult>({ url: '/backtest/run', method: 'post', data: req, timeout: 120000 })
}

export function backtestRecommendations(batchId: number) {
  return request<BatchBacktestResult>({
    url: '/backtest/recommendations',
    method: 'post',
    data: { batch_id: batchId },
    timeout: 120000,
  })
}

export const HOLD_STATUS_LABEL: Record<string, string> = {
  traded: '成交',
  skip_limit_up: '一字板',
  skip_cash: '拨款不足',
  skip_suspend: '停牌',
  pending: '未走完',
  no_data: '无数据',
}
