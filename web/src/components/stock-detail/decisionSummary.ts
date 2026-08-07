import type { AnnouncementItem } from '@/api/announcement'
import type { StockCorpEvents } from '@/api/event'
import type { StockFinance } from '@/api/finance'
import type { Bar, Quote, StockFundFlow, StockScore, Valuation } from '@/api/market'
import type { NewsItem } from '@/api/news'

export type StockSectionPhase = 'idle' | 'loading' | 'refreshing' | 'ready' | 'empty' | 'error'
export type DecisionTone = 'positive' | 'negative' | 'warning' | 'neutral' | 'unknown'

export interface PositionRelationSummary {
  lots: number
  quantity: number
  averageCost: number
  remainingCost: number
  profitAmount: number | null
  profitPct: number | null
  realizedPnl: number
  quoteFresh: boolean
  asOf?: string
  belowStopLoss: boolean
  nearStopLoss: boolean
  stopLoss?: number
}

export interface DecisionItem {
  id: string
  title: string
  value: string
  detail: string
  evidence: string
  source: string
  asOf: string
  tone: DecisionTone
}

export interface DecisionSummary {
  changes: DecisionItem[]
  risks: DecisionItem[]
  recentEvent: DecisionItem
  invalidation: string
}

export interface DecisionSummaryInput {
  quote: Quote | null
  position: PositionRelationSummary | null
  bars: Bar[]
  valuation: Valuation | null
  score: StockScore | null
  fundflow: StockFundFlow | null
  finance: StockFinance | null
  corpEvents: StockCorpEvents | null
  announcements: AnnouncementItem[]
  news: NewsItem[]
  eventPhase: StockSectionPhase
  eventPartial: boolean
  fundamentalPhase: StockSectionPhase
  now?: Date
}

function signedPct(value: number) {
  return `${value > 0 ? '+' : ''}${value.toFixed(2)}%`
}

function signedAmount(value: number) {
  return `${value > 0 ? '+' : ''}${value.toFixed(2)} 元`
}

function toneFor(value: number): DecisionTone {
  if (value > 0) return 'positive'
  if (value < 0) return 'negative'
  return 'neutral'
}

function phaseUnknown(phase: StockSectionPhase) {
  return phase === 'idle' || phase === 'loading' || phase === 'error'
}

function timestamp(value: string) {
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : 0
}

function unknownEvent(phase: StockSectionPhase, partial: boolean): DecisionItem {
  const settledOrRefreshing = phase === 'ready' || phase === 'empty' || phase === 'refreshing'
  const detail = partial && settledOrRefreshing
    ? '公司行动已检查；公告和新闻将在进入“事件”页签后加载，当前不能据此判断近期无事。'
    : phase === 'idle'
    ? '事件数据尚未请求，进入“事件”页签后加载。'
    : phase === 'loading'
      ? '事件数据正在加载，完成后会更新此处。'
      : phase === 'refreshing'
        ? '保留最近一次事件结果并正在刷新；这不等于未来没有其他风险。'
      : phase === 'error'
        ? '事件数据读取失败，当前无法判断近期事件。'
        : '已请求的事件来源未返回近期记录；这不等于未来没有其他风险。'
  return {
    id: 'event-unknown',
    title: partial && settledOrRefreshing
      ? '近期事件待补全'
      : settledOrRefreshing ? '未发现近期事件' : '近期事件 unknown',
    value: partial && settledOrRefreshing ? 'partial' : 'unknown',
    detail,
    evidence: '没有可用于展示的确定性事件记录',
    source: 'unknown',
    asOf: 'unknown',
    tone: 'unknown',
  }
}

