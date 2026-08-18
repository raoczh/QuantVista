import type { AnalysisRecord, AnalysisRating, AnalysisStatus, AnalysisView } from '@/api/analysis'

export const ANALYSIS_MODULE_LABELS = {
  stock: '单票分析',
  position: '本人持仓组合分析',
  watchlist: '本人自选组合分析',
  market: '全市场分析',
  sector: '板块分析',
} as const

export function analysisStatusLabel(status: AnalysisStatus): string {
  return ({ processing: '运行中', success: '成功', degraded: '部分成功', failed: '失败' } as const)[status]
}

export function ratingLabel(rating: AnalysisRating | ''): string {
  return ({ bullish: '偏多', neutral: '中性', bearish: '偏空' } as Record<string, string>)[rating] || '暂时无法判断'
}

export function suggestedAction(view: AnalysisView): string {
  if (view.stale_mode) return '等待行情恢复后重新评估，不按旧盘面采取行动'
  if (view.status !== 'success' || !view.result) return '先处理数据缺口，暂不根据本次结果行动'
  if (view.result.rating === 'bullish') return '继续核对买入条件和风险边界，不自动交易'
  if (view.result.rating === 'bearish') return '优先检查风险和失效条件；持仓卖出仍进入逐笔卖出决策'
  return '保持观察，等待方向和证据更明确'
}

export function recordStockName(record: Pick<AnalysisRecord, 'symbol' | 'target' | 'title'>): string {
  const values = [record.target, record.title].map((value) => value?.trim()).filter(Boolean) as string[]
  return values.find((value) => value.toLowerCase() !== record.symbol.toLowerCase()) || ''
}

export interface SnapshotFreshness {
  capturedAt: string
  quoteAsOf: string
  quoteSource: string
  barsAsOf: string
  freshness: 'fresh' | 'stale' | 'partial' | 'unknown'
  note: string
}

export function parseSnapshotFreshness(raw?: string): SnapshotFreshness {
  const fallback: SnapshotFreshness = {
    capturedAt: '', quoteAsOf: '', quoteSource: '', barsAsOf: '', freshness: 'unknown', note: '快照未提供数据新鲜度字段',
  }
  if (!raw) return fallback
  try {
    const value = JSON.parse(raw) as Record<string, unknown>
    const status = String(value.freshness_status || '').toLowerCase()
    const freshness = status === 'fresh' ? 'fresh' : status === 'stale' ? 'stale' : status === 'partial' ? 'partial' : 'unknown'
    return {
      capturedAt: String(value.data_as_of || value.captured_at || ''),
      quoteAsOf: String(value.quote_as_of || value.as_of || ''),
      quoteSource: String(value.quote_source || ''),
      barsAsOf: String(value.bars_as_of || ''),
      freshness,
      note: String(value.freshness_note || value.as_of_note || fallback.note),
    }
  } catch {
    return fallback
  }
}

export function freshnessLabel(value: SnapshotFreshness['freshness']): string {
  return ({ fresh: '行情可用', stale: '数据已过期', partial: '数据不完整', unknown: '新鲜度未知' } as const)[value]
}
