import { request } from './client'

export type TodoKind =
  | 'alert'
  | 'rec_review'
  | 'position_short'
  | 'position_long'
  | 'thesis_due'
  | 'stop_loss'
  | 'corp_adjust'
  | 'ipo'
  | 'sell_review'
  | 'job_failure'

export type TodoScope = 'ledger' | 'research' | 'market' | 'all'
export type TodoStatus = 'open' | 'needs_action' | 'awareness' | 'completed' | 'all'
export type TodoInboxAction = 'read' | 'snooze' | 'mute_today'

export interface TodoSourceRef {
  source_kind: string
  source_id: number
  source_version: string
}

export interface TodoChild extends TodoSourceRef {
  kind: TodoKind
  source_label: string
  title: string
  detail: string
  severity: string
  ref_id: number
  ref_type: string
  deep_link?: string
  time: string | null
  due_at?: string | null
  read: boolean
  snoozed_until?: string | null
  can_complete: boolean
}

export interface TodoItem extends TodoSourceRef {
  kind: TodoKind
  scope: TodoScope
  status: Exclude<TodoStatus, 'open' | 'all'>
  priority: number
  symbol: string
  market: string
  name: string
  title: string
  detail: string
  ref_id: number
  ref_type: string
  deep_link?: string
  time: string | null
  due_at?: string | null
  source_label: string
  severity: string
  read: boolean
  snoozed_until?: string | null
  can_complete: boolean
  group_key: string
  child_count: number
  children: TodoChild[]
}

export interface TodoResult {
  date: string
  scope: TodoScope
  status: TodoStatus
  source: string
  total: number
  matched_total: number
  alerts: number
  reviews: number
  items: TodoItem[]
  complete: boolean
  partial: boolean
  errors?: string[]
  scope_counts: Record<string, number>
  status_counts: Record<string, number>
  source_counts: Record<string, number>
  filtered: number
  page: number
  page_size: number
  has_more: boolean
}

export interface TodoQuery {
  scope?: TodoScope
  status?: TodoStatus
  source?: string
  page?: number
  page_size?: number
  history_days?: number
  limit?: number
}

export function getTodoInbox(params: TodoQuery = {}, signal?: AbortSignal) {
  return request<TodoResult>({ url: '/todos', method: 'get', params, signal })
}

// 兼容既有消费方；新页面应使用 getTodoInbox 显式声明投影筛选。
export function getTodos(scope?: TodoScope, signal?: AbortSignal) {
  return getTodoInbox({ scope }, signal)
}

export function updateTodoInbox(action: TodoInboxAction, items: TodoSourceRef[]) {
  return request<{ ok: boolean }>({
    url: '/todos/actions',
    method: 'put',
    data: { action, items },
  })
}
