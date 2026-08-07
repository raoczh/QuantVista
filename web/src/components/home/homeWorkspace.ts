export type HomeWorkspaceMode = 'pre' | 'intraday' | 'post'
export type HomeModePreference = 'auto' | HomeWorkspaceMode
export type HomeWorkspaceSectionID = 'todos' | 'positions' | 'watchlist' | 'research'

export interface ShanghaiClock {
  date: string
  time: string
  hour: number
  minute: number
}

export interface HomeSessionDecision {
  mode: HomeWorkspaceMode
  marketState: string
  confirmed: boolean
  source: 'market_state' | 'clock_fallback'
  clock: ShanghaiClock
}

const SECTION_ORDER: Record<HomeWorkspaceMode, HomeWorkspaceSectionID[]> = {
  pre: ['todos', 'research', 'watchlist', 'positions'],
  intraday: ['positions', 'todos', 'watchlist', 'research'],
  post: ['positions', 'research', 'todos', 'watchlist'],
}

const DEFAULT_EXPANDED: Record<HomeWorkspaceMode, HomeWorkspaceSectionID[]> = {
  pre: ['todos', 'research'],
  intraday: ['positions', 'todos'],
  post: ['positions', 'research'],
}

const MARKET_STATE_MODE: Record<string, HomeWorkspaceMode> = {
  pre_open: 'pre',
  trading: 'intraday',
  break: 'intraday',
  post_close: 'post',
  closed: 'post',
}

export function shanghaiClock(now = new Date()): ShanghaiClock {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  }).formatToParts(now)
  const value = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((part) => part.type === type)?.value || ''
  const hour = Number(value('hour'))
  const minute = Number(value('minute'))
  return {
    date: `${value('year')}-${value('month')}-${value('day')}`,
    time: `${value('hour')}:${value('minute')}`,
    hour: Number.isFinite(hour) ? hour : 0,
    minute: Number.isFinite(minute) ? minute : 0,
  }
}

function clockFallbackMode(clock: ShanghaiClock): HomeWorkspaceMode {
  const minutes = clock.hour * 60 + clock.minute
  if (minutes < 9 * 60 + 30) return 'pre'
  if (minutes <= 15 * 60 + 5) return 'intraday'
  return 'post'
}

/**
 * 交易状态由后端行情新鲜度契约提供。缺失时只用上海时间选择展示顺序，
 * 同时保持 confirmed=false，不能把周末或工作日近似冒充交易日事实。
 */
export function resolveHomeSession(now = new Date(), marketState?: string): HomeSessionDecision {
  const clock = shanghaiClock(now)
  const normalized = (marketState || '').trim().toLowerCase()
  const confirmedMode = MARKET_STATE_MODE[normalized]
  if (confirmedMode) {
    return {
      mode: confirmedMode,
      marketState: normalized,
      confirmed: true,
      source: 'market_state',
      clock,
    }
  }
  return {
    mode: clockFallbackMode(clock),
    marketState: 'unknown',
    confirmed: false,
    source: 'clock_fallback',
    clock,
  }
}

export function sortHomeSections(
  sections: readonly HomeWorkspaceSectionID[],
  mode: HomeWorkspaceMode,
): HomeWorkspaceSectionID[] {
  const rank = new Map(SECTION_ORDER[mode].map((id, index) => [id, index]))
  return [...sections].sort((a, b) => (rank.get(a) ?? 99) - (rank.get(b) ?? 99))
}

export function defaultExpandedSections(mode: HomeWorkspaceMode): HomeWorkspaceSectionID[] {
  return [...DEFAULT_EXPANDED[mode]]
}

export interface PriorityItem {
  priority: number
  time?: string | null
  ref_id?: number
}

export function sortPriorityItems<T extends PriorityItem>(items: readonly T[]): T[] {
  return [...items].sort((a, b) => {
    if (a.priority !== b.priority) return a.priority - b.priority
    const timeA = a.time ? Date.parse(a.time) : Number.POSITIVE_INFINITY
    const timeB = b.time ? Date.parse(b.time) : Number.POSITIVE_INFINITY
    if (timeA !== timeB) return timeA - timeB
    return (a.ref_id || 0) - (b.ref_id || 0)
  })
}

export interface PositionRiskItem {
  id: number
  below_stop_loss: boolean
  near_stop_loss: boolean
  short_term_review: boolean
  analysis_stale: boolean
}

export function positionRiskPriority(item: PositionRiskItem): number {
  if (item.below_stop_loss) return 0
  if (item.near_stop_loss) return 1
  if (item.short_term_review) return 2
  if (item.analysis_stale) return 3
  return Number.POSITIVE_INFINITY
}

export function sortPositionRisks<T extends PositionRiskItem>(items: readonly T[]): T[] {
  return [...items]
    .filter((item) => Number.isFinite(positionRiskPriority(item)))
    .sort((a, b) => positionRiskPriority(a) - positionRiskPriority(b) || a.id - b.id)
}

export interface WatchChangeItem {
  id: number
  is_pinned: boolean
  quote_ok: boolean
  change_pct: number
}

export function rankPinnedWatchChanges<T extends WatchChangeItem>(items: readonly T[]): T[] {
  return [...items]
    .filter((item) => item.is_pinned)
    .sort((a, b) => {
      if (a.quote_ok !== b.quote_ok) return a.quote_ok ? -1 : 1
      const changeDiff = Math.abs(b.change_pct) - Math.abs(a.change_pct)
      return changeDiff || a.id - b.id
    })
}
