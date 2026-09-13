import { computed, ref } from 'vue'
import type { Router } from 'vue-router'
import type { MessageApiInjection } from 'naive-ui/es/message/src/MessageProvider'
import { getSessionEpoch } from '@/api/token'
import {
  ackBrowserNotification,
  listBrowserNotificationEvents,
  type BrowserNotificationEvent,
} from '@/api/notify'

const DEVICE_KEY_PREFIX = 'qv-browser-device-key:'
const DEVICE_ID_PREFIX = 'qv-browser-device-id:'
export const BROWSER_NOTIFICATION_CHANGED = 'qv-browser-notifications-changed'

export function refreshBrowserNotificationRuntime() {
  window.dispatchEvent(new Event(BROWSER_NOTIFICATION_CHANGED))
}

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
  const registration = await navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' })
  if (registration.active?.state === 'activated') return registration
  const worker = registration.installing || registration.waiting || registration.active
  if (!worker) throw new Error('通知服务尚未安装，请重试')
  await new Promise<void>((resolve, reject) => {
    const done = () => {
      if (worker.state !== 'activated' && worker.state !== 'redundant') return
      clearTimeout(timer)
      worker.removeEventListener('statechange', done)
      if (worker.state === 'activated') resolve()
      else reject(new Error('通知服务安装失败，请重试'))
    }
    const timer = setTimeout(() => {
      worker.removeEventListener('statechange', done)
      reject(new Error('通知服务激活超时，请重试'))
    }, 12_000)
    worker.addEventListener('statechange', done)
    done()
  })
  return registration
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

async function showSystemNotification(item: BrowserNotificationEvent, router: Router, current: () => boolean) {
  if (!current() || !browserNotificationSupported() || Notification.permission !== 'granted') throw new Error('通知权限不可用')
  const route = safeInternalRoute(item.event.route)
  // 与 Web Push 共用 Worker 的展示与去重入口，移动端也不依赖不支持的构造器。
  if ('serviceWorker' in navigator && typeof navigator.serviceWorker.getRegistration === 'function') {
    const registration = await navigator.serviceWorker.getRegistration('/')
    if (!current()) throw new Error('会话已变更')
    if (registration?.active) {
      const channel = new MessageChannel()
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => { channel.port1.close(); reject(new Error('系统通知展示超时')) }, 8_000)
        channel.port1.onmessage = ({ data }) => {
          clearTimeout(timer)
          channel.port1.close()
          if (data?.ok) resolve()
          else reject(new Error('系统通知未能展示'))
        }
        registration.active!.postMessage({ type: 'qv-show-notification', payload: {
          ...item.event, event_id: item.event.id, delivery_id: item.delivery_id, route,
        } }, [channel.port2])
      })
      return
    }
  }
  const notification = new Notification(item.event.title, {
    body: item.event.body,
    tag: `qv-event-${item.event.user_id}-${item.event.id}`,
    requireInteraction: item.event.level === 'urgent',
  })
  notification.onclick = () => {
    if (current()) {
      window.focus()
      void router.push(route)
    }
    notification.close()
  }
}

