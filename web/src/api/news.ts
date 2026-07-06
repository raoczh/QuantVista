import { request } from './client'

// 新闻/快讯条目。列表接口只返回轻字段（无 content 正文）。
// sentiment/sentiment_score 为 N2 情绪增强占位，本批恒为空。
export interface NewsItem {
  id: number
  title: string
  summary: string
  url: string
  source: string // cls / eastmoney
  category: string // telegraph / flash / stock
  publish_time: string
  related_symbols: string // JSON 数组字符串，元素为 6 位纯代码
  source_priority: number
  sentiment: string
  sentiment_score: number
  important_mark: boolean
}

export interface NewsQuery {
  symbol?: string
  source?: string
  limit?: number
}

export function getNews(params: NewsQuery = {}) {
  return request<NewsItem[]>({ url: '/news', method: 'get', params })
}

/** 解析 related_symbols JSON 数组，容错空串/坏 JSON。 */
export function parseRelatedSymbols(raw: string): string[] {
  if (!raw) return []
  try {
    const arr = JSON.parse(raw)
    return Array.isArray(arr) ? arr.filter((s): s is string => typeof s === 'string' && s !== '') : []
  } catch {
    return []
  }
}

/** 来源展示名：cls=财联社电报；eastmoney 按 category 细分快讯/个股新闻。 */
export function newsSourceLabel(n: Pick<NewsItem, 'source' | 'category'>): string {
  if (n.source === 'cls') return '财联社'
  if (n.source === 'eastmoney') return n.category === 'stock' ? '东财·个股' : '东财快讯'
  return n.source
}
