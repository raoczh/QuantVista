import { request } from './client'
import type { LLMTask } from './llmTask'
import type { EvidenceCheck } from './trust'

export interface Position {
  id: number
  user_id: number
  symbol: string
  market: string
  name: string
  position_type: string // short_term / long_term
  status: string // holding / closed
  currency: string
  buy_price: number
  buy_date: string
  quantity: number
  buy_fee: number
  buy_tax: number
  buy_reason: string
  user_note: string
  plan_stop_loss: number
  plan_take_profit: number
  checklist_json: string
  sell_price: number
  sell_date: string
  sell_fee: number
  sell_tax: number
  sell_reason: string
  review_note: string
  sell_planned: string // yes/no/partial
  ai_verdict: string // right/wrong/mixed/unused
  lesson_learned: string
  // B5 账本汇总（由 position_trades 流水回写）：
  // quantity/buy_price 恒为**当前剩余**持仓的数量与加权成本（全部卖出后数量为 0），
  // 「一共买过多少 / 一共赚了多少」看下面四个累计字段。
  realized_pnl: number // 累计已实现盈亏（元；持仓中也可能非 0=已部分兑现）
  total_buy_cost: number // 累计买入总成本（含买入费税）
  total_sell_net: number // 累计卖出净收入（已扣卖出费税）
  total_buy_qty: number // 累计买入股数
  remaining_cost: number // 当前剩余仓位精确成本余额（结转权威，避免均价舍入放大）
  recommendation_id: number // 来源推荐（0=手动建仓）
  // 富化字段
  current_price: number
  quote_ok: boolean // 仅取到当前有效（fresh）行情时为 true
  cost: number
  market_value: number
  profit_amount: number
  profit_pct: number
  realized: boolean
  day_change_pct: number // 当日涨跌幅 %（仅 fresh 时有值）
  // 行情时效契约块（fail-closed）
  quote_as_of?: string // 行情数据源时刻（含 stale 的最近已知）
  freshness_status?: string // fresh | stale | unknown
  stale_reason?: string // 非 fresh 的原因说明
  last_price?: number // 最近已知价（stale 展示用，不参与盈亏）
  held_trade_days: number // 已持有交易日（按交易日历）
  short_term_review: boolean // 短线持仓超阈值，建议复盘
  near_stop_loss: boolean // 现价距计划止损 ≤3%（未破）；仅 fresh 时判定
  below_stop_loss: boolean // 现价已跌破计划止损；仅 fresh 时判定
  last_analyzed_at: string | null // 该标的最近一次个股 AI 分析时间
  analysis_stale: boolean // 持仓中从未分析或距上次分析超过 7 天
  // D15 持仓期最高价与回撤（未初始化/已平仓时缺席）
  peak?: PositionPeak
  exit_assessment?: PositionExitAssessment
}

export type PositionExitLevel = 'normal' | 'watch' | 'review' | 'urgent' | 'unknown'

export interface PositionExitSignal {
  key: string
  label: string
  detail: string
  severity: PositionExitLevel
  value?: number
  threshold?: number
  crossing?: boolean
}

export interface PositionExitAssessment {
  id: number
  user_id: number
  position_id: number
  symbol: string
  market: string
  name: string
  trade_date: string
  session: 'intraday' | 'close'
  evaluated_at: string
  level: PositionExitLevel
  primary_signal: string
  primary_reason: string
  next_action: string
  data_status: 'ready' | 'partial' | 'unknown'
  trend: 'intact' | 'weak' | 'broken' | 'unknown'
  quote_as_of: string
  bars_as_of: string
  quote_price: number
  buy_price: number
  profit_pct: number
  peak_price: number
  peak_drawdown_pct: number
  ma20: number
  ma60: number
  atr14: number
  atr_line: number
  params_hash: string
  fact_hash: string
  version: string
  should_todo: boolean
  is_upgrade: boolean
  signals: PositionExitSignal[]
  evidence: string[]
  data_gaps: string[]
  alert_event_ids: number[]
  sell_review_ids: number[]
}

/**
 * D15 持仓期最高价。**口径**：自 from 起该标的到过的最高价（账面口径，除权除息同步折算）；
 * 加仓会把峰值重置为加仓价（成本已变，加仓前的高点不再是这本账赚到过的利润），减仓不重置。
 * drawdown_pct 仅在取到当前有效行情时有值；backfilled=true 表示由本地日线（前复权口径）
 * 回填，与账面实际成交价可能有出入，展示时须带 note 说明。
 */
