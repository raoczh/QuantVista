import type { RecommendationItem, RecStatus, RecTracking } from '@/api/recommendation'
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
  if (plan?.status === 'ready') {
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
    reasons: plan?.unavailable_reasons?.length
      ? plan.unavailable_reasons
      : ['系统未给出可执行计划，此处仅登记你的实际买入事实，不代表买入建议'],
  }
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
