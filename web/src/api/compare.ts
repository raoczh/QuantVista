import { request, AI_TIMEOUT } from './client'

export interface CompareRow {
  symbol: string
  market: string
  name: string
  quote_ok: boolean
  price: number
  change_pct: number
  amount: number
  ma5: number
  ma10: number
  ma20: number
  period_high: number
  period_low: number
  change_pct_5d: number
  change_pct_20d: number
  above_ma20: boolean
  score: number
  score_label: string
  error: string
}

export interface CompareResult {
  rows: CompareRow[]
  ai_comment: string
  note: string
}

export interface CompareRequest {
  symbols: { symbol: string; market: string }[]
  with_ai?: boolean
  llm_config_id?: number
}

export function compareStocks(req: CompareRequest) {
  return request<CompareResult>({ url: '/compare', method: 'post', data: req, timeout: AI_TIMEOUT })
}
