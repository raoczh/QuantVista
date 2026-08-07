import { nextTick, onBeforeUnmount, onMounted, type Ref, watch } from 'vue'
import {
  onBeforeRouteLeave,
  onBeforeRouteUpdate,
  type LocationQueryValue,
  type RouteLocationNormalizedLoaded,
  type Router,
} from 'vue-router'
import { useAuthStore } from '@/stores/auth'

type RawQueryValue = LocationQueryValue | LocationQueryValue[]

export interface QueryCodec<T> {
  parse: (raw: RawQueryValue | undefined) => T
  format: (value: T) => string | undefined
}

interface RouteQueryBinding {
  key: string
  read: () => unknown
  write: (value: unknown) => void
  parse: (raw: RawQueryValue | undefined) => unknown
  format: (value: unknown) => string | undefined
}

export function queryRef<T>(key: string, state: Ref<T>, codec: QueryCodec<T>): RouteQueryBinding {
  return {
    key,
    read: () => state.value,
    write: (value) => {
      state.value = value as T
    },
    parse: codec.parse,
    format: (value) => codec.format(value as T),
  }
}

export function enumQuery<T extends string>(fallback: T, values: readonly T[]): QueryCodec<T> {
  const allowed = new Set<string>(values)
  return {
    parse: (raw) => {
      const value = queryString(raw)
      return allowed.has(value) ? (value as T) : fallback
    },
    format: (value) => (value === fallback ? undefined : value),
  }
}

export function enumListQuery<T extends string>(values: readonly T[], maxItems = 8): QueryCodec<T[]> {
  const allowed = new Set<string>(values)
  const normalize = (items: string[]) =>
    [...new Set(items.filter((item): item is T => allowed.has(item)))].slice(0, maxItems)
  return {
    parse: (raw) => {
      const value = queryString(raw)
      return value ? normalize(value.split(',')) : []
    },
    format: (value) => {
      const normalized = normalize(value)
      return normalized.length ? normalized.join(',') : undefined
    },
  }
}

export function integerQuery(fallback: number, min: number, max: number): QueryCodec<number> {
  return {
    parse: (raw) => {
      const value = Number(queryString(raw))
      return Number.isInteger(value) && value >= min && value <= max ? value : fallback
    },
    format: (value) =>
      Number.isInteger(value) && value >= min && value <= max && value !== fallback ? String(value) : undefined,
  }
}

export function integerEnumQuery(fallback: number, values: readonly number[]): QueryCodec<number> {
  const allowed = new Set(values)
  return {
    parse: (raw) => {
      const value = Number(queryString(raw))
      return Number.isInteger(value) && allowed.has(value) ? value : fallback
    },
    format: (value) => (Number.isInteger(value) && allowed.has(value) && value !== fallback ? String(value) : undefined),
  }
}

export function numberQuery(fallback: number, min: number, max: number): QueryCodec<number> {
  return {
    parse: (raw) => {
      const value = Number(queryString(raw))
      return Number.isFinite(value) && value >= min && value <= max ? value : fallback
    },
    format: (value) =>
      Number.isFinite(value) && value >= min && value <= max && value !== fallback ? String(value) : undefined,
  }
}

export function booleanQuery(fallback = false): QueryCodec<boolean> {
  return {
    parse: (raw) => {
      const value = queryString(raw)
      if (value === '1') return true
      if (value === '0') return false
      return fallback
    },
    format: (value) => (value === fallback ? undefined : value ? '1' : '0'),
  }
}

export function stringQuery(fallback = '', maxLength = 80): QueryCodec<string> {
  return {
    parse: (raw) => {
      const value = queryString(raw).trim()
      return value && value.length <= maxLength ? value : fallback
    },
    format: (value) => {
      const normalized = value.trim()
      return normalized && normalized.length <= maxLength && normalized !== fallback ? normalized : undefined
    },
  }
}

