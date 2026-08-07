import type { AlertInput, AlertKind, AlertRule } from '@/api/alert'

export type AlertTemplateGroup = 'price' | 'position' | 'financial' | 'technical'

export type AlertTemplateId =
  | 'target_price'
  | 'pct_move'
  | 'cost_loss'
  | 'cost_gain'
  | 'peak_drawdown'
  | 'earn_date'
  | 'earn_forecast'
  | 'volume_surge'
  | 'amplitude'
  | 'moving_average'
  | 'breakout'

export interface AlertTemplate {
  id: AlertTemplateId
  group: AlertTemplateGroup
  title: string
  summary: string
  kind: AlertKind
  op: 'gte' | 'lte'
  threshold?: number
  period?: number
  once: boolean
}

export type AlertWizardInput = AlertInput & {
  symbol: string
  market: string
  name: string
}

export interface AlertStockContext {
  symbol: string
  market: string
  name: string
  nonce?: string
}

export const ALERT_TEMPLATE_GROUPS: { id: AlertTemplateGroup; label: string }[] = [
  { id: 'price', label: '价格与异动' },
  { id: 'position', label: '我的持仓' },
  { id: 'financial', label: '财报事件' },
  { id: 'technical', label: '技术观察' },
]

export const ALERT_TEMPLATES: AlertTemplate[] = [
  {
    id: 'target_price',
    group: 'price',
    title: '到目标价',
    summary: '当日最高或最低触及指定价格',
    kind: 'price',
    op: 'gte',
    once: true,
  },
  {
    id: 'pct_move',
    group: 'price',
    title: '涨跌幅异动',
    summary: '当日涨幅或跌幅达到指定比例',
    kind: 'pct_change',
    op: 'gte',
    threshold: 5,
    once: true,
  },
  {
    id: 'cost_loss',
    group: 'position',
    title: '跌破我的成本',
    summary: '相对当前持仓加权成本下跌',
    kind: 'cost_drawdown',
    op: 'gte',
    threshold: 8,
    once: false,
  },
  {
    id: 'cost_gain',
    group: 'position',
    title: '达到持仓收益',
    summary: '相对当前持仓加权成本上涨',
    kind: 'cost_gain',
    op: 'gte',
    threshold: 20,
    once: false,
  },
  {
    id: 'peak_drawdown',
    group: 'position',
    title: '从持仓高点回撤',
    summary: '从这笔持仓建立后的峰值回落',
    kind: 'peak_drawdown',
    op: 'gte',
    threshold: 15,
    once: false,
  },
  {
    id: 'earn_date',
    group: 'financial',
    title: '财报披露临近',
    summary: '预约披露日进入提前提醒窗口',
    kind: 'earn_date',
    op: 'gte',
    threshold: 3,
    once: false,
  },
  {
    id: 'earn_forecast',
    group: 'financial',
    title: '新业绩预告',
    summary: '本地财报数据出现新的业绩预告',
    kind: 'earn_fcst',
    op: 'gte',
    threshold: 0,
    once: false,
  },
  {
    id: 'volume_surge',
    group: 'technical',
    title: '放量观察',
    summary: '当日成交量达到 20 日均量倍数',
    kind: 'volume_surge',
    op: 'gte',
    threshold: 2,
    once: true,
  },
  {
    id: 'amplitude',
    group: 'technical',
    title: '振幅观察',
    summary: '当日振幅达到指定比例',
    kind: 'amplitude',
    op: 'gte',
    threshold: 5,
    once: true,
  },
  {
    id: 'moving_average',
    group: 'technical',
    title: '均线观察',
    summary: '现价站上或跌破指定周期均线',
    kind: 'ma',
    op: 'gte',
    period: 20,
    once: true,
  },
  {
    id: 'breakout',
    group: 'technical',
    title: '突破观察',
    summary: '创指定窗口的新高或新低',
    kind: 'breakout',
    op: 'gte',
    period: 20,
    once: true,
  },
]

export const ALERT_KIND_OPTIONS: { label: string; value: AlertKind }[] = [
  { label: '到价提醒', value: 'price' },
  { label: '涨跌幅异动', value: 'pct_change' },
  { label: '均线（站上/跌破）', value: 'ma' },
  { label: '突破（新高/新低）', value: 'breakout' },
  { label: '放量/缩量（20 日均量）', value: 'volume_surge' },
  { label: '振幅异动', value: 'amplitude' },
  { label: '财报披露临近', value: 'earn_date' },
  { label: '新业绩预告', value: 'earn_fcst' },
  { label: '相对我的成本涨幅', value: 'cost_gain' },
  { label: '相对我的成本跌幅', value: 'cost_drawdown' },
  { label: '从持仓期峰值回撤', value: 'peak_drawdown' },
]

const ALERT_TEMPLATE_KIND_MAP = {
  price: 'target_price',
  pct_change: 'pct_move',
  ma: 'moving_average',
  breakout: 'breakout',
  volume_surge: 'volume_surge',
  amplitude: 'amplitude',
  earn_date: 'earn_date',
  earn_fcst: 'earn_forecast',
  cost_gain: 'cost_gain',
  cost_drawdown: 'cost_loss',
  peak_drawdown: 'peak_drawdown',
} satisfies Record<AlertKind, AlertTemplateId>

export function templateById(id: AlertTemplateId | null): AlertTemplate | undefined {
  return ALERT_TEMPLATES.find((item) => item.id === id)
}

export function makeTemplateInput(template: AlertTemplate): AlertWizardInput {
  return {
    symbol: '',
    market: 'cn',
    name: '',
    kind: template.kind,
    op: template.op,
    threshold: template.threshold,
    period: template.period ?? 20,
    once: template.once,
    note: '',
  }
}

