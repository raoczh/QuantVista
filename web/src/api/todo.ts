import { request } from './client'

export type TodoKind =
  | 'alert'
  | 'rec_review'
  | 'position_short'
  | 'position_long'
  | 'thesis_due'
  | 'stop_loss'
  | 'corp_adjust' // B8 除权除息待确认折算（不确认则持仓页盈亏是错的）
  | 'ipo' // B9 今日可申购新股/可转债（不依赖持仓）

export interface TodoItem {
  kind: TodoKind
  priority: number
  symbol: string
  market: string
  name: string
  title: string
  detail: string
  ref_id: number
  ref_type: string // alerts / recommendations / positions / thesis / ipo
  time: string | null
}

export interface TodoResult {
  date: string
  total: number
  alerts: number
  reviews: number
  items: TodoItem[]
  complete: boolean // 全部数据块读取成功才为 true；false 时清单可能不完整
  errors?: string[] // 读取失败/状态不明的数据块说明
}

export function getTodos() {
  return request<TodoResult>({ url: '/todos', method: 'get' })
}