export interface PositionPeak {
  price: number
  date: string
  from: string
  drawdown_pct: number
  backfilled: boolean
  note?: string
}

// C13 行业 / 市值风格 / 估值风格暴露。
// **available=false 表示该维度一条数据都没有（不知道），不是「分布均匀」**——整块不渲染。
// unknown=true 的桶是「数据缺失」桶，必须中性色 + 恒排最后，不得与真实取值混排。
export interface ExposureBucket {
  key: string
  label: string
  value: number // 市值（元）
  weight_pct: number // 占已定价持仓市值 %
  count: number // 标的数（同标的多笔仓算一只）
  unknown?: boolean
}

export interface ExposureDim {
  available: boolean
  buckets: ExposureBucket[]
  known_pct: number // 有归属的市值占比 %
  top_label?: string
  top_weight_pct?: number
  note?: string
}

export interface PortfolioExposure {
  base: number // 已定价持仓市值合计（元）
  base_note: string
  industry: ExposureDim
  cap_style: ExposureDim
  value_style: ExposureDim
  window_days?: number
  sample_count?: number
  as_of?: string
  factor_version?: string
  data_version?: string
}

export interface PortfolioOverview {
  holding_count: number
  total_cost: number
  total_value: number
  total_profit: number
  profit_pct: number
  realized_profit: number // 已平仓累计已实现盈亏
  win_count: number // 盈利仓数（持仓中）
  lose_count: number // 亏损仓数（持仓中）
  short_value: number // 短线市值
  long_value: number // 长线市值
  top_symbol: string
  top_name: string
  top_weight_pct: number // 最大单一持仓占比 %
  quote_failed_count: number // 行情拉取失败、未计入市值/收益的持仓数（部分估值口径）
  quote_stale_count: number // 行情已过期（非当前有效）、未计入市值/收益的持仓数
  exposure?: PortfolioExposure // C13；缺席=没有可定价的持仓
  signals: string[] // 风控信号（集中度/止损/未分析）
}

export interface PositionInput {
  symbol?: string
  market?: string
  name?: string
  position_type?: string
  currency?: string
  buy_price?: number
  buy_date?: string
  quantity?: number
  buy_fee?: number
  buy_tax?: number
  buy_reason?: string
  user_note?: string
  plan_stop_loss?: number
  plan_take_profit?: number
  checklist_json?: string
  recommendation_id?: number // 来源推荐（一键建仓带入）
}

export interface CloseInput {
  sell_price: number
  sell_date?: string
  sell_fee?: number
  sell_tax?: number
  sell_reason?: string
  review_note?: string
  sell_planned?: string
  ai_verdict?: string
  lesson_learned?: string
}

// 写接口（建仓/改仓/平仓）返回裸持仓模型，不含列表接口才回填的行情/收益富化字段。
export type PositionBase = Omit<
  Position,
  | 'current_price'
  | 'quote_ok'
  | 'cost'
  | 'market_value'
  | 'profit_amount'
  | 'profit_pct'
  | 'realized'
  | 'day_change_pct'
  | 'quote_as_of'
  | 'freshness_status'
  | 'stale_reason'
  | 'last_price'
  | 'held_trade_days'
  | 'short_term_review'
  | 'near_stop_loss'
  | 'below_stop_loss'
  | 'last_analyzed_at'
  | 'analysis_stale'
  | 'peak'
  | 'exit_assessment'
>

export function listPositions(status: 'holding' | 'closed' | 'all' = 'all') {
  return request<Position[]>({ url: '/positions', params: { status } })
}

export function getPositionExitAssessment(positionID: number, assessmentID: number) {
  return request<PositionExitAssessment>({
    url: `/positions/${positionID}/exit-assessments/${assessmentID}`,
  })
}

export function getPortfolioOverview() {
  return request<PortfolioOverview>({ url: '/positions/overview' })
}

export function createPosition(input: PositionInput) {
  return request<PositionBase>({ url: '/positions', method: 'post', data: input })
}

export function updatePosition(id: number, input: PositionInput) {
  return request<PositionBase>({ url: `/positions/${id}`, method: 'put', data: input })
}

export function closePosition(id: number, input: CloseInput) {
  return request<PositionBase>({ url: `/positions/${id}/close`, method: 'post', data: input })
}

export function deletePosition(id: number) {
  return request<{ ok: boolean }>({ url: `/positions/${id}`, method: 'delete' })
}

