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

// M3c 板块详情：指数日线 + 成分股（各块可缺，errors 记录哪块失败）。
export interface BoardDetail {
  code: string
  bars: Bar[]
  stocks: BoardStock[]
  errors: Record<string, string>
  data_time: string
}

export function getBoardDetail(market: string, code: string) {
  return request<BoardDetail>({ url: `/markets/${market}/boards/${code}`, method: 'get' })
}
