import { request } from './client'

export interface ThesisCard {
  id: number
  user_id: number
  symbol: string
  market: string
  name: string
  thesis: string
  key_evidence: string
  risks: string
  kill_switches: string
  track_metrics: string
  next_review_date: string
  status: 'active' | 'invalidated' | 'archived'
  invalid_reason: string
  created_at: string
  updated_at: string
}

export interface ThesisUpsertRequest {
  symbol: string
  market: string
  thesis: string
  key_evidence?: string
  risks?: string
  kill_switches?: string
  track_metrics?: string
  next_review_date?: string
}

export interface ThesisCheckItem {
  card: ThesisCard
  quote_ok: boolean
  price: number
  change_pct: number
  change_pct_20d: number
  review_due: boolean
  signals: string[]
}

export function listThesisCards(status = '') {
  return request<ThesisCard[]>({ url: '/thesis-cards', method: 'get', params: status ? { status } : {} })
}

export function getThesisBySymbol(symbol: string, market: string) {
  return request<ThesisCard | null>({ url: '/thesis-cards', method: 'get', params: { symbol, market } })
}

export function upsertThesisCard(req: ThesisUpsertRequest) {
  return request<ThesisCard>({ url: '/thesis-cards', method: 'post', data: req })
}

export function setThesisStatus(id: number, status: string, reason = '') {
  return request<ThesisCard>({ url: `/thesis-cards/${id}/status`, method: 'put', data: { status, reason } })
}

export function deleteThesisCard(id: number) {
  return request({ url: `/thesis-cards/${id}`, method: 'delete' })
}

export function checkupThesisCards() {
  return request<ThesisCheckItem[]>({ url: '/thesis-cards/checkup', method: 'get' })
}
