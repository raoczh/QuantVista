import { request } from './client'

export type TodoKind = 'alert' | 'rec_review' | 'position_short' | 'position_long' | 'thesis_due'

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
}

export function getTodos() {
  return request<TodoResult>({ url: '/todos', method: 'get' })
}
