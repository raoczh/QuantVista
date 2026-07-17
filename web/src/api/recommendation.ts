import { request } from './client'
import type { EvidenceCheck, TrustReview } from './trust'

// 信任层类型统一收敛到 trust.ts；此处 re-export 保持既有 import 路径不炸。
export type { EvidenceCheck } from './trust'
// 推荐域历史别名：PickReview = TrustReview（含可选 symbol，按标的复核）。
export type PickReview = TrustReview
export type { TrustReview }

export type RecType = 'short_term' | 'long_term'
// processing：异步任务化（2026-07-14）——生成接口立即返回任务批次，后台完成后回写。
export type RecStatus = 'processing' | 'success' | 'degraded' | 'failed'
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
  exclude_gem_star: boolean // 排除创业板(30)/科创板(68)，仅推荐主板普通个股
}

export function emptyRecFilters(): RecFilters {
  return {
    price_min: 0,
    price_max: 50,
    float_cap_min_yi: 0,
    float_cap_max_yi: 0,
    turnover_min: 0,
    turnover_max: 0,
    max_gain_5d_pct: 25,
    exclude_limit_up: true,
    exclude_gem_star: false,
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
  bear_check?: boolean // S2-2 反方研究员（影子）：不传时后端默认关联 verify
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
  // S1-2 建议仓位（占总资金 %，服务端目标波动模型程序计算；0/缺失=无法给出）
  position_pct?: number
  position_why?: string
  // S2-2 反方研究员结论（影子：只展示，不改写动作/置信度）
  bear?: PickBear
  // S2-3 数据质量门控影子输出（只记录 would-be 封顶与缺失面，不实际封顶）
  quality_gate?: QualityGateShadow
  // 非空 = 降级生成（quant_fallback：AI 精选超时后按量化排名规则合成，未经 AI 解读）
  degraded_source?: string
}

// S2-2 反方研究员对单条 buy 的最强 bear case。
export interface PickBear {
  symbol: string
  bear_case: string
  severity: 'high' | 'med' | 'low'
}

// S2-3 数据质量门控影子输出。
export interface QualityGateShadow {
  would_be_confidence_cap: number
  missing_critical_fields?: string[]
  senti_missing?: boolean // 情绪数据缺失（≠情绪中性）
  data_age_days: number
  quality_gate_version: string
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
  // S0-4 用户执行事实（持仓血缘）：实际买入价与实际收益，与模拟口径并列不混算。
  actual_buy_price?: number
  actual_return_pct?: number | null
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
  // S0-4 买入成熟口径（主指标）：胜率只统计 action=buy 且已成熟的样本。
  buy_matured: number
  buy_win_rate: number
  buy_avg_return_pct: number
  buy_median_pct: number
  buy_avg_alpha_pct: number
  buy_bench_sample: number
  buy_active: number
  watch_sample: number
  watch_win_rate: number
  degraded_excluded: number
  // 时间节点均值（推荐后第 N 交易日）。
  avg_7d_pct: number
  avg_14d_pct: number
  avg_30d_pct: number
  sample_7d: number
  sample_14d: number
  sample_30d: number
}

// S0-6 确定性错误归因报表。
export interface AttributionCell {
  dim: string
  key: string
  sample: number
  win_rate: number
  avg_net_pct: number
  median_net_pct: number
  p10_net_pct: number
  severe_loss_pct: number
  avg_alpha_pct: number
  alpha_sample: number
}

export interface AttributionReport {
  type: string
  horizon_days: number
  sample: number
  sample_buy: number
  skipped: number
  pending: number
  groups: AttributionCell[] | null
  notes: string[]
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
  // S1-1 市场状态三档判定（offense/neutral/defense；空=旧记录/数据不足）。影子模式：
  // 只展示不改写 action；regime_json 含判定依据明细与仓位模型参数（详情接口返回）。
  regime?: string
  regime_json?: string
  candidate_pool?: string // 候选池快照 JSON（详情接口返回，列表不含）
  rejected_json?: string // 池内落选理由 JSON（详情接口返回，列表不含）
  filters_json?: string // 本次生效筛选条件快照（详情接口返回）
  review_json?: string // AI 复核结论 JSON（verify 模式，详情接口返回）
  llm_config_id?: number // 生成时使用的 LLM 配置 id（配置名前端按自己的清单解析）
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

// 生成推荐。2026-07-14 异步任务化：接口立即返回 processing 批次（建池/评分/AI 精选
// 在服务端后台执行），轮询 getRecommendation 直到脱离 processing——不再需要超长前端超时。
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

// 手动刷新某批次的推荐追踪状态，返回最新详情（含 status）。
// 独立 60s 超时：逐条拉日线+实时行情（服务端已并发 4），全局 20s 对多标的批次不够。
export function trackRecommendation(id: number) {
  return request<RecommendationView>({ url: `/recommendations/${id}/track`, method: 'post', timeout: 60000 })
}

// 推荐历史表现统计（带样本量）。
export function getPerformance(type?: string) {
  return request<PerformanceStats>({
    url: '/recommendations/performance',
    method: 'get',
    params: { type },
  })
}

// 标记推荐复盘提示已读（今日待办 rec_review 条目就地消项；statusId=追踪状态行 id）。
export function ackRecommendationReview(statusId: number) {
  return request<{ ok: boolean }>({ url: `/recommendations/review-ack/${statusId}`, method: 'put' })
}

// S1-4 执行纪律：对推荐条目的止损价一键创建到价提醒（price/lte，命中自动暂停）。
export function createStopLossAlert(itemId: number) {
  return request<{ id: number }>({ url: `/recommendations/items/${itemId}/stop-alert`, method: 'post' })
}

// S0-6 确定性错误归因报表（成熟标签按入场特征桶×regime×策略×来源×行业分组）。
export function getAttribution(type?: string, horizon = 10) {
  return request<AttributionReport>({
    url: '/recommendations/attribution',
    method: 'get',
    params: { type, horizon },
  })
}

// S2-4 影子门控对照：单一 gate_type 的 gated vs ungated 统计。
export interface ShadowGateGroup {
  gate_type: string
  gate_label: string
  marked: number
  would_rewrite: number
  gated: AttributionCell
  ungated: AttributionCell
}

// S2-4 影子门控对照报表（闸门/反方/质量门控转正评审的数据地基）。
export interface ShadowReport {
  type: string
  horizon_days: number
  picked_buy: number
  picked_buy_matured: number
  groups: ShadowGateGroup[] | null
  notes: string[]
}

// S2-4 影子门控对照报表（gated vs ungated 成熟收益分布 + 覆盖率）。
export function getShadowReport(type?: string, horizon = 10) {
  return request<ShadowReport>({
    url: '/recommendations/shadow-report',
    method: 'get',
    params: { type, horizon },
  })
}

// S3-2 收益分布摘要。
export interface RecallDist {
  n: number
  mean_pct: number
  median_pct: number
  p10_pct: number
  p90_pct: number
}

// S3-2 来源消融行。
export interface RecallSourceAblation {
  source: string
  label: string
  pool_count: number
  recall_pct: number
  ablated_pct: number
  drop_pct: number
}

// S3-2 单批次召回明细行。
export interface RecallBatchRow {
  batch_id: number
  signal_date: string
  opp_size: number
  pool_size: number
  k_eff: number
  hit_pool: number
  hit_llm: number
  hit_picked: number
}

// S3-2 候选池召回评估报表（Recall@K / 来源消融 / 错失机会率 / 收益分布对比）。
export interface RecallReport {
  type: string
  horizon_days: number
  k: number
  batches: number
  recall_pool_pct: number
  recall_llm_pct: number
  recall_picked_pct: number
  topk_stage_counts: Record<string, number>
  missed_rate_pct: number
  missed_labels?: RecallDist
  opp_dist: RecallDist
  pool_dist: RecallDist
  source_ablation: RecallSourceAblation[] | null
  batch_rows: RecallBatchRow[] | null
  notes: string[]
  elapsed_ms: number
}

// S3-2 候选池召回评估（数秒级重活，服务端全局互斥）。
export function getRecallReport(type?: string, horizon = 10, k = 50) {
  return request<RecallReport>({
    url: '/recommendations/recall-report',
    method: 'get',
    params: { type, horizon, k },
  })
}
