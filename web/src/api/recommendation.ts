import { request, AI_TIMEOUT } from './client'
import type { EvidenceCheck, TrustReview } from './trust'

// 信任层类型统一收敛到 trust.ts；此处 re-export 保持既有 import 路径不炸。
export type { EvidenceCheck } from './trust'
// 推荐域历史别名：PickReview = TrustReview（含可选 symbol，按标的复核）。
export type PickReview = TrustReview
export type { TrustReview }

export type RecType = 'short_term' | 'long_term'
export type RecStatus = 'success' | 'degraded' | 'failed'
export type RecAction = 'buy' | 'watch'

export interface Strategy {
  key: string
  name: string
  desc: string
}

// 候选筛选条件（阶段②用户硬过滤；0 = 不限）。
export interface RecFilters {
  price_min: number
  price_max: number
  float_cap_min_yi: number
  float_cap_max_yi: number
  turnover_min: number
  turnover_max: number
  max_gain_5d_pct: number
  exclude_limit_up: boolean
}

export function emptyRecFilters(): RecFilters {
  return {
    price_min: 0,
    price_max: 0,
    float_cap_min_yi: 0,
    float_cap_max_yi: 0,
    turnover_min: 0,
    turnover_max: 0,
    max_gain_5d_pct: 25,
    exclude_limit_up: true,
  }
}

export interface RecommendRequest {
  type: RecType
  market?: string
  strategy?: string
  llm_config_id?: number
  count?: number
  filters?: RecFilters // 不传 = 用偏好默认
  verify?: boolean // AI 复核员二次调用
}

// 候选池条目（透明化：来源/被筛原因/因子/量化分/排名全落库）。
export interface PoolCandidate {
  symbol: string
  market: string
  name: string
  price: number
  change_pct: number
  amount?: number
  pe_ttm?: number
  pb?: number
  total_cap?: number
  float_cap?: number
  turnover_rate?: number
  volume_ratio?: number
  amplitude?: number
  source?: string // 旧记录单来源
  sources?: string[]
  excluded?: string
  score?: number
  rank?: number
  bonus?: string[]
  sent_to_llm?: boolean
  factors?: {
    ma5?: number
    ma10?: number
    ma20?: number
    ma60?: number
    chg_5d: number
    chg_20d: number
    high_20d?: boolean
    bull_align?: boolean
    above_ma20?: boolean
    vol_boost?: number
    bias_20?: number
    volatility_20?: number
    drawdown_20?: number
    pos_60: number
    bar_count: number
  }
  score_dims?: { trend: number; momentum: number; position: number; volume: number; risk: number }
}

// 证据数字核验结果与 AI 复核结论类型统一由 trust.ts 提供（见文件顶部 re-export）。

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
  // 信任层（服务端回填）
  quant_score?: number
  quant_rank?: number
  pool_size?: number
  lot_cost?: number
  evidence_check?: EvidenceCheck
  sys_confidence?: 'high' | 'medium' | 'low'
  sys_confidence_why?: string
  review?: PickReview
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
  title?: string // 生成时由筛选条件组合固化（旧记录为空，前端回退策略名）
  status: RecStatus
  error: string
  candidate_count: number
  candidate_pool?: string // 候选池快照 JSON（详情接口返回，列表不含）
  rejected_json?: string // 池内落选理由 JSON（详情接口返回，列表不含）
  filters_json?: string // 本次生效筛选条件快照（详情接口返回）
  review_json?: string // AI 复核结论 JSON（verify 模式，详情接口返回）
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
