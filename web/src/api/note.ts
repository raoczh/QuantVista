import { request } from './client'

export type NoteKind = '' | 'decision' | 'review' | 'idea' | 'event'

export interface ResearchNote {
  id: number
  user_id: number
  symbol: string
  market: string
  name: string
  kind: NoteKind
  title: string
  content: string
  created_at: string
  updated_at: string
}

export interface NoteInput {
  symbol?: string
  market?: string
  kind?: NoteKind
  title: string
  content: string
}

export function listNotes(params: { symbol?: string; market?: string; keyword?: string; limit?: number } = {}) {
  return request<ResearchNote[]>({ url: '/notes', method: 'get', params })
}

export function createNote(data: NoteInput) {
  return request<ResearchNote>({ url: '/notes', method: 'post', data })
}

export function updateNote(id: number, data: NoteInput) {
  return request<ResearchNote>({ url: `/notes/${id}`, method: 'put', data })
}

export function deleteNote(id: number) {
  return request({ url: `/notes/${id}`, method: 'delete' })
}
