const FALLBACK_ROUTE = '/'

function safeRoute(raw) {
  if (typeof raw !== 'string' || !raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) return FALLBACK_ROUTE
  try {
    const url = new URL(raw, self.location.origin)
    if (url.origin !== self.location.origin) return FALLBACK_ROUTE
    return `${url.pathname}${url.search}${url.hash}`
  } catch {
    return FALLBACK_ROUTE
  }
}

self.addEventListener('install', event => event.waitUntil(self.skipWaiting()))
self.addEventListener('activate', event => event.waitUntil(self.clients.claim()))

// 只保存已展示的事件编号。Push、在线通知、多页签与 Worker 重启共用去重依据，
// 正文及用户令牌不写入浏览器数据库。
async function receipt(key, save = false) {
  const db = await new Promise((resolve, reject) => {
    const request = indexedDB.open('quantvista-notification-receipts', 1)
    request.onupgradeneeded = () => request.result.createObjectStore('shown')
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
  })
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction('shown', save ? 'readwrite' : 'readonly')
      const store = tx.objectStore('shown')
      let found = false
      if (save) {
        store.put(Date.now(), key)
        const cursor = store.openCursor()
        cursor.onsuccess = () => {
          if (!cursor.result) return
          if (cursor.result.value < Date.now() - 86_400_000) cursor.result.delete()
          cursor.result.continue()
        }
      } else {
        const request = store.get(key)
        request.onsuccess = () => { found = !!request.result }
      }
      tx.oncomplete = () => resolve(found)
      tx.onerror = () => reject(tx.error)
      tx.onabort = () => reject(tx.error)
    })
  } finally { db.close() }
}

let displayQueue = Promise.resolve()
function display(payload) {
  const task = displayQueue.catch(() => {}).then(async () => {
    const route = safeRoute(payload.route)
    const key = payload.event_id && payload.user_id ? `${payload.user_id}:${payload.event_id}` : ''
    const tag = key ? `qv-event-${payload.user_id}-${payload.event_id}` : undefined
    let alreadyShown = false
    if (key) {
      alreadyShown = await receipt(key).catch(() => false)
      if (!alreadyShown) alreadyShown = (await self.registration.getNotifications({ tag })).length > 0
    }
    if (!alreadyShown) {
      await self.registration.showNotification(String(payload.title || 'QuantVista 通知'), {
        body: String(payload.body || ''), tag,
        requireInteraction: payload.level === 'urgent',
        data: { route, user_id: payload.user_id, event_id: payload.event_id },
      })
      if (key) await receipt(key, true).catch(() => {})
    }
    return { ...payload, route, duplicate: alreadyShown }
  })
  displayQueue = task
  return task
}

self.addEventListener('message', event => {
  if (event.data?.type !== 'qv-show-notification') return
  event.waitUntil(display(event.data.payload || {}).then(
    () => event.ports[0]?.postMessage({ ok: true }),
    () => event.ports[0]?.postMessage({ ok: false }),
  ))
})

self.addEventListener('push', (event) => {
  let payload = {}
  try {
    payload = event.data ? event.data.json() : {}
  } catch {
    payload = { title: 'QuantVista 通知', body: event.data ? event.data.text() : '' }
  }
  event.waitUntil(display(payload).then(async shown => {
    const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
    for (const client of clients) client.postMessage({ type: 'qv-browser-notification', payload: shown })
  }))
})

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const route = safeRoute(event.notification.data?.route)
  event.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(async (clients) => {
    for (const client of clients) {
      if ('focus' in client) {
        await client.focus()
        client.postMessage({ type: 'qv-browser-notification', payload: { route, user_id: event.notification.data?.user_id, title: event.notification.title, focus: true } })
        return
      }
    }
    if (self.clients.openWindow) await self.clients.openWindow(route)
  }))
})
