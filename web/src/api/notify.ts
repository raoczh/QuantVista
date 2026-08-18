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

export interface BrowserNotificationSettings {
  exit_risk: boolean
  manual_alert: boolean
  guard: boolean
}

export interface BrowserNotificationDevice {
  id: number
  name: string
  enabled: boolean
  has_web_push: boolean
  last_seen_at: string | null
  last_success_at: string | null
  last_failure_at: string | null
  last_error_code: string
  created_at: string
}

export interface BrowserNotificationConfig {
  vapid_configured: boolean
  vapid_public_key: string
  settings: BrowserNotificationSettings
  devices: BrowserNotificationDevice[]
}

export interface BrowserNotificationEvent {
  delivery_id: number
  event: {
    id: number
    source_type: string
    source_id: number
    fact_key: string
    category: string
    level: string
    title: string
    body: string
    route: string
    created_at: string
  }
}

export interface BrowserSubscriptionInput {
  device_key: string
  name: string
  endpoint?: string
  p256dh?: string
  auth?: string
}

export function getBrowserNotificationConfig() {
  return request<BrowserNotificationConfig>({ url: '/browser-notifications/config', method: 'get' })
}

export function updateBrowserNotificationSettings(settings: BrowserNotificationSettings) {
  return request<BrowserNotificationSettings>({ url: '/browser-notifications/settings', method: 'put', data: settings })
}

export function upsertBrowserSubscription(input: BrowserSubscriptionInput) {
  return request<BrowserNotificationDevice>({ url: '/browser-notifications/subscriptions', method: 'post', data: input })
}

export function removeBrowserDevice(id: number) {
  return request<{ ok: boolean }>({ url: `/browser-notifications/subscriptions/${id}`, method: 'delete' })
}

export function listBrowserNotificationEvents(deviceKey: string, afterID = 0) {
  return request<BrowserNotificationEvent[]>({
    url: '/browser-notifications/events',
    method: 'get',
    params: { device_key: deviceKey, after_id: afterID, limit: 20 },
  })
}

export function ackBrowserNotification(deliveryID: number, deviceKey: string) {
  return request<{ ok: boolean }>({
    url: `/browser-notifications/events/${deliveryID}/ack`,
    method: 'put',
    data: { device_key: deviceKey },
  })
}

export function testBrowserNotification(deviceKey: string) {
  return request<{ ok: boolean }>({ url: '/browser-notifications/test', method: 'post', data: { device_key: deviceKey } })
}
