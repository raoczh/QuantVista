import { request } from './client'

export interface StatusInfo {
  version: string
  uptime_sec: number
  db: boolean
  redis: boolean
  server_time: string
}

export interface Quote {
  symbol: string
  market: string
  name: string
  price: number
  change_pct: number
  open: number
  high: number
  low: number
  prev_close: number
  volume: number
  amount: number
  source: string
  data_time: string
  freshness?: QuoteFreshness
}

// 行情响应统一携带的新鲜度块：区分「请求成功」与「数据仍然有效」。
export interface QuoteFreshness {
  captured_at: string
  source_data_time?: string // 空=数据源未返回行情时间
  expected_as_of?: string // 期望的最近有效行情交易日
  source?: string
  market_state: string // trading | break | pre_open | post_close | closed
  freshness_status: string // fresh | stale | unknown
  stale_reason?: string
}

export interface Bar {
  trade_date: string
  open: number
  high: number
  low: number
  close: number
  volume: number
  amount: number
}

export interface Index {
  code: string
  name: string
  price: number
  change_pct: number
  open: number
  high: number
  low: number
  prev_close: number
  source: string
  data_time: string
}

export interface StockRank {
  symbol: string
  name: string
  price: number
  change_pct: number
  amount: number
  turnover_rate: number
  source: string
}

export interface SectorRank {
  code: string
  name: string
  change_pct: number
  leader: string
  source: string
}

export interface Breadth {
  advances: number
  declines: number
  unchanged: number
  limit_up: number
  limit_down: number
  trade_date: string
  source: string
  data_time: string
}

export interface MarketFundFlow {
  trade_date: string
  main_net: number
  super_net: number
  large_net: number
  medium_net: number
  small_net: number
  source: string
  data_time: string
}

export interface Overview {
  indices: Index[]
  gainers: StockRank[]
  actives: StockRank[]
  sectors: SectorRank[]
  breadth: Breadth | null
  fund_flow: MarketFundFlow | null
  errors: Record<string, string>
  data_time: string
}

export interface Valuation {
  symbol: string
  market: string
  name: string
  pe_ttm: number
  pe_dynamic: number
  pe_static: number
  pb: number
  total_cap: number
  float_cap: number
  turnover_rate: number
  amplitude: number
  volume_ratio: number
  limit_up: number
  limit_down: number
  is_st: boolean
  source: string
  data_time: string
}

export interface StockScore {
  symbol: string
  market: string
  name: string
  price: number
  trade_date: string
  total: number
  trend: number
  momentum: number
  position: number
  volume: number
  risk: number
  label: string
  bar_count: number
  data_limited: boolean
}

// T1 指标序列（与 K 线按日期对齐；null=该位置无值，如 BOLL 前 19 根）。
export interface IndicatorSeries {
  symbol: string
  market: string
  dates: string[]
  dif: (number | null)[]
  dea: (number | null)[]
  hist: (number | null)[] // 2×(DIF−DEA)，A 股柱口径
  boll_up: (number | null)[]
  boll_mid: (number | null)[]
  boll_low: (number | null)[]
  rsi: (number | null)[]
  atr: (number | null)[]
}

// T1 筹码分布（本地复算：210 根日线 + 换手率三角衰减）。
export interface ChipDay {
  date: string
  profit: number
  avg_cost: number
  c90_low: number
  c90_high: number
  conc_90: number
  c70_low: number
  c70_high: number
  conc_70: number
}

export interface ChipDist extends ChipDay {
  symbol: string
  market: string
  days: ChipDay[]
  prices: number[]
  chips: number[]
  last_close: number
  bar_count: number
  data_limited: boolean
}

export function getOverview(market = 'cn') {
  return request<Overview>({ url: `/markets/${market}/overview`, method: 'get' })
}

export function getStatus() {
  return request<StatusInfo>({ url: '/status', method: 'get' })
}

export function getQuote(market: string, symbol: string) {
  return request<Quote>({ url: `/markets/${market}/stocks/${symbol}/quote`, method: 'get' })
}

