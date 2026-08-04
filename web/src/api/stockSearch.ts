import { request } from './client'

export interface StockSearchItem {
  symbol: string
  market: string
  name: string
  industry?: string
  as_of: string
  in_watchlist: boolean
  has_position: boolean
}

export interface StockSearchResult {
  query: string
  source: string
  as_of: string
  items: StockSearchItem[]
}

export function searchStocks(q: string, limit = 20, signal?: AbortSignal) {
  return request<StockSearchResult>({
    url: '/stocks/search',
    method: 'get',
    params: { q, limit },
    signal,
  })
}