export function useBrowserNotificationRuntime(userID: () => number, router: Router, message: MessageApiInjection) {
  const running = ref(false)
  const lastEventID = ref(0)
  let activeUserID = 0
  let timer: number | undefined
  let started = false
  let startedSession = 0
  let generation = 0
  let pollController: AbortController | null = null
  let nextPoll: number | undefined
  const permission = ref(browserPermission())
  const shown = new Set<string>()

  const enabled = computed(() => userID() > 0 && permission.value === 'granted')

  async function handleEvent(item: BrowserNotificationEvent, key: string, owner: number, current: () => boolean) {
    if (!current() || item.event.user_id !== owner) return false
    const show = async () => {
      if (!current()) return
      const eventKey = `${owner}:${item.event.id}`
      const storageKey = `qv-browser-shown:${eventKey}`
      if (!shown.has(eventKey) && localStorage.getItem(storageKey) === null) {
        await showSystemNotification(item, router, current)
        if (!current()) return
        shown.add(eventKey)
        localStorage.setItem(storageKey, String(Date.now()))
        if (document.visibilityState !== 'hidden') message.info(item.event.title, { duration: 4500, closable: true })
      }
    }
    if (navigator.locks) await navigator.locks.request(`qv-browser-show:${owner}`, show)
    else await show()
    if (!current()) return false
    try {
      await ackBrowserNotification(item.delivery_id, key)
      if (!current()) return false
      // 只有服务端确认了当前设备的投递，才能推进游标；否则下一轮仍要
      // 重试这条事件，避免一次网络抖动把通知永久跳过。
      lastEventID.value = Math.max(lastEventID.value, item.event.id)
      return true
    } catch {
      return false
    }
  }

  async function poll() {
    permission.value = browserPermission()
    if (!started || getSessionEpoch() !== startedSession || !enabled.value || running.value) return
    const owner = userID()
    const epoch = generation
    const session = getSessionEpoch()
    const controller = new AbortController()
    pollController = controller
    const current = () => started && generation === epoch && userID() === owner && getSessionEpoch() === session && !controller.signal.aborted
    if (activeUserID !== userID()) {
      activeUserID = userID()
      lastEventID.value = 0
    }
    running.value = true
    let delay = 20_000
    try {
      const key = browserDeviceKey(owner)
      const rows = await listBrowserNotificationEvents(key, lastEventID.value, controller.signal)
      if (!current()) return
      for (const row of rows) {
        const acknowledged = await handleEvent(row, key, owner, current)
        if (!acknowledged) break
      }
      delay = rows.length > 0 ? 250 : 1_000
    } catch {
      // 轮询是旁路，设置页会提供明确恢复状态；外壳不持续打扰用户。
    } finally {
      if (generation === epoch && pollController === controller) {
        pollController = null
        running.value = false
        if (started) nextPoll = window.setTimeout(() => void poll(), delay)
      }
    }
  }

  function onWorkerMessage(event: MessageEvent) {
    if (!started || getSessionEpoch() !== startedSession || !enabled.value || event.data?.type !== 'qv-browser-notification') return
    const payload = event.data.payload || {}
    if (payload.user_id !== userID()) return
    const route = safeInternalRoute(String(payload.route || '/'))
    if (payload.delivery_id && payload.focus !== true) {
      void ackBrowserNotification(payload.delivery_id, browserDeviceKey(userID())).catch(() => {})
    }
    if (document.visibilityState !== 'hidden' && !payload.duplicate && payload.focus !== true) message.info(String(payload.title || 'QuantVista 通知'), { duration: 4500, closable: true })
    if (payload.focus === true) void router.push(route)
  }

  function start() {
    if (started) return
    started = true
    startedSession = getSessionEpoch()
    generation++
    // 仅保存事件编号，24 小时后清理；不保存标题、正文或账户凭据。
    for (let i = localStorage.length - 1; i >= 0; i--) {
      const key = localStorage.key(i)
      if (key?.startsWith('qv-browser-shown:') && Number(localStorage.getItem(key)) < Date.now() - 86_400_000) localStorage.removeItem(key)
    }
    if ('serviceWorker' in navigator) navigator.serviceWorker.addEventListener('message', onWorkerMessage)
    void poll()
    timer = window.setInterval(poll, 20_000)
    document.addEventListener('visibilitychange', poll)
    window.addEventListener('focus', poll)
    window.addEventListener('online', poll)
    window.addEventListener(BROWSER_NOTIFICATION_CHANGED, poll)
  }

  function stop() {
    started = false
    generation++
    pollController?.abort()
    pollController = null
    running.value = false
    if (timer !== undefined) window.clearInterval(timer)
    timer = undefined
    if (nextPoll !== undefined) window.clearTimeout(nextPoll)
    nextPoll = undefined
    document.removeEventListener('visibilitychange', poll)
    window.removeEventListener('focus', poll)
    window.removeEventListener('online', poll)
    window.removeEventListener(BROWSER_NOTIFICATION_CHANGED, poll)
    if ('serviceWorker' in navigator) navigator.serviceWorker.removeEventListener('message', onWorkerMessage)
  }

  return { start, stop, poll, enabled }
}