export function idListQuery(maxItems = 8): QueryCodec<number[]> {
  const normalize = (values: number[]) =>
    [...new Set(values.filter((value) => Number.isInteger(value) && value > 0))].slice(0, maxItems)
  return {
    parse: (raw) => {
      const value = queryString(raw)
      if (!value) return []
      return normalize(value.split(',').map(Number))
    },
    format: (value) => {
      const normalized = normalize(value)
      return normalized.length ? normalized.join(',') : undefined
    },
  }
}

function queryString(raw: RawQueryValue | undefined): string {
  const value = Array.isArray(raw) ? raw[0] : raw
  return typeof value === 'string' ? value : ''
}

function rawMatches(raw: RawQueryValue | undefined, expected: string | undefined): boolean {
  if (expected === undefined) return raw === undefined
  return typeof raw === 'string' && raw === expected
}

// 只拥有 bindings 中声明的键；其余 query（包括一次性深链）始终从当前路由合并保留。
export function useRouteQueryState(
  route: RouteLocationNormalizedLoaded,
  router: Router,
  bindings: RouteQueryBinding[],
) {
  const ownerName = route.name
  let applyingRoute = false
  let scheduled = false
  let active = true

  function applyRoute() {
    applyingRoute = true
    for (const binding of bindings) binding.write(binding.parse(route.query[binding.key]))
    applyingRoute = false
  }

  async function syncQueryNow() {
    scheduled = false
    if (!active || applyingRoute || route.name !== ownerName) return

    const query = { ...route.query }
    let changed = false
    for (const binding of bindings) {
      const expected = binding.format(binding.read())
      if (rawMatches(route.query[binding.key], expected)) continue
      changed = true
      if (expected === undefined) delete query[binding.key]
      else query[binding.key] = expected
    }
    if (!changed) return
    await router.replace({ path: route.path, query, hash: route.hash }).catch(() => undefined)
  }

  function scheduleSync() {
    if (scheduled || !active) return
    scheduled = true
    queueMicrotask(() => void syncQueryNow())
  }

  applyRoute()
  watch(
    () => bindings.map((binding) => route.query[binding.key]),
    () => {
      applyRoute()
      scheduleSync()
    },
    { flush: 'sync' },
  )
  watch(
    () => bindings.map((binding) => binding.format(binding.read())),
    () => {
      if (!applyingRoute) scheduleSync()
    },
    { flush: 'sync' },
  )
  // 初始 URL 中的数组、越界值或旧枚举值也会被安全规范化。
  scheduleSync()

  onBeforeUnmount(() => {
    active = false
  })

  return { syncQueryNow }
}

export async function replaceRouteQuery(
  route: RouteLocationNormalizedLoaded,
  router: Router,
  patch: Record<string, string | number | undefined>,
) {
  const query = { ...route.query }
  let changed = false
  for (const [key, value] of Object.entries(patch)) {
    const expected = value === undefined ? undefined : String(value)
    if (rawMatches(route.query[key], expected)) continue
    changed = true
    if (expected === undefined) delete query[key]
    else query[key] = expected
  }
  if (changed) await router.replace({ path: route.path, query, hash: route.hash }).catch(() => undefined)
}

interface ScrollEntry {
  entry: string
  path: string
  top: number
  updatedAt: number
}

interface ScrollStore {
  version: 1
  entries: ScrollEntry[]
}

const SCROLL_PREFIX = 'qv:list-scroll:v1:'
const MAX_SCROLL_ENTRIES = 8
const SCROLL_TTL_MS = 24 * 60 * 60 * 1000
const MAX_SCROLL_TOP = 20_000_000

function browserEntryID() {
  const position = window.history.state?.position
  return Number.isInteger(position) && position >= 0 ? String(position) : `history-${window.history.length}`
}

