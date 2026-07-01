import { request } from './client'

export type RecType = 'short_term' | 'long_term'
export type RecStatus = 'success' | 'degraded' | 'failed'
export type RecAction = 'buy' | 'watch'

export interface Strategy {
  key: string
  name: string
  desc: string
}

export interface RecommendRequest {
  type: RecType
  market?: string
  strategy?: string
  llm_config_id?: number
  count?: number
}

// 单条推荐的结构化明细（短线/长线字段并存）。
export interface RecDetail {
  symbol: string
  action: RecAction
  confidence: number
  reason: string[]
  risks: string[]
  evidence: string[]
  // 短线
  buy_zone_low: number
  buy_zone_high: number
  take_profit: number
  stop_loss: number
  valid_days: number
  invalidation: string
  // 长线
  thesis: string
  valuation_low: number
  valuation_high: number
  key_metrics: string[]
  review_cycle: string
  disclaimer: string
}

export interface RecommendationItem {
  id: number
  batch_id: number
  symbol: string
  market: string
  name: string
  action: RecAction
  confidence: number
  summary: string
  ref_price: number
  sort_order: number
  detail: RecDetail | null
}

export interface RecommendationBatch {
  id: number
  type: RecType
  market: string
  strategy: string
  status: RecStatus
  error: string
  candidate_count: number
  provider: string
  model: string
  prompt_version: string
  strategy_version: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  latency_ms: number
  created_at: string
}

export interface RecommendationView extends RecommendationBatch {
  items: RecommendationItem[]
}

export function listStrategies(type: RecType) {
  return request<Strategy[]>({ url: '/recommendations/strategies', method: 'get', params: { type } })
}

export function generateRecommendations(req: RecommendRequest) {
  return request<RecommendationView>({ url: '/recommendations', method: 'post', data: req })
}

export function listRecommendations(type?: string, limit = 30) {
  return request<RecommendationBatch[]>({
    url: '/recommendations',
    method: 'get',
    params: { type, limit },
  })
}

export function getRecommendation(id: number) {
  return request<RecommendationView>({ url: `/recommendations/${id}`, method: 'get' })
}

export function deleteRecommendation(id: number) {
  return request<{ ok: boolean }>({ url: `/recommendations/${id}`, method: 'delete' })
}
