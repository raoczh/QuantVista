import { request } from './client'

export type TodoKind = 'alert' | 'rec_review' | 'position_short' | 'position_long' | 'thesis_due' | 'stop_loss'

export interface TodoItem {
  kind: TodoKind
  priority: number
  symbol: string
  market: string
  name: string
  title: string
  detail: string
  ref_id: number
  ref_type: string // alerts / recommendations / positions
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
