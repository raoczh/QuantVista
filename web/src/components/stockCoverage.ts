export type StockCoverageStatus = 'available' | 'missing' | 'error' | 'stale' | 'unknown'

export interface StockCoverageItem {
  key: string
  label: string
  status: StockCoverageStatus
  source: string
  asOf?: string
  note?: string
}

export interface CoverageObservation {
  observed: boolean
  available: boolean
  error?: string
  stale?: boolean
  unknown?: boolean
}

export function resolveCoverageStatus(input: CoverageObservation): StockCoverageStatus {
  if (input.error) return 'error'
  if (!input.observed || input.unknown) return 'unknown'
  if (!input.available) return 'missing'
  if (input.stale) return 'stale'
  return 'available'
}

// 每次切股/刷新领取新 epoch；迟到响应只有 token 仍为当前值时才允许落地。
export function createLoadEpoch() {
  let value = 0
  return {
    next: () => ++value,
    invalidate: () => { value++ },
    isCurrent: (token: number) => token === value,
  }
}
