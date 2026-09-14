import type { PoolCandidate, RecReject, RecommendationItem, RecStatus, RecTracking } from '@/api/recommendation'
import type { TaskStatus } from '@/api/taskCenter'

export type RecommendationDecisionState = 'buy_research' | 'watch' | 'no_action' | 'insufficient' | 'expired'
export type TrackingState = 'pending' | 'immature' | 'tracking' | 'expired' | 'insufficient' | 'settled'

export function recommendationDecisionState(item: RecommendationItem): RecommendationDecisionState {
  const plan = item.detail?.execution_plan
  if (item.status && ['take_profit', 'stop_loss', 'expired'].includes(item.status.outcome)) return 'expired'
  if (!item.detail || plan?.data_status === 'stale' || plan?.data_status === 'unknown') return 'insufficient'
  if (plan?.status === 'not_suitable') return 'no_action'
  if (item.action === 'buy' && plan?.status === 'ready') return 'buy_research'
  return 'watch'
}

export const RECOMMENDATION_DECISION_LABEL: Record<RecommendationDecisionState, string> = {
  buy_research: '买入研究',
  watch: '观察',
  no_action: '暂不行动',
  insufficient: '数据不足',
  expired: '已失效',
}

export function confidenceExplanation(value: number, system?: string): string {
  if (system === 'low' || value < 45) return '把握较低，需要补数据或等待更多信号'
  if (system === 'high' && value >= 75) return '证据较完整，但仍需自行核对风险'
  return '有一定依据，仍可能因行情变化而失效'
}

// 旧快照省略零分，但仍记录已计算的排名；没有排名时保留“未提供”的语义。
export function rankedScore(score?: number, rank?: number): number | undefined {
  return score ?? (rank != null && rank > 0 ? 0 : undefined)
}

export function scoringVersionLabel(version?: string): string {
  return ({ qr1: '历史质量规则', qr2: '历史质量规则', qr3: '质量规则', additive_sp1: '原加法评分对照', ridge1: '学习排序' } as Record<string,string>)[version || ''] || version || '历史未记录'
}

export function scoreComponentLabel(key: string): string {
  return ({ technical: '技术基础', setup: '形态质量', entry: '入场质量', risk: '交易风险', fundamentals: '财务质量', context: '信息背景' } as Record<string,string>)[key] || key
}

export function omittedCandidates(raw?: string): number {
  if (!raw) return 0
  try {
    const value = JSON.parse(raw)?.pool_omitted
    return Number.isSafeInteger(value) && value > 0 ? value : 0
  } catch { return 0 }
}

/**
 * 追踪状态分档。
 *
 * `no_data` 在后端是一个混合态，前端必须按已过交易日数把它拆开——否则「今天刚生成」
 * 这种必然且会自愈的正常情况会被显示成警告：
 *   - 推荐日之后还没有任何交易日 → pending。追踪要求 `trade_date > 推荐日` 的日线，
 *     当天生成必然查不到；当日实时行情也被 `today <= recDate` 挡住不追加。次日日线
 *     到位后后台任务（每 2 小时）会自动推进，用户无需做任何事。
 *   - 已过交易日却仍无日线 → insufficient。这才是真的取数异常（或参考价缺失），
 *     值得提示用户手动刷新。
 */
export function trackingState(status: RecTracking | null): TrackingState {
  if (!status) return 'insufficient'
  if (status.outcome === 'no_data') {
    return status.elapsed_trade_days > 0 ? 'insufficient' : 'pending'
  }
  if (status.outcome === 'take_profit' || status.outcome === 'stop_loss') return 'settled'
  if (status.outcome === 'expired') return 'expired'
  if (status.outcome === 'tracking') return 'tracking'
  return 'immature'
}

export const TRACKING_STATE_LABEL: Record<TrackingState, string> = {
  pending: '等待首个交易日',
  immature: '尚未成熟',
  tracking: '正常跟踪',
  expired: '已失效',
  insufficient: '数据不足',
  settled: '已结算',
}

