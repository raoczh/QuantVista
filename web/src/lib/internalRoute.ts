export function safeInternalRoute(raw: unknown, fallback = '/') {
  if (typeof raw !== 'string' || !raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) return fallback
  try {
    const url = new URL(raw, location.origin)
    if (url.origin !== location.origin) return fallback
    return `${url.pathname}${url.search}${url.hash}`
  } catch {
    return fallback
  }
}
