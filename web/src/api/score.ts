import { request } from './client'

export interface ScoreView {
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

// 个股综合评分（趋势/动量/位置/量能/风险 5 维加权，纯技术面）。
export function getStockScore(market: string, symbol: string) {
  return request<ScoreView>({ url: `/markets/${market}/stocks/${symbol}/score`, method: 'get' })
}