/**
 * 建仓入口的文案与预填行为。
 *
 * 登记既成事实与「系统建议你买入」是两件事，不该共用 execution_plan.status 这一个闸门。
 * 原先按钮只在 status==='ready' 时出现，于是偏好未完成、资金未设置、行情 stale、原动作
 * 为观察等情况下，用户即使真的买了也**没有任何带血缘的建仓入口**，追踪体系直接漏账。
 *
 * 现在按钮恒显，只做语气分层：ready 才是「按推荐记录建仓」并预填计划数量；其余一律降级
 * 为「我已买入，登记到这条推荐」且不预填数量——系统没算出计划量，硬编一个反而误导。
 */
export interface PositionEntryAction {
  ready: boolean
  label: string
  /** 预填买入数量；0 表示不预填（由用户按实际成交填写）。 */
  prefillQuantity: number
  /** 非 ready 时的原因，挂 tooltip 说明「这不是买入建议」。 */
  reasons: string[]
}

export function positionEntryAction(item: RecommendationItem): PositionEntryAction {
  const plan = item.detail?.execution_plan
  const expired = !!item.status && ['take_profit', 'stop_loss', 'expired'].includes(item.status.outcome)
  const stale = plan?.data_status === 'stale' || plan?.data_status === 'unknown'
  if (plan?.status === 'ready' && !expired && !stale && item.action !== 'watch') {
    return {
      ready: true,
      label: '按推荐记录建仓',
      prefillQuantity: plan.quantity > 0 ? plan.quantity : 0,
      reasons: [],
    }
  }
  return {
    ready: false,
    label: '我已买入，登记到这条推荐',
    prefillQuantity: 0,
    reasons: expired ? ['推荐已结算或失效，请按实际成交登记，旧计划数量不再作为当前参考']
      : stale ? ['原计划数据已过期或时点未知，请按实际成交登记']
      : plan?.unavailable_reasons?.length
      ? plan.unavailable_reasons
      : ['系统未给出可执行计划，此处仅登记你的实际买入事实，不代表买入建议'],
  }
}

function snapshotArray<T>(raw: string | undefined, valid: (value: unknown) => value is T): { items: T[]; invalid: boolean } {
  if (!raw) return { items: [], invalid: false }
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return { items: [], invalid: true }
    const items = parsed.filter(valid)
    return { items, invalid: items.length !== parsed.length }
  } catch { return { items: [], invalid: true } }
}
const isObject = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value)
const strings = (value: unknown) => value === undefined || (Array.isArray(value) && value.every(item => typeof item === 'string'))
const optionalNumber = (value: unknown) => value == null || (typeof value === 'number' && Number.isFinite(value))
const optionalString = (value: unknown) => value === undefined || typeof value === 'string'
const finiteMap = (value: unknown) => value === undefined || (isObject(value) && Object.values(value).every(v => typeof v === 'number' && Number.isFinite(v)))
const validCurrentCheck = (value: unknown) => value === undefined || (isObject(value) &&
  ['matched', 'missed', 'unknown'].includes(String(value.status)) && Number.isInteger(value.checked) && strings(value.missed) && strings(value.unknown))
const validEntryQuality = (value: unknown) => value === undefined || (isObject(value) &&
  ['aligned', 'extended', 'waiting_confirmation', 'insufficient'].includes(String(value.status)) && strings(value.reasons))
export function entryQualityLabel(status?: string): string {
  return ({ aligned: '入场距离合理', extended: '延伸较大，等待', waiting_confirmation: '等待企稳确认', insufficient: '入场依据不足' } as Record<string, string>)[status || ''] || '历史未记录'
}

export function parseSourceCoverage(raw?: string): Array<{ source: string; observed: number; intake: number; scored: number; ranked: number; sent: number; omitted: number }> {
  if (!raw) return []
  try {
    const rows = JSON.parse(raw)?.source_coverage
    if (!Array.isArray(rows)) return []
    return rows.filter(row => isObject(row) && typeof row.source === 'string' &&
      ['observed', 'intake', 'scored', 'ranked', 'sent', 'omitted'].every(key => typeof row[key] === 'number' && Number.isSafeInteger(row[key]) && row[key] >= 0))
  } catch { return [] }
}

