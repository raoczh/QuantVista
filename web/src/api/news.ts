import { request } from './client'

// 新闻/快讯条目。列表接口只返回轻字段（无 content 正文）。
// sentiment 由 N2 情绪增强回填（positive/negative/neutral，空=尚未增强）。
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

/**
 * 情绪标签展示（N2）：仅利好/利空显性展示（中性/未增强不渲染，避免满屏噪声标签）。
 * dir=1 利好走涨色、-1 利空走跌色（颜色由调用方按 useUi 的 upColor/downColor 取，兼容 6 主题）。
 */
export function sentimentTag(n: Pick<NewsItem, 'sentiment'>): { text: string; dir: 1 | -1 } | null {
  if (n.sentiment === 'positive') return { text: '利好', dir: 1 }
  if (n.sentiment === 'negative') return { text: '利空', dir: -1 }
  return null
}