function validScrollEntry(value: unknown, now: number): value is ScrollEntry {
  if (!value || typeof value !== 'object') return false
  const entry = value as Partial<ScrollEntry>
  return (
    typeof entry.entry === 'string' &&
    entry.entry.length <= 40 &&
    typeof entry.path === 'string' &&
    entry.path.length <= 1000 &&
    typeof entry.top === 'number' &&
    Number.isFinite(entry.top) &&
    entry.top >= 0 &&
    entry.top <= MAX_SCROLL_TOP &&
    typeof entry.updatedAt === 'number' &&
    now - entry.updatedAt >= 0 &&
    now - entry.updatedAt <= SCROLL_TTL_MS
  )
}

// 滚动是瞬时状态：按用户和路由隔离，再以 history position 区分同一路由的不同访问。
export function useListPageScroll(route: RouteLocationNormalizedLoaded, routeKey: string) {
  const auth = useAuthStore()
  const ownerName = route.name
  const userID = auth.user?.id || 0
  const storageKey = `${SCROLL_PREFIX}${userID}:${routeKey}`
  let currentEntry = browserEntryID()
  let currentPath = route.fullPath
  let scrollTimer = 0

  function readStore(): ScrollStore {
    if (!userID) return { version: 1, entries: [] }
    try {
      const parsed: unknown = JSON.parse(sessionStorage.getItem(storageKey) || 'null')
      if (!parsed || typeof parsed !== 'object' || (parsed as ScrollStore).version !== 1) {
        sessionStorage.removeItem(storageKey)
        return { version: 1, entries: [] }
      }
      const now = Date.now()
      const entries = Array.isArray((parsed as ScrollStore).entries)
        ? (parsed as ScrollStore).entries.filter((entry) => validScrollEntry(entry, now)).slice(0, MAX_SCROLL_ENTRIES)
        : []
      return { version: 1, entries }
    } catch {
      sessionStorage.removeItem(storageKey)
      return { version: 1, entries: [] }
    }
  }

  function writeStore(store: ScrollStore) {
    if (!userID) return
    try {
      sessionStorage.setItem(storageKey, JSON.stringify(store))
    } catch {
      // sessionStorage 被禁用或已满时不影响页面导航。
    }
  }

  function saveScroll() {
    if (!userID || route.name !== ownerName) return
    const now = Date.now()
    const top = Math.min(Math.max(window.scrollY || 0, 0), MAX_SCROLL_TOP)
    const store = readStore()
    const entries = store.entries.filter(
      (entry) => !(entry.entry === currentEntry && entry.path === currentPath),
    )
    entries.unshift({ entry: currentEntry, path: currentPath, top, updatedAt: now })
    writeStore({ version: 1, entries: entries.slice(0, MAX_SCROLL_ENTRIES) })
  }

  function onScroll() {
    if (scrollTimer) return
    scrollTimer = window.setTimeout(() => {
      scrollTimer = 0
      saveScroll()
    }, 150)
  }

  async function restoreScroll() {
    if (!userID || route.name !== ownerName) return false
    const matched = readStore().entries.find(
      (entry) => entry.entry === currentEntry && entry.path === currentPath,
    )
    if (!matched) return false
    await nextTick()
    await new Promise<void>((resolve) =>
      window.requestAnimationFrame(() => window.requestAnimationFrame(() => resolve())),
    )
    if (route.name !== ownerName || route.fullPath !== currentPath) return false
    window.scrollTo({ top: matched.top, behavior: 'auto' })
    return true
  }

  onMounted(() => {
    window.addEventListener('scroll', onScroll, { passive: true })
  })
  onBeforeRouteLeave(() => {
    saveScroll()
  })
  onBeforeRouteUpdate(() => {
    saveScroll()
  })
  watch(
    () => route.fullPath,
    (path) => {
      if (route.name !== ownerName) return
      const nextEntry = browserEntryID()
      const entryChanged = nextEntry !== currentEntry
      currentEntry = nextEntry
      currentPath = path
      if (entryChanged) void restoreScroll()
    },
    { flush: 'post' },
  )
  onBeforeUnmount(() => {
    saveScroll()
    window.removeEventListener('scroll', onScroll)
    if (scrollTimer) window.clearTimeout(scrollTimer)
  })

  return { restoreScroll, saveScroll }
}