function selectRecentEvent(input: DecisionSummaryInput): DecisionItem {
  const now = input.now || new Date()
  const candidates: Array<DecisionItem & { eventAt: number }> = []

  for (const lift of input.corpEvents?.lifts || []) {
    const eventAt = timestamp(`${lift.free_date}T00:00:00`)
    if (!eventAt) continue
    candidates.push({
      id: `lift-${lift.id}`,
      title: '限售股解禁',
      value: `${lift.free_date} · 占流通 ${lift.free_ratio.toFixed(2)}%`,
      detail: lift.free_type || '限售股解禁安排',
      evidence: `${(lift.free_shares / 10_000).toFixed(0)} 万股，参考市值 ${(lift.lift_market_cap / 100_000_000).toFixed(2)} 亿元`,
      source: '本地事件表 / 东财',
      asOf: lift.free_date,
      tone: lift.free_ratio >= 10 ? 'warning' : 'neutral',
      eventAt,
    })
  }
  for (const action of input.corpEvents?.actions || []) {
    const date = action.notice_date || action.report_date
    const eventAt = timestamp(`${date}T00:00:00`)
    if (!eventAt) continue
    candidates.push({
      id: `action-${action.id}`,
      title: '分红送转方案',
      value: date,
      detail: action.plan_profile || '公司披露分红送转方案',
      evidence: `报告期 ${action.report_date}${action.ex_date ? `，除权日 ${action.ex_date}` : ''}`,
      source: '本地事件表 / 东财',
      asOf: date,
      tone: 'neutral',
      eventAt,
    })
  }
  for (const item of input.announcements) {
    const eventAt = timestamp(`${item.notice_date}T00:00:00`)
    if (!eventAt) continue
    candidates.push({
      id: `announcement-${item.art_code}`,
      title: '公司公告',
      value: item.notice_date,
      detail: item.title,
      evidence: item.notice_type || '公告原文',
      source: '东财公告',
      asOf: item.notice_date,
      tone: 'neutral',
      eventAt,
    })
  }
  for (const item of input.news) {
    const eventAt = timestamp(item.publish_time)
    if (!eventAt) continue
    candidates.push({
      id: `news-${item.id}`,
      title: '相关新闻',
      value: new Date(eventAt).toLocaleString('zh-CN', { hour12: false }),
      detail: item.title,
      evidence: item.summary || '新闻标题',
      source: item.source === 'cls' ? '财联社' : item.source === 'eastmoney' ? '东财' : item.source,
      asOf: item.publish_time,
      tone: item.sentiment === 'negative' ? 'warning' : item.sentiment === 'positive' ? 'positive' : 'neutral',
      eventAt,
    })
  }

  if (!candidates.length) return unknownEvent(input.eventPhase, input.eventPartial)
  const nowAt = now.getTime()
  candidates.sort((a, b) => Math.abs(a.eventAt - nowAt) - Math.abs(b.eventAt - nowAt))
  const { eventAt: _eventAt, ...selected } = candidates[0]
  return selected
}