// ---------- B5 分批加仓 / 减仓 ----------

export interface PositionTrade {
  id: number
  user_id: number
  position_id: number
  side: string // buy=加仓 / sell=减仓 / adjust=除权除息折算（B8）
  price: number
  quantity: number
  fee: number
  tax: number
  trade_date: string
  note: string
  realized_pnl: number // 卖出结转的已实现盈亏；adjust 笔为到手现金分红（买入笔恒 0）
  avg_cost_after: number // 该笔之后的加权平均成本
  quantity_after: number // 该笔之后的持仓数量
  backfilled: boolean // 旧持仓惰性补建的等价首笔买入（非用户录入）
  // B8 折算审计：仅 adjust 笔有效（0 值在其它笔上无意义，勿反推）
  avg_cost_before?: number
  quantity_before?: number
  corporate_action_id?: number
  adjust_id?: number
  created_at: string
  updated_at: string
}

export interface PositionTradeInput {
  side: string
  price: number
  quantity: number
  fee?: number
  tax?: number
  trade_date?: string
  note?: string
  // 减到 0 自动平仓时沿用的复盘字段（可选）
  sell_reason?: string
  review_note?: string
  sell_planned?: string
  ai_verdict?: string
  lesson_learned?: string
}

export function listPositionTrades(id: number) {
  return request<PositionTrade[]>({ url: `/positions/${id}/trades` })
}

export function addPositionTrade(id: number, input: PositionTradeInput) {
  return request<PositionBase>({ url: `/positions/${id}/trades`, method: 'post', data: input })
}

// ---------- B6 个人交易复盘统计 ----------

export interface TradeStatBucket {
  key: string
  label: string
  trades: number
  win_count: number
  win_rate: number
  realized_pnl: number
  avg_return_pct: number
  unknown?: boolean // 该组是「数据缺失」而不是一个真实取值
}

export interface TradeStatTop {
  position_id: number
  symbol: string
  market: string
  name: string
  realized_pnl: number
  return_pct: number
  buy_date: string
  sell_date: string
  hold_trade_days: number
  sell_planned: string
  ai_verdict: string
}

export interface TradeStatLesson {
  position_id: number
  symbol: string
  name: string
  sell_date: string
  realized_pnl: number
  lesson: string
  sell_planned: string
  ai_verdict: string
}

export interface TradeStats {
  range: string
  range_from?: string
  first_sell?: string
  last_sell?: string
  closed: number
  total_realized_pnl: number
  win_count: number
  loss_count: number
  flat_count: number
  win_rate: number
  avg_win: number
  avg_loss: number
  // 盈亏比：无亏损交易时为 null（分母为 0，不是 0 也不是无穷大）
  profit_factor: number | null
  avg_hold_trade_days: number
  hold_sample: number
  by_industry: TradeStatBucket[]
  by_hold_bucket: TradeStatBucket[]
  by_buy_reason: TradeStatBucket[]
  by_ai_verdict: TradeStatBucket[]
  by_sell_planned: TradeStatBucket[]
  top_winners: TradeStatTop[]
  top_losers: TradeStatTop[]
  lessons: TradeStatLesson[]
  notes: string[]
}

export function getTradeStats(range = 'all') {
  return request<TradeStats>({ url: '/positions/stats', params: { range } })
}

// ---------- B7 资产曲线 ----------

export interface PortfolioCurvePoint {
  trade_date: string
  market_value: number
  cost: number
  unrealized_pnl: number
  realized_cum: number
  cash: number
  total_assets: number // paper=现金+市值；real=市值
  position_count: number
  partial: boolean // 当日有标的无有效行情，未计入市值——该点不是完整净值
  missing_count: number
  note?: string
}

export interface PortfolioCurve {
  kind: string
  days: number
  points: PortfolioCurvePoint[]
  partial_count: number
  notes: string[]
}

export function getPositionCurve(days = 90, signal?: AbortSignal) {
  return request<PortfolioCurve>({ url: '/positions/curve', params: { days }, signal })
}

// ---------- B8 除权除息持仓调整 ----------

// 调整建议状态机：pending 待确认 → confirmed 已写入账本 → reverted 已撤销（可再确认）
// / dismissed 已忽略（终态）。
export type CorpAdjustStatus = 'pending' | 'confirmed' | 'reverted' | 'dismissed'

