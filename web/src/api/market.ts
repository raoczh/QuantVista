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
