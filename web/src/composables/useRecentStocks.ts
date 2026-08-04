import type { StockRef } from './useStockActions'

export interface RecentStock {
  symbol: string
  market: string
  name: string
  lastVisitedAt: string
}

const RECENT_LIMIT = 10
const STORAGE_PREFIX = 'qv:recent-stocks:'

function storageKey(userID: number) {
  return `${STORAGE_PREFIX}${userID}`
}

function normalizeRecentStock(value: unknown): RecentStock | null {
  if (!value || typeof value !== 'object') return null
  const input = value as Partial<RecentStock>
  const symbol = typeof input.symbol === 'string' ? input.symbol.trim().slice(0, 16) : ''
  const market = typeof input.market === 'string' ? input.market.trim().toLowerCase().slice(0, 8) : ''
  const name = typeof input.name === 'string' ? input.name.trim().slice(0, 64) : ''
  const lastVisitedAt = typeof input.lastVisitedAt === 'string' ? input.lastVisitedAt : ''
  if (!symbol || !market || !name || Number.isNaN(Date.parse(lastVisitedAt))) return null
  return { symbol, market, name, lastVisitedAt }
}

export function readRecentStocks(userID: number): RecentStock[] {
  if (!userID) return []
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(storageKey(userID)) || '[]')
    if (!Array.isArray(parsed)) return []
    return parsed.map(normalizeRecentStock).filter((item): item is RecentStock => !!item).slice(0, RECENT_LIMIT)
  } catch {
    return []
  }
}

export function recordRecentStock(userID: number, stock: StockRef): RecentStock[] {
  if (!userID) return []
  const symbol = stock.symbol.trim().slice(0, 16)
  const market = (stock.market || 'cn').trim().toLowerCase().slice(0, 8)
  const name = (stock.name || symbol).trim().slice(0, 64)
  if (!symbol || !market || !name) return readRecentStocks(userID)

  const key = `${market}\u0000${symbol.toLowerCase()}`
  const next: RecentStock[] = [
    { symbol, market, name, lastVisitedAt: new Date().toISOString() },
    ...readRecentStocks(userID).filter(
      (item) => `${item.market}\u0000${item.symbol.toLowerCase()}` !== key,
    ),
  ].slice(0, RECENT_LIMIT)
  try {
    localStorage.setItem(storageKey(userID), JSON.stringify(next))
  } catch {
    // 本地存储不可用时不阻断股票动作。
  }
  return next
}

export function clearRecentStocks(userID: number) {
  if (!userID) return
  try {
    localStorage.removeItem(storageKey(userID))
  } catch {
    // 清理失败不影响搜索本身。
  }
}