export interface PositionCorpAdjust {
  id: number
  user_id: number
  position_id: number
  corporate_action_id: number
  symbol: string
  market: string
  name: string
  ex_date: string
  record_date: string
  // 方案（每 10 股口径，勿预先除以 10）
  bonus_ratio: number
  transfer_ratio: number
  dividend_pretax: number
  plan_profile: string
  // 折算前后账面
  qty_before: number
  entitled_qty: number // 股权登记日收盘时实际有权数量；除权日后买入的份额不参与本次权益
  qty_after: number
  cost_before: number
  cost_after: number
  cash_dividend: number // 到手税前现金分红（元）
  manual_review: boolean // 历史流水无法安全自动倒序折算，只能人工核对后忽略
  review_reason: string
  status: CorpAdjustStatus
  trade_id: number
  confirmed_at: string | null
  reverted_at: string | null
  created_at: string
  updated_at: string
}

export function listCorpAdjusts(status = 'pending', signal?: AbortSignal) {
  return request<PositionCorpAdjust[]>({ url: '/positions/corp-adjusts', params: { status }, signal })
}

// 确认 / 撤销 / 忽略。撤销仅在「当前账面仍等于折算结果且其后无新交易」时被接受，
// 否则后端明确拒绝并给出原因（不做部分回滚）。
export function actCorpAdjust(id: number, action: 'confirm' | 'revert' | 'dismiss') {
  return request<PositionCorpAdjust>({ url: `/positions/corp-adjusts/${id}/${action}`, method: 'post' })
}

// ---------- D16 卖出复核 ----------

/** 触发类型：解禁 / 业绩预告变脸 / 跌破关键均线 / 龙虎榜净卖出 / 除权除息临近。 */
export type SellReviewTrigger = 'lift' | 'earn_fcst' | 'ma_break' | 'lhb_sell' | 'ex_div'
export type SellReviewStatus = 'open' | 'resolved' | 'dismissed'

/**
 * 持仓命中利空事件时自动生成的「该不该卖」复核项。**与条件提醒的区别**：
 * 用户无需配置任何规则；detail 回答的是「这件事对**我这笔持仓**意味着什么」（含我的成本与浮盈亏）。
 * quote_ok=false 时 price/profit_pct 恒为 0 且 detail 已如实声明行情不可用（不用旧价冒充）。
 */
export interface SellReview {
  id: number
  user_id: number
  position_id: number
  symbol: string
  market: string
  name: string
  trigger: SellReviewTrigger
  trade_date: string // 事件自身日期，不是扫描日
  severity: 'high' | 'med' | 'low'
  title: string
  detail: string
  buy_price: number
  quantity: number
  price: number
  profit_pct: number
  quote_ok: boolean
  status: SellReviewStatus
  resolved_at: string | null
  created_at: string
  updated_at: string
}

export function listSellReviews(status: SellReviewStatus | 'all' = 'open', signal?: AbortSignal) {
  return request<SellReview[]>({ url: '/positions/sell-reviews', params: { status }, signal })
}

export function setSellReviewStatus(id: number, status: SellReviewStatus) {
  return request<SellReview>({ url: `/positions/sell-reviews/${id}/status`, method: 'put', data: { status } })
}

// ---------- D17 AI 持有 / 减仓 / 清仓建议 ----------

/** 封闭三值枚举——服务端强校验，模型输出越界的整条丢弃（绝不代填 hold）。 */
export type PositionVerdict = 'hold' | 'trim' | 'exit'

export interface PositionAdvice {
  position_id: number
  symbol: string
  name?: string
  position_type: string
  cost: number
  quantity: number
  verdict: PositionVerdict
  reason: string
  invalidation: string
}

export interface PositionAdviceResult {
  advices: PositionAdvice[]
  analyzed: number
  skipped: number // 因无当前有效行情未参与分析的仓数（fail-closed）
  notes: string[]
  evidence_check?: EvidenceCheck
  llm_config_id?: number
  provider?: string
  model?: string
  trace_id?: string
  prompt_version?: string
  generated_at: string
}

/** 发起建议（后台任务，秒回任务 id；用 getLLMTask 轮询结果）。 */
export function requestPositionAdvice(input: { llm_config_id?: number; symbol?: string; position_id?: number } = {}) {
  return request<LLMTask<PositionAdviceResult>>({ url: '/positions/advice', method: 'post', data: input })
}

export const POSITION_VERDICT_LABEL: Record<PositionVerdict, string> = {
  hold: '继续持有',
  trim: '减仓',
  exit: '清仓',
}
