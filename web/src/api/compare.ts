import { request } from './client'
import type { LLMTask } from './llmTask'
import type { EvidenceCheck } from './trust'

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
  valuation_ok: boolean
  is_fund?: boolean // ETF/场内基金（无个股估值指标）
  pe_ttm: number
  pb: number
  total_cap: number
  turnover_rate: number
  volume_ratio: number
  is_st: boolean
  error: string
  quote_as_of?: string // 行情数据源时刻
  freshness_status?: string // fresh | stale | unknown（非 fresh 行不参与对比结论与 AI 点评）
}

export interface CompareResult {
  rows: CompareRow[]
  ai_comment: string
  ai_comment_check?: EvidenceCheck // AI 点评引用数字与各行指标的核验（服务端回填）
  note: string
  // AI 点评未生成时的机读拒答码（行情不足、配额/配置、调用/完整性等）。
  ai_refusal_code?: string
  // AI 点评实际使用的 LLM（无点评时缺席）。
  ai_llm_config_id?: number
  ai_provider?: string
  ai_model?: string
  // P0-2 调用追溯 ID（管理端 LLM 调用记录页按 trace 可查本次调用）。
  ai_trace_id?: string
}

export interface CompareRequest {
  symbols: { symbol: string; market: string }[]
  with_ai?: boolean
  llm_config_id?: number
}

export function compareStocks(req: CompareRequest) {
  return request<CompareResult | LLMTask<CompareResult>>({ url: '/compare', method: 'post', data: req })
}