/** 历史 JSON 快照逐项核验，损坏行不参与展示或统计，调用方明确披露缺失。 */
export function parseCandidateSnapshot(raw?: string) {
  return snapshotArray<PoolCandidate>(raw, (value): value is PoolCandidate => {
    if (!isObject(value) || typeof value.symbol !== 'string' || !value.symbol.trim() ||
      !optionalString(value.name) || !optionalString(value.market) || !optionalString(value.source) ||
      !optionalString(value.excluded) || !strings(value.sources) || !strings(value.bonus) ||
      typeof value.change_pct !== 'number' || !Number.isFinite(value.change_pct)) return false
    if (['price', 'score', 'ranking_score', 'rank'].some(key => !optionalNumber(value[key]))) return false
    const hit = value.strategy_hit
    if (hit != null && (!isObject(hit) || !Number.isInteger(hit.total) || !Number.isInteger(hit.hit) ||
      typeof hit.full !== 'boolean' || !strings(hit.matched) || !strings(hit.missed) || !strings(hit.unknown) ||
      !finiteMap(hit.values) || !validCurrentCheck(hit.current))) return false
    const timing = value.time_facts
    if (timing != null && (!isObject(timing) || typeof timing.signal_date !== 'string' ||
      !optionalNumber(timing.signal_close) || !finiteMap(timing.current_returns))) return false
    const final = value.final_check
    if (final != null && (!isObject(final) || typeof final.passed !== 'boolean' || !optionalNumber(final.price) ||
      !optionalString(final.quote_as_of) || !optionalString(final.reason) || !validCurrentCheck(final.strategy) || !validEntryQuality(final.entry_quality))) return false
    if (!validEntryQuality(value.entry_quality)) return false
    const quality = value.signal_quality
    if (quality != null && (!isObject(quality) || !strings(quality.missing) ||
      !['atr', 'breakout_distance_atr', 'ma20_distance_atr', 'compression', 'volume_contraction', 'close_location', 'support', 'support_distance_atr']
        .every(key => optionalNumber(quality[key])))) return false
    const breakdown = value.score_breakdown
    if (breakdown != null && (!isObject(breakdown) || typeof breakdown.version !== 'string' || typeof breakdown.profile !== 'string' ||
      typeof breakdown.valid !== 'boolean' || typeof breakdown.total !== 'number' || !Number.isFinite(breakdown.total) || !strings(breakdown.missing) ||
      !Array.isArray(breakdown.components) || !breakdown.components.every(part => isObject(part) && typeof part.key === 'string' &&
        typeof part.value === 'number' && Number.isFinite(part.value) && typeof part.status === 'string' && strings(part.notes)))) return false
    const comparison = value.scoring_comparison
    if (comparison != null && (!isObject(comparison) || typeof comparison.legacy_version !== 'string' || typeof comparison.quality_version !== 'string' ||
      typeof comparison.legacy_score !== 'number' || !Number.isFinite(comparison.legacy_score) || typeof comparison.quality_score !== 'number' || !Number.isFinite(comparison.quality_score) ||
      !optionalNumber(comparison.learned_score) || !optionalString(comparison.learned_version) || !optionalString(comparison.model_hash))) return false
    return true
  })
}

export function parseRejectedSnapshot(raw?: string) {
  return snapshotArray<RecReject>(raw, (value): value is RecReject => isObject(value) &&
    typeof value.symbol === 'string' && !!value.symbol.trim() && optionalString(value.name) && typeof value.reason === 'string')
}

export function businessStatusLabel(status: RecStatus): string {
  return ({ processing: '运行中', success: '成功', degraded: '部分成功', failed: '失败' } as const)[status]
}

export function taskStatusLabel(status: TaskStatus | null): string {
  if (!status) return '未开始'
  return ({
    queued: '排队中',
    running: '运行中',
    success: '成功',
    degraded: '部分成功',
    failed: '失败',
    canceled: '已取消',
  } as const)[status]
}

const EXCLUSION_LABELS: Array<[RegExp, string]> = [
  [/停牌|suspend/i, '停牌'],
  [/流动性|成交额|换手.*不足|liquidity/i, '流动性不足'],
  [/过期|stale|数据.*旧/i, '数据过期'],
  [/黑名单|blacklist/i, '已在黑名单'],
  [/涨停|limit.?up/i, '已涨停，当前难以成交'],
]

export function exclusionReason(reason: string): string {
  const text = reason.trim()
  if (!text) return '不满足当前策略条件'
  const matched = EXCLUSION_LABELS.find(([pattern]) => pattern.test(text))
  return matched ? `${matched[1]}：${text}` : text
}
