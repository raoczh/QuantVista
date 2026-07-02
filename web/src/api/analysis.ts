import { request, AI_TIMEOUT } from './client'

export type AnalysisModule = 'market' | 'sector' | 'stock' | 'watchlist' | 'position'
export type AnalysisStatus = 'success' | 'degraded' | 'failed'
export type AnalysisRating = 'bullish' | 'neutral' | 'bearish'

export interface AnalyzeRequest {
  module: AnalysisModule
  market?: string
  symbol?: string
  target?: string
  llm_config_id?: number
  question?: string
}

// 结构化分析结果。
export interface AnalysisResult {
  rating: AnalysisRating
  confidence: number
  summary: string
  highlights: string[]
  risks: string[]
  opportunities: string[]
  suggestions: string[]
  disclaimer: string
}

// 分析记录（列表项不含 result_json/data_snapshot）。
export interface AnalysisRecord {
  id: number
  module: AnalysisModule
  market: string
  symbol: string
  target: string
  title: string
  status: AnalysisStatus
  rating: AnalysisRating | ''
  confidence: number
  summary: string
  error: string
  provider: string
  model: string
  prompt_version: string
  strategy_version: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  latency_ms: number
  created_at: string
  // 详情附带：
  result_json?: string
  data_snapshot?: string
}

// 详情/发起返回的视图。
export interface AnalysisView extends AnalysisRecord {
  result: AnalysisResult | null
  raw: string
}

export function createAnalysis(req: AnalyzeRequest) {
  return request<AnalysisView>({ url: '/analysis', method: 'post', data: req, timeout: AI_TIMEOUT })
}

export function listAnalysis(module?: string, limit = 30) {
  return request<AnalysisRecord[]>({
    url: '/analysis',
    method: 'get',
    params: { module, limit },
  })
}

export function getAnalysis(id: number) {
  return request<AnalysisView>({ url: `/analysis/${id}`, method: 'get' })
}

export function deleteAnalysis(id: number) {
  return request<{ ok: boolean }>({ url: `/analysis/${id}`, method: 'delete' })
}