export function buildDecisionSummary(input: DecisionSummaryInput): DecisionSummary {
  const changes: DecisionItem[] = []
  const risks: DecisionItem[] = []
  const q = input.quote
  const position = input.position

  if (position?.profitPct != null && position.profitAmount != null) {
    changes.push({
      id: 'position-pnl',
      title: '持仓浮盈亏',
      value: `${signedPct(position.profitPct)} · ${signedAmount(position.profitAmount)}`,
      detail: `按剩余持仓 ${position.quantity.toFixed(0)} 股、平均成本 ${position.averageCost.toFixed(2)} 元统计。`,
      evidence: `剩余成本 ${position.remainingCost.toFixed(2)} 元；${position.lots} 笔持仓账本`,
      source: '个人持仓账本 + 有效行情',
      asOf: position.asOf || 'unknown',
      tone: toneFor(position.profitPct),
    })
  }

  if (q) {
    const quoteFresh = q.freshness?.freshness_status === 'fresh'
    changes.push({
      id: 'daily-change',
      title: quoteFresh
        ? q.change_pct > 0 ? '今日上涨' : q.change_pct < 0 ? '今日下跌' : '今日平盘'
        : q.change_pct > 0 ? '最近已知上涨' : q.change_pct < 0 ? '最近已知下跌' : '最近已知平盘',
      value: signedPct(q.change_pct),
      detail: `${quoteFresh ? '现价' : '最近已知价'} ${q.price.toFixed(2)} 元，昨收 ${q.prev_close.toFixed(2)} 元。`,
      evidence: `(${q.price.toFixed(2)} - ${q.prev_close.toFixed(2)}) / ${q.prev_close.toFixed(2)}`,
      source: q.source || '行情聚合',
      asOf: q.data_time || 'unknown',
      tone: toneFor(q.change_pct),
    })
  }

  if (input.bars.length >= 2) {
    const recent = input.bars.slice(-20)
    const first = recent[0]
    const last = recent[recent.length - 1]
    if (first.close > 0) {
      const pct = ((last.close - first.close) / first.close) * 100
      changes.push({
        id: 'period-change',
        title: `近 ${recent.length} 个交易日`,
        value: signedPct(pct),
        detail: `收盘价由 ${first.close.toFixed(2)} 元变为 ${last.close.toFixed(2)} 元。`,
        evidence: `前复权日线区间收益，${first.trade_date} 至 ${last.trade_date}`,
        source: '本地日线 / 行情源',
        asOf: last.trade_date,
        tone: toneFor(pct),
      })
    }
  }

  const latestFinance = input.finance?.indicators.at(-1)
  if (latestFinance) {
    changes.push({
      id: 'finance-change',
      title: '最新财务变化',
      value: `营收 ${signedPct(latestFinance.revenue_yoy)} · 净利 ${signedPct(latestFinance.net_profit_yoy)}`,
      detail: `${latestFinance.report_name}累计口径，财报披露存在滞后。`,
      evidence: `ROE ${latestFinance.roe.toFixed(2)}%，毛利率 ${latestFinance.gross_margin.toFixed(2)}%`,
      source: '东财 F10',
      asOf: latestFinance.report_date,
      tone: toneFor(Math.min(latestFinance.revenue_yoy, latestFinance.net_profit_yoy)),
    })
  }

  if (input.fundflow?.days.length) {
    changes.push({
      id: 'fundflow-change',
      title: '近 5 日主力资金',
      value: `${input.fundflow.main_net_5d_yi > 0 ? '+' : ''}${input.fundflow.main_net_5d_yi.toFixed(2)} 亿元`,
      detail: input.fundflow.streak_days > 0
        ? `连续净流入 ${input.fundflow.streak_days} 天。`
        : input.fundflow.streak_days < 0
          ? `连续净流出 ${-input.fundflow.streak_days} 天。`
          : '当前没有连续流入或流出记录。',
      evidence: '主力净额为超大单与大单净额之和，不代表价格必然方向',
      source: '东财资金流',
      asOf: input.fundflow.last_date || 'unknown',
      tone: toneFor(input.fundflow.main_net_5d_yi),
    })
  }

  if (q?.freshness?.freshness_status !== 'fresh') {
    risks.push({
      id: 'quote-freshness',
      title: q?.freshness?.freshness_status === 'stale' ? '行情已经过期' : '行情时效 unknown',
      value: q?.freshness?.freshness_status || 'unknown',
      detail: q?.freshness?.stale_reason || '当前无法确认报价是否仍然有效。',
      evidence: `数据源时间 ${q?.data_time || 'unknown'}；期望时点 ${q?.freshness?.expected_as_of || 'unknown'}`,
      source: q?.source || '行情聚合',
      asOf: q?.data_time || 'unknown',
      tone: 'warning',
    })
  }

  if (position?.belowStopLoss || position?.nearStopLoss) {
    risks.push({
      id: 'position-stop',
      title: position.belowStopLoss ? '已跌破账本止损价' : '接近账本止损价',
      value: position.stopLoss ? `${position.stopLoss.toFixed(2)} 元` : '已触发',
      detail: '该状态由持仓服务根据当前有效行情和用户账本计划计算。',
      evidence: `平均成本 ${position.averageCost.toFixed(2)} 元；计划止损 ${position.stopLoss?.toFixed(2) || 'unknown'} 元`,
      source: '个人持仓账本 + 有效行情',
      asOf: position.asOf || 'unknown',
      tone: 'warning',
    })
  }

  const nextLift = (input.corpEvents?.lifts || [])
    .filter((item) => timestamp(`${item.free_date}T00:00:00`) >= (input.now || new Date()).getTime())
    .sort((a, b) => a.free_date.localeCompare(b.free_date))[0]
  if (nextLift) {
    risks.push({
      id: 'upcoming-lift',
      title: nextLift.free_ratio >= 10 ? '较大比例解禁临近' : '有限售股解禁安排',
      value: `${nextLift.free_date} · ${nextLift.free_ratio.toFixed(2)}%`,
      detail: '解禁可能增加可流通筹码，但不等同于实际减持。',
      evidence: `${(nextLift.free_shares / 10_000).toFixed(0)} 万股，参考市值 ${(nextLift.lift_market_cap / 100_000_000).toFixed(2)} 亿元`,
      source: '本地事件表 / 东财',
      asOf: nextLift.free_date,
      tone: nextLift.free_ratio >= 10 ? 'warning' : 'neutral',
    })
  }

  if (latestFinance && (latestFinance.net_profit_yoy < 0 || latestFinance.debt_ratio >= 70)) {
    risks.push({
      id: 'finance-risk',
      title: latestFinance.net_profit_yoy < 0 ? '净利润同比下降' : '资产负债率较高',
      value: latestFinance.net_profit_yoy < 0
        ? signedPct(latestFinance.net_profit_yoy)
        : `${latestFinance.debt_ratio.toFixed(2)}%`,
      detail: '这是按财务字段阈值呈现的关注项，不是对公司价值的结论。',
      evidence: `净利同比 ${latestFinance.net_profit_yoy.toFixed(2)}%；资产负债率 ${latestFinance.debt_ratio.toFixed(2)}%`,
      source: '东财 F10',
      asOf: latestFinance.report_date,
      tone: 'warning',
    })
  }

  const amplitude = input.valuation?.amplitude
  if (q && amplitude != null && amplitude >= 7) {
    risks.push({
      id: 'amplitude-risk',
      title: '日内波动较大',
      value: `${amplitude.toFixed(2)}%`,
      detail: '振幅达到 7% 规则阈值，价格波动风险需要额外关注。',
      evidence: `最高 ${q.high.toFixed(2)} 元，最低 ${q.low.toFixed(2)} 元`,
      source: input.valuation?.source || q.source || '行情聚合',
      asOf: input.valuation?.data_time || q.data_time || 'unknown',
      tone: 'warning',
    })
  }

  const eventEvidenceIncomplete = input.eventPartial || phaseUnknown(input.eventPhase)
  if (risks.length < 2 && eventEvidenceIncomplete) {
    risks.push({
      id: 'event-gap',
      title: input.eventPartial ? '事件风险待补全' : '事件风险 unknown',
      value: input.eventPartial
        ? input.eventPhase === 'error'
          ? '公司行动读取失败'
          : input.eventPhase === 'loading' ? '公司行动加载中' : '仅公司行动'
        : input.eventPhase === 'error' ? '读取失败' : input.eventPhase === 'loading' ? '加载中' : '未请求',
      detail: input.eventPartial
        ? input.eventPhase === 'error'
          ? '公司行动读取失败，公告和新闻也尚未加载，当前无法形成完整事件判断。'
          : input.eventPhase === 'loading'
            ? '公司行动正在加载，公告和新闻也尚未加载，当前无法形成完整事件判断。'
          : '首屏只预取公司行动，公告和新闻尚未加载，不能据此判断“近期无事”。'
        : '公告、新闻和公司事件尚未形成完整证据，不能据此判断“近期无事”。',
      evidence: '事件分区尚无可核验结果',
      source: 'unknown',
      asOf: 'unknown',
      tone: 'unknown',
    })
  }
  if (risks.length < 2 && phaseUnknown(input.fundamentalPhase)) {
    risks.push({
      id: 'fundamental-gap',
      title: '基本面风险 unknown',
      value: input.fundamentalPhase === 'error' ? '读取失败' : input.fundamentalPhase === 'loading' ? '加载中' : '未请求',
      detail: '财务与估值尚未形成完整证据，摘要不会代填基本面结论。',
      evidence: '基本面分区尚无可核验结果',
      source: 'unknown',
      asOf: 'unknown',
      tone: 'unknown',
    })
  }

  const invalidation: string[] = []
  if (q?.freshness?.freshness_status !== 'fresh') invalidation.push('行情不是当前有效盘面')
  if (!position?.quoteFresh && position) invalidation.push('持仓盈亏缺少完整有效行情')
  if (input.eventPartial) invalidation.push('事件证据仅预取公司行动，公告与新闻尚未加载')
  else if (phaseUnknown(input.eventPhase)) invalidation.push('事件证据未完成加载')
  if (phaseUnknown(input.fundamentalPhase)) invalidation.push('基本面证据未完成加载')
  if (input.score?.data_limited) invalidation.push('技术评分样本不足')

  return {
    changes: changes.slice(0, 3),
    risks: risks.slice(0, 2),
    recentEvent: selectRecentEvent(input),
    invalidation: invalidation.length
      ? `${invalidation.join('；')}，摘要不足以支持完整判断。`
      : '后续行情、财报或事件更新会改变摘要排序，应回到各页签核对原始证据。',
  }
}
