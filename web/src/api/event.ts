import { request } from './client'

// B9 事件日历：未来 N 天与我相关的解禁/除权/财报 + 全市场打新。

export type EventKind = 'lift' | 'ex_div' | 'earn' | 'ipo' | 'cb'
export type EventRelation = 'position' | 'watch' | 'market'

export interface CalendarEvent {
  date: string
  days_left: number
  kind: EventKind
  relation: EventRelation
  symbol: string
  market: string
  name: string
  title: string
  detail: string
  shares?: number // 解禁股数（股）
  market_cap?: number // 解禁市值（元）
  ratio?: number // 占流通股 %
  apply_code?: string // 申购代码
  route?: string
}

export interface CalendarResult {
  from: string
  to: string
  days: number
  events: CalendarEvent[]
  total: number
  // complete=false 表示至少一类读取失败，清单可能不全——
  // 前端不得据此显示「未来没事发生」，须提示状态不明。
  complete: boolean
  errors: string[]
}

export function getEventCalendar(days = 30, signal?: AbortSignal) {
  return request<CalendarResult>({ url: '/events/calendar', params: { days }, signal })
}

// ---------- 个股解禁 / 分红（StockDetail 用） ----------

export interface RestrictedRelease {
  id: number
  symbol: string
  market: string
  name: string
  free_date: string
  free_type: string
  free_shares: number // 本次解禁股数（股）
  lift_market_cap: number // 解禁市值（元）
  free_ratio: number // 占解禁前流通股 %
  total_ratio: number // 占总股本 %
}

export interface CorporateAction {
  id: number
  symbol: string
  market: string
  name: string
  report_date: string
  ex_date: string
  record_date: string
  notice_date: string
  // 每 10 股口径（bonus_ratio=2 表示每 10 股送 2 股），勿预先除以 10
  bonus_ratio: number
  transfer_ratio: number
  dividend_pretax: number
  dividend_yield: number // 股息率 %
  progress: string
  plan_profile: string
}

export interface StockCorpEvents {
  symbol: string
  market: string
  lifts: RestrictedRelease[]
  actions: CorporateAction[]
  // **unavailable=true 与空数组语义不同**：前者是「查不到」，后者是「确实没有」。
  // 展示时必须区分，不能把未知说成无解禁。
  lift_unavailable: boolean
  action_unavailable: boolean
  note?: string
}

export function getStockCorpEvents(market: string, symbol: string, signal?: AbortSignal) {
  return request<StockCorpEvents>({
    url: `/markets/${market}/stocks/${symbol}/corp-events`,
    signal,
  })
}
