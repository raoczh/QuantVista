import { computed, ref } from 'vue'
import type { Router } from 'vue-router'
import type { MessageApiInjection } from 'naive-ui/es/message/src/MessageProvider'
import {
  ackBrowserNotification,
  listBrowserNotificationEvents,
  type BrowserNotificationEvent,
} from '@/api/notify'

const DEVICE_KEY_PREFIX = 'qv-browser-device-key:'
const DEVICE_ID_PREFIX = 'qv-browser-device-id:'

export function browserNotificationSupported() {
  return typeof window !== 'undefined' && 'Notification' in window
}

export function webPushSupported() {
  return browserNotificationSupported() && 'serviceWorker' in navigator && 'PushManager' in window
}

export function browserPermission(): NotificationPermission | 'unsupported' {
  return browserNotificationSupported() ? Notification.permission : 'unsupported'
}

export function browserDeviceKey(userID: number) {
  const storageKey = DEVICE_KEY_PREFIX + userID
  let value = localStorage.getItem(storageKey)
  if (!value) {
    value = typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID()
      : `${Date.now()}-${crypto.getRandomValues(new Uint32Array(4)).join('-')}`
    localStorage.setItem(storageKey, value)
  }
  return value
}

export function currentBrowserDeviceID(userID: number): number | null {
  const value = Number(localStorage.getItem(DEVICE_ID_PREFIX + userID))
  return Number.isSafeInteger(value) && value > 0 ? value : null
}

export function rememberBrowserDeviceID(userID: number, id: number | null) {
  const key = DEVICE_ID_PREFIX + userID
  if (id && id > 0) localStorage.setItem(key, String(id))
  else localStorage.removeItem(key)
}

export async function ensureNotificationServiceWorker() {
  if (!('serviceWorker' in navigator)) throw new Error('当前浏览器不支持 Service Worker')
  return navigator.serviceWorker.register('/sw.js', { scope: '/' })
}

export function urlBase64ToUint8Array(value: string) {
  const padding = '='.repeat((4 - (value.length % 4)) % 4)
  const raw = atob((value + padding).replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(raw, (char) => char.charCodeAt(0))
}

export function pushSubscriptionInput(subscription: PushSubscription) {
  const json = subscription.toJSON()
  return {
    endpoint: subscription.endpoint,
    p256dh: json.keys?.p256dh || '',
    auth: json.keys?.auth || '',
  }
}

function safeInternalRoute(raw: string) {
  if (!raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) return '/'
  try {
    const url = new URL(raw, location.origin)
    return url.origin === location.origin ? `${url.pathname}${url.search}${url.hash}` : '/'
  } catch {
    return '/'
  }
}

function showForegroundNotification(item: BrowserNotificationEvent, router: Router) {
  if (!browserNotificationSupported() || Notification.permission !== 'granted') return
  const route = safeInternalRoute(item.event.route)
  const notification = new Notification(item.event.title, {
    body: item.event.body,
    tag: `qv-event-${item.event.id}`,
  })
  notification.onclick = () => {
    window.focus()
    void router.push(route)
    notification.close()
  }
}

export function useBrowserNotificationRuntime(userID: () => number, router: Router, message: MessageApiInjection) {
  const running = ref(false)
  const lastEventID = ref(0)
  let activeUserID = 0
  let timer: number | undefined

  const enabled = computed(() => userID() > 0 && browserNotificationSupported() && Notification.permission === 'granted')

  async function handleEvent(item: BrowserNotificationEvent, fromServiceWorker = false) {
    if (!fromServiceWorker) showForegroundNotification(item, router)
    message.info(item.event.title, { duration: 4500, closable: true })
    lastEventID.value = Math.max(lastEventID.value, item.event.id)
    const key = browserDeviceKey(userID())
    await ackBrowserNotification(item.delivery_id, key).catch(() => undefined)
  }

  async function poll() {
    if (!enabled.value || running.value) return
    if (activeUserID !== userID()) {
      activeUserID = userID()
      lastEventID.value = 0
    }
    running.value = true
    try {
      const key = browserDeviceKey(userID())
      const rows = await listBrowserNotificationEvents(key, lastEventID.value)
      for (const row of rows) await handleEvent(row)
    } catch {
      // 轮询是旁路，设置页会提供明确恢复状态；外壳不持续打扰用户。
    } finally {
      running.value = false
    }
  }

  function onWorkerMessage(event: MessageEvent) {
    if (event.data?.type !== 'qv-browser-notification') return
    const payload = event.data.payload || {}
    const route = safeInternalRoute(String(payload.route || '/'))
    message.info(String(payload.title || 'QuantVista 通知'), { duration: 4500, closable: true })
    if (payload.focus === true) void router.push(route)
  }

  function start() {
    if (timer !== undefined) return
    if ('serviceWorker' in navigator) navigator.serviceWorker.addEventListener('message', onWorkerMessage)
    void poll()
    timer = window.setInterval(poll, 20_000)
    document.addEventListener('visibilitychange', poll)
  }

  function stop() {
    if (timer !== undefined) window.clearInterval(timer)
    timer = undefined
    document.removeEventListener('visibilitychange', poll)
    if ('serviceWorker' in navigator) navigator.serviceWorker.removeEventListener('message', onWorkerMessage)
  }

  return { start, stop, poll, enabled }
}