export function getDailyBars(market: string, symbol: string, limit = 120) {
  return request<Bar[]>({
    url: `/markets/${market}/stocks/${symbol}/bars`,
    method: 'get',
    params: { limit },
  })
}

// A1 个股分时线。成交量单位为手；均价为服务端基于分钟线估算的累计均价。
export interface MinutePoint {
  time: string
  price: number
  avg: number
  volume: number
}

export interface MinuteLine {
  symbol: string
  market: string
  trade_date: string
  prev_close: number
  base_from_open: boolean
  points: MinutePoint[]
  total_volume: number
  high: number
  low: number
  last: number
  avg_note: string
}

export function getMinuteLine(market: string, symbol: string) {
  return request<MinuteLine>({
    url: `/markets/${market}/stocks/${symbol}/minute`,
    method: 'get',
  })
}

// A2 盘面情绪。比例字段均为百分比数值，金额字段单位为元。
export interface MarketMoodDaily {
  market: string
  trade_date: string
  limit_up_count: number
  broken_count: number
  broken_rate: number
  max_streak: number
  streak_dist_json: string
  yzt_count: number
  yzt_avg_chg: number
  yzt_up_ratio: number
  seal_fund_top: number
}

export interface LimitUpStock {
  symbol: string
  market: string
  trade_date: string
  name: string
  price: number
  amount: number
  float_cap: number
  turnover_rate: number
  streak: number
  first_seal_at: number
  last_seal_at: number
  seal_fund: number
  break_count: number
  industry: string
  stat_days: number
  stat_count: number
}

export interface MoodTrendPoint {
  trade_date: string
  limit_up_count: number
  broken_count: number
  broken_rate: number
  max_streak: number
  yzt_avg_chg: number
  yzt_up_ratio: number
}

export interface StreakLadder {
  streak: number
  count: number
  stocks: LimitUpStock[]
}

export interface MoodOverview {
  market: string
  latest: MarketMoodDaily | null
  streak_dist: Record<string, number>
  streak_ladders: StreakLadder[]
  trend: MoodTrendPoint[]
  seal_fund_top: LimitUpStock[]
}

export function getMarketMood(market = 'cn', days = 20) {
  return request<MoodOverview>({
    url: `/markets/${market}/mood`,
    method: 'get',
    params: { days },
  })
}

// A3 全市场龙虎榜与人气榜。
export interface LhbDailyItem {
  symbol: string
  name: string
  reason: string
  note: string
  close: number
  change_pct: number
  net_buy: number
  buy_amt: number
  sell_amt: number
  deal_amt: number
  net_ratio: number
  turnover_rate: number
  org_net_buy: number
  org_buy_times: number
  org_sell_times: number
}

export interface LhbDaily {
  market: string
  trade_date: string
  items: LhbDailyItem[]
}

export function getMarketLhb(market = 'cn', date = '', limit = 50) {
  return request<LhbDaily>({
    url: `/markets/${market}/lhb`,
    method: 'get',
    params: { date: date || undefined, limit },
  })
}

export interface PopularityDailyItem {
  symbol: string
  name: string
  rank: number
  prev_rank: number
  is_new: boolean
}

export interface PopularityDaily {
  market: string
  trade_date: string
  items: PopularityDailyItem[]
}

export function getMarketPopularity(market = 'cn', date = '') {
  return request<PopularityDaily>({
    url: `/markets/${market}/popularity`,
    method: 'get',
    params: { date: date || undefined },
  })
}

export function getValuation(market: string, symbol: string) {
  return request<Valuation>({ url: `/markets/${market}/stocks/${symbol}/valuation`, method: 'get' })
}

export function getScore(market: string, symbol: string) {
  return request<StockScore>({ url: `/markets/${market}/stocks/${symbol}/score`, method: 'get' })
}

export function getIndicators(market: string, symbol: string, limit = 120) {
  return request<IndicatorSeries>({
    url: `/markets/${market}/stocks/${symbol}/indicators`,
    method: 'get',
    params: { limit },
  })
}

