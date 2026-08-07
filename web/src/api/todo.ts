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
  | 'sell_review' // D16 持仓命中利空事件，需卖出复核

/**
 * D18 待办范围：今日待办**默认只显示与我的账本有关的条目**。
 * - ledger：持仓相关（止损/复盘/除权折算/卖出复核/持仓标的的提醒与逻辑卡）
 * - research：推荐复盘 + 非持仓标的的提醒与逻辑卡（推荐追踪页自己的区域取这一份）
 * - market：全市场机会（打新）
 * - all：全部
 * 数据一条不删，范围只是消费出口的过滤器。
 */
export type TodoScope = 'ledger' | 'research' | 'market' | 'all'

export interface TodoItem {
  kind: TodoKind
  scope: TodoScope
  priority: number
  symbol: string
  market: string
  name: string
  title: string
  detail: string
  ref_id: number
  ref_type: string // alerts / recommendations / positions / thesis / ipo
  deep_link?: string
  time: string | null
}

export interface TodoResult {
  date: string
  scope: TodoScope
  total: number
  alerts: number
  reviews: number
  items: TodoItem[]
  complete: boolean // 全部数据块读取成功才为 true；false 时清单可能不完整
  errors?: string[] // 读取失败/状态不明的数据块说明
  // 各范围的全量条数（不受本次过滤影响），供提示「另有 N 条在别处」
  scope_counts: Record<string, number>
  filtered: number // 因范围过滤未展示的条数
}

export function getTodos(scope?: TodoScope, signal?: AbortSignal) {
  return request<TodoResult>({ url: '/todos', method: 'get', params: { scope }, signal })
}
