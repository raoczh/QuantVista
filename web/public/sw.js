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

self.addEventListener('push', (event) => {
  let payload = {}
  try {
    payload = event.data ? event.data.json() : {}
  } catch {
    payload = { title: 'QuantVista 通知', body: event.data ? event.data.text() : '' }
  }
  const route = safeRoute(payload.route)
  const title = String(payload.title || 'QuantVista 通知')
  const options = {
    body: String(payload.body || ''),
    tag: payload.event_id ? `qv-event-${payload.event_id}` : undefined,
    data: { route },
  }
  event.waitUntil(Promise.all([
    self.registration.showNotification(title, options),
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        client.postMessage({ type: 'qv-browser-notification', payload: { ...payload, route } })
      }
    }),
  ]))
})

self.addEventListener('notificationclick', (event) => {
  event.notification.close()
  const route = safeRoute(event.notification.data?.route)
  event.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(async (clients) => {
    for (const client of clients) {
      if ('focus' in client) {
        await client.focus()
        client.postMessage({ type: 'qv-browser-notification', payload: { route, title: event.notification.title, focus: true } })
        return
      }
    }
    if (self.clients.openWindow) await self.clients.openWindow(route)
  }))
})
