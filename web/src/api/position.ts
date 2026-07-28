import { request } from './client'

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
>

export function listPositions(status: 'holding' | 'closed' | 'all' = 'all') {
  return request<Position[]>({ url: '/positions', params: { status } })
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
  side: string // buy=加仓 / sell=减仓
  price: number
  quantity: number
  fee: number
  tax: number
  trade_date: string
  note: string
  realized_pnl: number // 该笔卖出结转的已实现盈亏（买入笔恒 0）
  avg_cost_after: number // 该笔之后的加权平均成本
  quantity_after: number // 该笔之后的持仓数量
  backfilled: boolean // 旧持仓惰性补建的等价首笔买入（非用户录入）
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
