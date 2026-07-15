import { request } from './client'

export type NotifyKind = 'serverchan' | 'webhook' | 'ntfy'

export interface NotifyChannel {
  id: number
  kind: NotifyKind
  name: string
  enabled: boolean
  has_target: boolean
  last_sent_at: string | null
  last_error: string
  created_at: string
}

export interface NotifyChannelInput {
  kind: NotifyKind
  name?: string
  target?: string
  enabled?: boolean
}

export function listChannels() {
  return request<NotifyChannel[]>({ url: '/notify-channels', method: 'get' })
}

export function createChannel(input: NotifyChannelInput) {
  return request<NotifyChannel>({ url: '/notify-channels', method: 'post', data: input })
}

export function updateChannel(id: number, input: NotifyChannelInput) {
  return request<NotifyChannel>({ url: `/notify-channels/${id}`, method: 'put', data: input })
}

export function deleteChannel(id: number) {
  return request<{ ok: boolean }>({ url: `/notify-channels/${id}`, method: 'delete' })
}

export function testChannel(id: number) {
  return request<{ ok: boolean }>({ url: `/notify-channels/${id}/test`, method: 'post' })
}
