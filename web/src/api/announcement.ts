import { request } from './client'

// 个股公告（F1）：后台按自选∪持仓每日采集，查询时库中无该股记录会按需补拉一次。
export interface AnnouncementItem {
  id: number
  symbol: string
  market: string
  art_code: string
  name: string
  title: string
  notice_type: string
  notice_date: string // YYYY-MM-DD
  url: string
}

export function getAnnouncements(symbol: string, limit = 20) {
  return request<AnnouncementItem[]>({ url: '/announcements', method: 'get', params: { symbol, limit } })
}
