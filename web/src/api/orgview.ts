// P3a 机构观点：卖方研报评级 + 机构调研（按需拉取+缓存，首次访问触发同步，冷却 1h）。
// 数据口径：研报为东财 reportapi 归一评级（买入/增持/中性/减持/卖出），近 400 天窗口；
// 调研为按调研日聚合（org_count=当日参与机构家数）。卖方评级普遍乐观，汇总里的
// 评级变动/目标价偏离/调研密度比"买入家数"更有参考价值。
import { request } from './client'

export interface ReportRatingItem {
  report_date: string
  org_name: string
  researcher: string
  title: string
  rating: string // 可空
  last_rating: string // 可空
  rating_change: number // 0=上调 1=下调 2=首次覆盖 3=维持 -1=缺失
  target_price: number // 元，0=该份研报未给目标价
}

export interface OrgSurveyItem {
  survey_date: string
  notice_date: string
  org_count: number
  org_names: string // 参与机构取样（逗号分隔）
  receive_way: string
}

export interface OrgViewSummary {
  rating_dist_90d?: Record<string, number>
  rating_dist_180d?: Record<string, number>
  rating_changes_90d?: { upgrades: number; downgrades: number; first_covers: number; note?: string }
  latest_rating_change?: { date: string; org: string; kind: string; from: string; to: string }
  target_price?: {
    count: number
    min: number
    median: number
    max: number
    median_vs_price_pct?: number // 现价缺失时不给
    note?: string
  }
  survey?: {
    batches_30d: number
    batches_90d: number
    batches_prev_90d: number
    latest_date: string
    latest_org_count: number
    note?: string
  }
  note?: string
}

export interface StockOrgView {
  summary: OrgViewSummary | null
  reports: ReportRatingItem[]
  surveys: OrgSurveyItem[]
}

export function getStockOrgView(market: string, symbol: string, price?: number) {
  return request<StockOrgView>({
    url: `/markets/${market}/stocks/${symbol}/orgview`,
    method: 'get',
    params: price && price > 0 ? { price } : undefined,
  })
}
