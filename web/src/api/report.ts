import { request } from './client'
import type { RecommendationView } from './recommendation'
import type { EvidenceCheck } from './trust'

// 收盘日报：交易日 15:35 后自动生成的「今日复盘 + 明日选股推荐」。

export interface DailyReview {
  summary: string
  market_review: string
  events_review?: string // N2 今日重要事件解读（事件由硬规则筛出、LLM 只写摘要；旧日报无此字段）
  position_review: string
  watch_review: string
  risk_warnings: string[]
  tomorrow_plan: string
  evidence_check?: EvidenceCheck // 复盘文本引用数字与快照的核验（服务端回填）
}

export interface DailyReportRow {
  id: number
  user_id: number
  trade_date: string
  market: string
  status: 'processing' | 'success' | 'partial' | 'failed'
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

// 手动生成/重生成当日日报（计 1 次配额）。2026-07-14 异步任务化：接口立即返回
// processing 记录，复盘+推荐在服务端后台并行执行——轮询 getDailyReport 直到脱离
// processing（lib/poll.ts 的 pollUntil），不再需要超长前端超时。
export function generateDailyReport() {
  return request<DailyReportView>({ url: '/daily-reports/generate', method: 'post' })
}
