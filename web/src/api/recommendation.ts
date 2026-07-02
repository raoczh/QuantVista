import { request, AI_TIMEOUT } from './client'

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

export type RecOutcome =
  | 'active'
  | 'take_profit'
  | 'stop_loss'
  | 'expired'
  | 'tracking'
  | 'no_data'

// 单条推荐的追踪状态（阶段6）。
export interface RecTracking {
  recommendation_id: number
  ref_price: number
  current_price: number
  period_high: number
  period_low: number
  return_pct: number
  max_gain_pct: number
  max_drawdown_pct: number
  bench_return_pct: number
  alpha_pct: number
  outcome: RecOutcome
  review_needed: boolean
  hit_take_profit: boolean
  hit_stop_loss: boolean
  elapsed_trade_days: number
  valid_days: number
  bars_count: number
  last_eval_date: string
  note: string
  updated_at: string
}

// 推荐对应的实际持仓（血缘：一键建仓时写入 positions.recommendation_id）。
export interface RecPositionLink {
  position_id: number
  buy_price: number
  buy_date: string
  quantity: number
  status: string // holding / closed
}

// 池内落选标的的一句话理由（「为什么没选它」）。
export interface RecReject {
  symbol: string
  name?: string
  reason: string
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
  status: RecTracking | null
  position: RecPositionLink | null
}

// 推荐历史表现统计（带样本量）。
export interface PerformanceStats {
  type: string
  sample: number
  win_rate: number
  avg_return_pct: number
  avg_alpha_pct: number
  avg_max_gain_pct: number
  avg_max_drawdown_pct: number
  take_profit: number
  stop_loss: number
  expired: number
  active: number
  bench_sample: number
  // 时间节点均值（推荐后第 N 交易日）。
  avg_7d_pct: number
  avg_14d_pct: number
  avg_30d_pct: number
  sample_7d: number
  sample_14d: number
  sample_30d: number
}

export interface RecommendationBatch {
  id: number
  type: RecType
  market: string
  strategy: string
  status: RecStatus
  error: string
  candidate_count: number
  rejected_json?: string // 池内落选理由 JSON（详情接口返回，列表不含）
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
  return request<RecommendationView>({ url: '/recommendations', method: 'post', data: req, timeout: AI_TIMEOUT })
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

// 手动刷新某批次的推荐追踪状态，返回最新详情（含 status）。
export function trackRecommendation(id: number) {
  return request<RecommendationView>({ url: `/recommendations/${id}/track`, method: 'post' })
}

// 推荐历史表现统计（带样本量）。
export function getPerformance(type?: string) {
  return request<PerformanceStats>({
    url: '/recommendations/performance',
    method: 'get',
    params: { type },
  })
}
