import { request, AI_TIMEOUT } from './client'
import type { RecommendationView } from './recommendation'

// 收盘日报：交易日 15:35 后自动生成的「今日复盘 + 明日选股推荐」。

export interface DailyReview {
  summary: string
  market_review: string
  position_review: string
  watch_review: string
  risk_warnings: string[]
  tomorrow_plan: string
}

export interface DailyReportRow {
  id: number
  user_id: number
  trade_date: string
  market: string
  status: 'success' | 'partial' | 'failed'
  recommendation_batch_id: number
  error: string
  total_tokens: number
  latency_ms: number
  created_at: string
}

export interface DailyReportView extends DailyReportRow {
  review_json: string
  snapshot_json: string
  review: DailyReview | null
  recommendation: RecommendationView | null
}

export function listDailyReports(limit = 20) {
  return request<DailyReportRow[]>({ url: '/daily-reports', params: { limit } })
}

export function getDailyReport(id: number) {
  return request<DailyReportView>({ url: `/daily-reports/${id}` })
}

// 无日报时 data 为 null。
export function getLatestDailyReport() {
  return request<DailyReportView | null>({ url: '/daily-reports/latest' })
}

// 手动生成/重生成当日日报（计 1 次配额；AI 耗时较长）。
export function generateDailyReport() {
  return request<DailyReportView>({ url: '/daily-reports/generate', method: 'post', timeout: AI_TIMEOUT })
}