export function inputFromRule(rule: AlertRule): AlertWizardInput {
  return {
    symbol: rule.symbol,
    market: rule.market,
    name: rule.name,
    kind: rule.kind,
    op: rule.op,
    threshold: rule.threshold,
    period: rule.period,
    once: rule.once,
    note: rule.note,
  }
}

/** 只有模板能无损表达全部方向语义时才反推；否则保留原字段进入专家模式。 */
export function inferAlertTemplate(rule: AlertRule): AlertTemplateId | null {
  const templateId = ALERT_TEMPLATE_KIND_MAP[rule.kind]
  const template = templateById(templateId)
  if (!template || template.kind !== rule.kind) return null
  if ((rule.kind === 'volume_surge' || rule.kind === 'amplitude') && rule.op !== 'gte') return null
  if (
    (rule.kind === 'earn_date' || rule.kind === 'earn_fcst' ||
      rule.kind === 'cost_gain' || rule.kind === 'cost_drawdown' || rule.kind === 'peak_drawdown') &&
    rule.op !== 'gte'
  ) {
    return null
  }
  return template.id
}

export function isEarnAlertKind(kind: string): boolean {
  return kind === 'earn_date' || kind === 'earn_fcst'
}

export function alertNeedsThreshold(kind: string): boolean {
  return !['ma', 'breakout', 'earn_fcst'].includes(kind)
}

export function alertNeedsPeriod(kind: string): boolean {
  return kind === 'ma' || kind === 'breakout'
}

function compactNumber(value: number | undefined, digits = 2): string {
  if (value == null || !Number.isFinite(value)) return '未填写'
  return String(Number(value.toFixed(digits)))
}

export function alertConditionText(input: AlertInput): string {
  const threshold = Number(input.threshold)
  switch (input.kind) {
    case 'price':
      return `当日${input.op === 'gte' ? '最高价达到或超过' : '最低价达到或低于'} ${compactNumber(threshold)} 元`
    case 'pct_change':
      return input.op === 'gte'
        ? `当日涨幅达到 ${compactNumber(Math.abs(threshold))}%`
        : `当日跌幅达到 ${compactNumber(Math.abs(threshold))}%`
    case 'ma':
      return `现价${input.op === 'gte' ? '站上' : '跌破'} MA${input.period || 0}`
    case 'breakout':
      return `当日${input.op === 'gte' ? '创' : '跌至'}近 ${input.period || 0} 日${input.op === 'gte' ? '新高' : '新低'}`
    case 'volume_surge':
      return input.op === 'gte'
        ? `当日成交量达到 20 日均量的 ${compactNumber(threshold)} 倍`
        : `当日成交量低于 20 日均量的 ${compactNumber(threshold)} 倍`
    case 'amplitude':
      return `当日振幅${input.op === 'gte' ? '达到' : '低于'} ${compactNumber(threshold)}%`
    case 'earn_date':
      return `距预约财报披露日不超过 ${compactNumber(threshold, 0)} 个自然日`
    case 'earn_fcst':
      return '出现尚未提醒过的新业绩预告'
    case 'cost_gain':
      return `当日最高价相对我的当前持仓成本上涨 ${compactNumber(threshold)}%`
    case 'cost_drawdown':
      return `当日最低价相对我的当前持仓成本下跌 ${compactNumber(threshold)}%`
    case 'peak_drawdown':
      return `价格从这笔持仓的持仓期峰值回撤 ${compactNumber(threshold)}%`
    default:
      return '满足专家规则参数'
  }
}

export function alertCheckTiming(kind: string): string {
  if (isEarnAlertKind(kind)) return '每日财报数据刷新后检查，也可手动立即检查'
  return '交易窗口约每 2 分钟检查，非交易时段低频检查，也可手动立即检查'
}

export function alertDataBoundary(kind: string): string {
  switch (kind) {
    case 'cost_gain':
    case 'cost_drawdown':
      return '成本取持仓账本中当前持仓的加权平均成本；加减仓后会随账本重算。已平仓、没有对应持仓或没有当前有效行情时不触发。'
    case 'peak_drawdown':
      return '峰值取持仓账本自建仓或最近加仓起算的持仓期最高价，并随除权折算；峰值未建立、已平仓或没有当前有效行情时不触发。'
    case 'earn_date':
      return '披露日取每日刷新的本地预约披露记录；尚未取得预约日、记录已过期或本轮刷新失败时不会凭空触发。'
    case 'earn_fcst':
      return '业绩预告取每日刷新的本地财报记录，并按预告发布日期识别新事实；没有新记录或刷新失败时不触发。'
    case 'ma':
    case 'breakout':
    case 'volume_surge':
      return '判断使用当前有效行情和行情服务提供的日线窗口；行情过期、样本不足或日线不可用时本轮跳过，不用旧数据误报。'
    case 'amplitude':
      return '振幅优先取当前有效估值行情，缺失时由当前有效报价的最高、最低和昨收计算；行情过期或字段不足时本轮跳过。'
    default:
      return '判断使用当前有效行情的现价及当日高低价；行情过期或不可用时本轮跳过，不用旧价触发。'
  }
}

export function alertPreview(input: AlertInput, target: string): string {
  const lifecycle = input.kind && ['cost_gain', 'cost_drawdown', 'peak_drawdown'].includes(input.kind)
    ? '持续生效，持仓类不会因单次命中暂停'
    : input.once
      ? '命中后自动暂停，仅提醒一次'
      : '命中后继续生效，可在新事实或后续交易日再次提醒'
  return `监控 ${target}；${alertConditionText(input)}时提醒；${alertCheckTiming(input.kind)}；${lifecycle}。`
}
