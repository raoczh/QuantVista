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

export interface Overview {
  indices: Index[]
  gainers: StockRank[]
  actives: StockRank[]
  sectors: SectorRank[]
  errors: Record<string, string>
  data_time: string
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
