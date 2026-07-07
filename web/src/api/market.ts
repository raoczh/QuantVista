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
