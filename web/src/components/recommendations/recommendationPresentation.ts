import type { RecommendationItem, RecStatus, RecTracking } from '@/api/recommendation'
import type { TaskStatus } from '@/api/taskCenter'

export type RecommendationDecisionState = 'buy_research' | 'watch' | 'no_action' | 'insufficient' | 'expired'
export type TrackingState = 'immature' | 'tracking' | 'expired' | 'insufficient' | 'settled'

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

export function trackingState(status: RecTracking | null): TrackingState {
  if (!status || status.outcome === 'no_data') return 'insufficient'
  if (status.outcome === 'take_profit' || status.outcome === 'stop_loss') return 'settled'
  if (status.outcome === 'expired') return 'expired'
  if (status.outcome === 'tracking') return 'tracking'
  return 'immature'
}

export const TRACKING_STATE_LABEL: Record<TrackingState, string> = {
  immature: '尚未成熟',
  tracking: '正常跟踪',
  expired: '已失效',
  insufficient: '数据不足',
  settled: '已结算',
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
