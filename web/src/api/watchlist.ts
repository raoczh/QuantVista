import { request } from './client'

export type ResearchStage =
  | ''
  | 'discovered'
  | 'screening'
  | 'watching'
  | 'waiting_price'
  | 'planned'
  | 'bought'
  | 'passed'
  | 'reviewed'

export interface WatchlistItem {
  id: number
  user_id: number
  watchlist_id: number
  symbol: string
  market: string
  name: string
  note: string
  focus_reason: string
  is_pinned: boolean
  research_stage: ResearchStage
  passed_reason: string
  passed_price: number
  stage_at: string | null
  // 富化字段
  price: number
  change_pct: number
  quote_ok: boolean
  data_time: string
}

// 写接口（新增/编辑自选项）返回裸模型，不含列表接口才回填的行情富化字段。
export type WatchlistItemBase = Omit<
  WatchlistItem,
  'price' | 'change_pct' | 'quote_ok' | 'data_time'
>

export interface MissedOpportunity extends WatchlistItem {
  current_price: number
  change_since_pct: number
  verdict: 'missed_gain' | 'avoided_loss' | 'neutral' | 'no_base'
}

export interface WatchlistGroup {
  id: number
  user_id: number
  name: string
  sort_order: number
  items: WatchlistItem[]
}

export interface WatchlistItemInput {
  symbol?: string
  market?: string
  name?: string
  note?: string
  focus_reason?: string
  is_pinned?: boolean
  watchlist_id?: number
}

export function listWatchlists() {
  return request<WatchlistGroup[]>({ url: '/watchlists' })
}

export function createGroup(name: string) {
  return request<WatchlistGroup>({ url: '/watchlists', method: 'post', data: { name } })
}

export function updateGroup(id: number, name: string, sortOrder = 0) {
  return request<WatchlistGroup>({
    url: `/watchlists/${id}`,
    method: 'put',
    data: { name, sort_order: sortOrder },
  })
}

export function deleteGroup(id: number) {
  return request<{ ok: boolean }>({ url: `/watchlists/${id}`, method: 'delete' })
}

export function addItem(groupId: number, input: WatchlistItemInput) {
  return request<WatchlistItemBase>({
    url: `/watchlists/${groupId}/items`,
    method: 'post',
    data: input,
  })
}

export function updateItem(itemId: number, input: WatchlistItemInput) {
  return request<WatchlistItemBase>({
    url: `/watchlist-items/${itemId}`,
    method: 'put',
    data: input,
  })
}

export function deleteItem(itemId: number) {
  return request<{ ok: boolean }>({ url: `/watchlist-items/${itemId}`, method: 'delete' })
}

export function setItemStage(itemId: number, stage: ResearchStage, reason = '') {
  return request<WatchlistItem>({
    url: `/watchlist-items/${itemId}/stage`,
    method: 'put',
    data: { stage, reason },
  })
}

export function listMissed() {
  return request<MissedOpportunity[]>({ url: '/watchlist-items/missed' })
}
