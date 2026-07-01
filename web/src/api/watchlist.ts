import { request } from './client'

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
  // 富化字段
  price: number
  change_pct: number
  quote_ok: boolean
  data_time: string
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
  return request<WatchlistItem>({
    url: `/watchlists/${groupId}/items`,
    method: 'post',
    data: input,
  })
}

export function updateItem(itemId: number, input: WatchlistItemInput) {
  return request<WatchlistItem>({
    url: `/watchlist-items/${itemId}`,
    method: 'put',
    data: input,
  })
}

export function deleteItem(itemId: number) {
  return request<{ ok: boolean }>({ url: `/watchlist-items/${itemId}`, method: 'delete' })
}