export function getChips(market: string, symbol: string) {
  return request<ChipDist>({ url: `/markets/${market}/stocks/${symbol}/chips`, method: 'get' })
}

// M3a 个股资金流（主力净额逐日 + 汇总；金额单位亿元）。
export interface FundFlowDay {
  date: string
  main_net_yi: number
  main_pct: number
  close: number
  change_pct: number
}

export interface StockFundFlow {
  symbol: string
  market: string
  days: FundFlowDay[]
  main_net_1d_yi: number
  main_net_5d_yi: number
  main_net_10d_yi: number
  main_net_20d_yi: number
  streak_days: number // 正=连续净流入天数，负=连续净流出
  fresh: boolean
  last_date?: string
}

export function getStockFundFlow(market: string, symbol: string, days = 90) {
  return request<StockFundFlow>({
    url: `/markets/${market}/stocks/${symbol}/fundflow`,
    method: 'get',
    params: { days },
  })
}

// M3a 龙虎榜上榜记录（金额单位元）。
export interface LhbRecord {
  trade_date: string
  reason: string
  note?: string
  change_pct: number
  net_buy: number
  deal_amt: number
  org_net_buy: number
  org_buys?: number
}

export function getStockLhb(market: string, symbol: string, limit = 10) {
  return request<LhbRecord[]>({
    url: `/markets/${market}/stocks/${symbol}/lhb`,
    method: 'get',
    params: { limit },
  })
}

// M3c 行业/概念板块热力图（面积=成交额、颜色=涨跌幅）。
export interface BoardHeat {
  code: string
  name: string
  change_pct: number
  amount: number // 成交额（元）
  advances: number
  declines: number
  leader: string
  leader_code: string
  source: string
}

export type BoardKind = 'industry' | 'concept'

export function getBoardHeatmap(market: string, kind: BoardKind = 'industry') {
  return request<BoardHeat[]>({
    url: `/markets/${market}/boards`,
    method: 'get',
    params: { kind },
  })
}

// M3c 板块成分股（is_leader=成交额龙头，is_top_gainer=涨幅第一）。
export interface BoardStock {
  symbol: string
  name: string
  price: number
  change_pct: number
  amount: number // 成交额（元）
  turnover_rate: number
  total_cap: number // 总市值（元）
  float_cap: number // 流通市值（元）
  is_leader: boolean
  is_top_gainer: boolean
  source: string
}

// P3b 板块估值（行业板块聚合表最新行；分位 -1=算不出，hist_days 为时序分位积累天数）。
export interface BoardValuation {
  trade_date: string
  board_name: string
  median_pe_ttm: number
  median_pb: number
  pos_pe_count: number
  stock_count: number
  pct_rank: number // 横截面分位（当日全行业，越高越贵）
  hist_pct_rank: number // 时序分位（自身近 ≤250 日）
  hist_days: number
}

// M3c 板块详情：指数日线 + 成分股 + 估值（各块可缺，errors 记录哪块失败；
// valuation 概念板块自然缺席）。
export interface BoardDetail {
  code: string
  bars: Bar[]
  stocks: BoardStock[]
  valuation?: BoardValuation
  errors: Record<string, string>
  data_time: string
}

export function getBoardDetail(market: string, code: string) {
  return request<BoardDetail>({ url: `/markets/${market}/boards/${code}`, method: 'get' })
}

// P3b 板块资金流历史（上游透传+短缓存；结构对齐个股 StockFundFlow，close 为板块指数点位）。
export interface BoardFundFlow {
  code: string
  days: FundFlowDay[]
  main_net_1d_yi: number
  main_net_5d_yi: number
  main_net_10d_yi: number
  main_net_20d_yi: number
  streak_days: number // 正=连续净流入天数，负=连续净流出
  last_date?: string
}

export function getBoardFundFlow(market: string, code: string, days = 90) {
  return request<BoardFundFlow>({
    url: `/markets/${market}/boards/${code}/fundflow`,
    method: 'get',
    params: { days },
  })
}
