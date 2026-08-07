<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  NAlert,
  NButton,
  NButtonGroup,
  NDropdown,
  NEmpty,
  NGi,
  NGrid,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  NTooltip,
  type DropdownOption,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getMinuteLine,
  getValuation,
  getScore,
  getIndicators,
  getChips,
  getStockFundFlow,
  getStockLhb,
  type Quote,
  type Bar,
  type MinuteLine,
  type Valuation,
  type StockScore,
  type IndicatorSeries,
  type ChipDist,
  type StockFundFlow,
  type LhbRecord,
} from '@/api/market'
import { getNews, newsSourceLabel, sentimentTag, type NewsItem } from '@/api/news'
import { isAbortError } from '@/api/client'
import { getAnnouncements, type AnnouncementItem } from '@/api/announcement'
import { getStockFinance, type StockFinance } from '@/api/finance'
import { getStockOrgView, type StockOrgView } from '@/api/orgview'
import { getStockCorpEvents, type StockCorpEvents } from '@/api/event'
import { listPositions, type Position } from '@/api/position'
import { listWatchlists, type WatchlistGroup } from '@/api/watchlist'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useStockActions } from '@/composables/useStockActions'
import { isEtfSymbol } from '@/api/etf'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'
import StockCoverageMatrix from '@/components/StockCoverageMatrix.vue'
import { createLoadEpoch, resolveCoverageStatus, type StockCoverageItem } from '@/components/stockCoverage'
import StockDecisionSummary from '@/components/stock-detail/StockDecisionSummary.vue'
import StockTabState from '@/components/stock-detail/StockTabState.vue'
import {
  buildDecisionSummary,
  type PositionRelationSummary,
  type StockSectionPhase,
} from '@/components/stock-detail/decisionSummary'

const route = useRoute()
const { vars, isDark, pctColor } = useUi()
const {
  adding,
  goAnalysis,
  goQa,
  goCompare,
  goAlert,
  goThesis,
  goWatchlist,
  goPosition: openPosition,
  goNote,
  addToWatchlist,
} = useStockActions()

const market = computed(() => String(route.params.market || 'cn'))
const symbol = computed(() => String(route.params.symbol || ''))
// ETF/场内基金无 PE/PB 个股估值指标（腾讯源返回 0 值），估值卡对基金隐藏不适用项。
const isFund = computed(() => market.value === 'cn' && isEtfSymbol(symbol.value))

const quote = ref<Quote | null>(null)
const valuation = ref<Valuation | null>(null)
const score = ref<StockScore | null>(null)
const bars = ref<Bar[]>([])
const minute = ref<MinuteLine | null>(null)
const minuteLoading = ref(false)
const minuteError = ref('')
const chartMode = ref<'minute' | 'daily'>(market.value === 'cn' ? 'minute' : 'daily')
const indicators = ref<IndicatorSeries | null>(null)
const chips = ref<ChipDist | null>(null)
const news = ref<NewsItem[]>([])
const announcements = ref<AnnouncementItem[]>([])
const finance = ref<StockFinance | null>(null)
const fundflow = ref<StockFundFlow | null>(null)
const lhbRecords = ref<LhbRecord[]>([])
const orgview = ref<StockOrgView | null>(null)
// B9 解禁 / 分红：null=尚未取到（加载中或失败）——与「取到了但没有解禁」严格区分，
// 后者是 corpEvents 非 null 且 lifts 为空数组。
const corpEvents = ref<StockCorpEvents | null>(null)
const corpEventsError = ref('')
const positions = ref<Position[]>([])
const watchlistGroups = ref<WatchlistGroup[]>([])
const positionsKnown = ref(false)
const watchlistKnown = ref(false)
const relationshipAsOf = ref('')

// 情绪标签（N2）：利好/利空才渲染，颜色随涨跌色主题。
function sentiView(n: NewsItem): { text: string; color: string } | null {
  const t = sentimentTag(n)
  return t ? { text: t.text, color: pctColor(t.dir) } : null
}
type TabKey = 'trend' | 'event' | 'fundamental' | 'research'
interface SectionState {
  phase: StockSectionPhase
  error: string
  updatedAt: string
}

const activeTab = ref<TabKey>('trend')
const quoteState = reactive<SectionState>({ phase: 'idle', error: '', updatedAt: '' })
const relationshipState = reactive<SectionState>({ phase: 'idle', error: '', updatedAt: '' })
const tabStates = reactive<Record<TabKey, SectionState>>({
  trend: { phase: 'idle', error: '', updatedAt: '' },
  event: { phase: 'idle', error: '', updatedAt: '' },
  fundamental: { phase: 'idle', error: '', updatedAt: '' },
  research: { phase: 'idle', error: '', updatedAt: '' },
})
const eventPreviewState = reactive<SectionState>({ phase: 'idle', error: '', updatedAt: '' })

type CoverageKey = 'quote' | 'daily' | 'finance' | 'news' | 'announcements' | 'institutions' | 'funds' | 'events'
const coverageKeys: CoverageKey[] = ['quote', 'daily', 'finance', 'news', 'announcements', 'institutions', 'funds', 'events']
const coverageObserved = reactive<Record<CoverageKey, boolean>>({
  quote: false, daily: false, finance: false, news: false,
  announcements: false, institutions: false, funds: false, events: false,
})
const coverageErrors = reactive<Record<CoverageKey, string>>({
  quote: '', daily: '', finance: '', news: '',
  announcements: '', institutions: '', funds: '', events: '',
})
function resetCoverageState() {
  for (const key of coverageKeys) {
    coverageObserved[key] = false
    coverageErrors[key] = ''
  }
}
function markCoverage(key: CoverageKey, error?: unknown) {
  coverageObserved[key] = true
  coverageErrors[key] = error
    ? (error instanceof Error ? error.message : String(error)) || '读取失败'
    : ''
}

const stockRef = computed(() => ({
  symbol: symbol.value,
  market: market.value,
  name: quote.value?.name || symbol.value,
}))

function sameStock(item: { symbol: string; market?: string }) {
  return item.symbol.toLowerCase() === symbol.value.toLowerCase()
    && (item.market || 'cn').toLowerCase() === market.value.toLowerCase()
}

const inWatchlist = computed(() => watchlistGroups.value.some((group) => group.items.some(sameStock)))
const matchingPositions = computed(() => positions.value.filter((item) => item.status === 'holding' && item.quantity > 0 && sameStock(item)))
const positionSummary = computed<PositionRelationSummary | null>(() => {
  const items = matchingPositions.value
  if (!items.length) return null
  const quantity = items.reduce((sum, item) => sum + item.quantity, 0)
  const remainingCost = items.reduce((sum, item) => {
    if (item.remaining_cost > 0) return sum + item.remaining_cost
    if (item.cost > 0) return sum + item.cost
    return sum + item.buy_price * item.quantity
  }, 0)
  const quoteFresh = items.every((item) => item.quote_ok)
  const profitAmount = quoteFresh ? items.reduce((sum, item) => sum + item.profit_amount, 0) : null
  const triggered = items.find((item) => item.below_stop_loss || item.near_stop_loss)
  return {
    lots: items.length,
    quantity,
    averageCost: quantity > 0 ? remainingCost / quantity : 0,
    remainingCost,
    profitAmount,
    profitPct: profitAmount != null && remainingCost > 0 ? (profitAmount / remainingCost) * 100 : null,
    realizedPnl: items.reduce((sum, item) => sum + item.realized_pnl, 0),
    quoteFresh,
    asOf: latestAsOf(items.map((item) => item.quote_as_of)),
    belowStopLoss: items.some((item) => item.below_stop_loss),
    nearStopLoss: items.some((item) => item.near_stop_loss),
    stopLoss: triggered?.plan_stop_loss || undefined,
  }
})

const eventSummaryPhase = computed<StockSectionPhase>(() => {
  const state = tabStates.event
  const phase = state.phase === 'idle' ? eventPreviewState.phase : state.phase
  const sourceUnavailable = !!corpEvents.value
    && (corpEvents.value.lift_unavailable || corpEvents.value.action_unavailable)
  if ((phase === 'ready' || phase === 'empty') && (state.error || sourceUnavailable)) return 'error'
  return phase
})
const eventSummaryPartial = computed(() => (
  tabStates.event.phase === 'idle' && eventPreviewState.phase !== 'idle'
))

const decisionSummary = computed(() => buildDecisionSummary({
  quote: quote.value,
  position: positionSummary.value,
  bars: bars.value,
  valuation: valuation.value,
  score: score.value,
  fundflow: fundflow.value,
  finance: finance.value,
  corpEvents: corpEvents.value,
  announcements: announcements.value,
  news: news.value,
  eventPhase: eventSummaryPhase.value,
  eventPartial: eventSummaryPartial.value,
  fundamentalPhase: tabStates.fundamental.phase,
}))

const tabDataState = computed(() => ({
  trend: tabHasData('trend'),
  event: tabHasData('event'),
  fundamental: tabHasData('fundamental'),
  research: tabHasData('research'),
}))
const pageRefreshing = computed(() => {
  const phases = [quoteState.phase, relationshipState.phase, tabStates[activeTab.value].phase]
  if (tabStates.event.phase === 'idle') phases.push(eventPreviewState.phase)
  return phases.some((phase) => phase === 'loading' || phase === 'refreshing')
})
const moreOptions = computed<DropdownOption[]>(() => [
  { key: 'qa', label: '个股问答' },
  { key: 'compare', label: '横向对比' },
  { key: 'thesis', label: '逻辑卡' },
  { key: 'note', label: '添加笔记' },
  { key: 'position', label: positionSummary.value ? '持仓管理' : '记录持仓' },
])

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const chipEl = ref<HTMLDivElement | null>(null)
let chipChart: echarts.ECharts | null = null
const chipTrendEl = ref<HTMLDivElement | null>(null)
let chipTrendChart: echarts.ECharts | null = null
const finEl = ref<HTMLDivElement | null>(null)
let finChart: echarts.ECharts | null = null
const ffEl = ref<HTMLDivElement | null>(null)
let ffChart: echarts.ECharts | null = null

// 标的 epoch 与分区 request epoch 双重守卫：切股使全部旧请求失效，同一分区连续重试时后发覆盖前发。
const stockEpoch = createLoadEpoch()
const quoteEpoch = createLoadEpoch()
const relationshipEpoch = createLoadEpoch()
const eventPreviewEpoch = createLoadEpoch()
const tabEpochs: Record<TabKey, ReturnType<typeof createLoadEpoch>> = {
  trend: createLoadEpoch(),
  event: createLoadEpoch(),
  fundamental: createLoadEpoch(),
  research: createLoadEpoch(),
}
let currentStockToken = 0
let stockAbort: AbortController | null = null

// disposeCharts 销毁全部 ECharts 实例。切标的时容器会随 v-if/v-show 变化重建，
// 旧实例继续绑在被移除的 DOM 上（renderChipCharts 等在数据为 null 时直接 return
// 不会 dispose），既泄漏又会让 onResize 对着孤儿实例 resize。
function disposeCharts() {
  chart?.dispose()
  chart = null
  chipChart?.dispose()
  chipChart = null
  chipTrendChart?.dispose()
  chipTrendChart = null
  finChart?.dispose()
  finChart = null
  ffChart?.dispose()
  ffChart = null
}

function errorMessage(error: unknown) {
  return (error instanceof Error ? error.message : String(error)) || '读取失败'
}

function observedAt() {
  return new Date().toLocaleString('sv-SE', { hour12: false })
}

function stockRequestIsCurrent(token: number, requestMarket: string, requestSymbol: string) {
  return stockEpoch.isCurrent(token) && market.value === requestMarket && symbol.value === requestSymbol
}

function corpEventsAvailabilityIssue(result: StockCorpEvents) {
  if (!result.lift_unavailable && !result.action_unavailable) return ''
  if (result.note) return result.note
  if (result.lift_unavailable && result.action_unavailable) return '解禁与分红数据本次均不可用'
  return result.lift_unavailable ? '解禁数据本次不可用' : '分红数据本次不可用'
}

// 决策摘要只预取本地公司行动；公告与新闻仍保持事件页签按需加载。
async function loadEventPreview(stockToken = currentStockToken) {
  if (!symbol.value || market.value !== 'cn' || isEtfSymbol(symbol.value)) return
  const requestToken = eventPreviewEpoch.next()
  const requestMarket = market.value
  const requestSymbol = symbol.value
  eventPreviewState.phase = corpEvents.value ? 'refreshing' : 'loading'
  eventPreviewState.error = ''
  corpEventsError.value = ''
  try {
    const result = await getStockCorpEvents(requestMarket, requestSymbol, stockAbort?.signal)
    if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !eventPreviewEpoch.isCurrent(requestToken)) return
    corpEvents.value = result
    corpEventsError.value = ''
    const issue = corpEventsAvailabilityIssue(result)
    markCoverage('events', issue || undefined)
    eventPreviewState.error = issue
    eventPreviewState.phase = issue ? 'error' : 'ready'
    eventPreviewState.updatedAt = observedAt()
  } catch (error) {
    if (
      isAbortError(error)
      || !stockRequestIsCurrent(stockToken, requestMarket, requestSymbol)
      || !eventPreviewEpoch.isCurrent(requestToken)
    ) return
    const message = errorMessage(error)
    corpEventsError.value = message
    markCoverage('events', error)
    eventPreviewState.error = message
    eventPreviewState.phase = 'error'
    eventPreviewState.updatedAt = observedAt()
  }
}

async function loadQuote(silent = false, stockToken = currentStockToken) {
  if (!symbol.value) return
  const requestToken = quoteEpoch.next()
  const requestMarket = market.value
  const requestSymbol = symbol.value
  quoteState.phase = silent && quote.value ? 'refreshing' : 'loading'
  quoteState.error = ''
  try {
    const result = await getQuote(requestMarket, requestSymbol)
    if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !quoteEpoch.isCurrent(requestToken)) return
    quote.value = result
    quoteState.phase = 'ready'
    quoteState.updatedAt = observedAt()
    markCoverage('quote')
  } catch (error) {
    if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !quoteEpoch.isCurrent(requestToken)) return
    quoteState.error = errorMessage(error)
    quoteState.phase = quote.value ? 'ready' : 'error'
    markCoverage('quote', error)
  }
}

async function loadRelationships(silent = false, stockToken = currentStockToken) {
  if (!symbol.value) return
  const requestToken = relationshipEpoch.next()
  const requestMarket = market.value
  const requestSymbol = symbol.value
  relationshipState.phase = silent && (positionsKnown.value || watchlistKnown.value) ? 'refreshing' : 'loading'
  relationshipState.error = ''
  const [positionResult, watchlistResult] = await Promise.allSettled([
    listPositions('holding'),
    listWatchlists(),
  ])
  if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !relationshipEpoch.isCurrent(requestToken)) return

  const errors: string[] = []
  if (positionResult.status === 'fulfilled') {
    positions.value = positionResult.value
    positionsKnown.value = true
  } else {
    errors.push(`持仓：${errorMessage(positionResult.reason)}`)
  }
  if (watchlistResult.status === 'fulfilled') {
    watchlistGroups.value = watchlistResult.value
    watchlistKnown.value = true
  } else {
    errors.push(`自选：${errorMessage(watchlistResult.reason)}`)
  }
  relationshipAsOf.value = observedAt()
  relationshipState.updatedAt = relationshipAsOf.value
  relationshipState.error = errors.join('；')
  relationshipState.phase = positionsKnown.value || watchlistKnown.value ? 'ready' : 'error'
}

interface RequestOutcome {
  ok: boolean
  error?: string
  aborted?: boolean
}

async function runTabRequest<T>(
  promise: Promise<T>,
  apply: (value: T) => void,
  stockToken: number,
  requestToken: number,
  tab: TabKey,
  requestMarket: string,
  requestSymbol: string,
  coverageKey?: CoverageKey,
): Promise<RequestOutcome> {
  try {
    const result = await promise
    if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !tabEpochs[tab].isCurrent(requestToken)) {
      return { ok: false, aborted: true }
    }
    apply(result)
    if (coverageKey) markCoverage(coverageKey)
    return { ok: true }
  } catch (error) {
    if (
      isAbortError(error)
      || !stockRequestIsCurrent(stockToken, requestMarket, requestSymbol)
      || !tabEpochs[tab].isCurrent(requestToken)
    ) return { ok: false, aborted: true }
    if (coverageKey) markCoverage(coverageKey, error)
    return { ok: false, error: errorMessage(error) }
  }
}

function tabHasData(tab: TabKey) {
  if (tab === 'trend') return !!minute.value || bars.value.length > 0 || !!chips.value || !!fundflow.value || lhbRecords.value.length > 0
  if (tab === 'event') return !!corpEvents.value || announcements.value.length > 0 || news.value.length > 0
  if (tab === 'fundamental') return !!valuation.value || !!finance.value || !!score.value
  return !!orgview.value || coverageKeys.some((key) => coverageObserved[key])
}

async function loadTab(tab: TabKey, force = false, stockToken = currentStockToken) {
  const state = tabStates[tab]
  if (!force && state.phase !== 'idle' && state.phase !== 'error') return
  if (!symbol.value) return
  const requestToken = tabEpochs[tab].next()
  const requestMarket = market.value
  const requestSymbol = symbol.value
  const requestIsFund = requestMarket === 'cn' && isEtfSymbol(requestSymbol)
  state.phase = tabHasData(tab) ? 'refreshing' : 'loading'
  state.error = ''

  const requests: Array<Promise<RequestOutcome>> = []
  if (tab === 'trend') {
    minuteLoading.value = requestMarket === 'cn'
    minuteError.value = ''
    requests.push(runTabRequest(
      getDailyBars(requestMarket, requestSymbol, 120),
      (result) => { bars.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol, 'daily',
    ))
    requests.push(runTabRequest(
      getIndicators(requestMarket, requestSymbol, 120),
      (result) => { indicators.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol,
    ))
    requests.push(runTabRequest(
      getChips(requestMarket, requestSymbol),
      (result) => { chips.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol,
    ))
    if (requestMarket === 'cn') {
      requests.push(runTabRequest(
        getMinuteLine(requestMarket, requestSymbol, stockAbort?.signal),
        (result) => { minute.value = result },
        stockToken, requestToken, tab, requestMarket, requestSymbol,
      ).then((outcome) => {
        if (outcome.error) minuteError.value = outcome.error
        return outcome
      }))
    } else {
      minute.value = null
      chartMode.value = 'daily'
    }
    if (requestMarket === 'cn' && !requestIsFund) {
      requests.push(runTabRequest(
        getStockFundFlow(requestMarket, requestSymbol, 90),
        (result) => { fundflow.value = result },
        stockToken, requestToken, tab, requestMarket, requestSymbol, 'funds',
      ))
      requests.push(runTabRequest(
        getStockLhb(requestMarket, requestSymbol, 10),
        (result) => { lhbRecords.value = result },
        stockToken, requestToken, tab, requestMarket, requestSymbol,
      ))
    }
  } else if (tab === 'event') {
    // 用户进入完整事件页签后由 tab epoch 接管；仍在飞行的首屏预取结果不得迟到覆盖。
    eventPreviewEpoch.invalidate()
    requests.push(runTabRequest(
      getNews({ symbol: requestSymbol, limit: 15 }),
      (result) => { news.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol, 'news',
    ))
    requests.push(runTabRequest(
      getAnnouncements(requestSymbol, 15),
      (result) => { announcements.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol, 'announcements',
    ))
    if (requestMarket === 'cn' && !requestIsFund && (force || !corpEvents.value)) {
      corpEventsError.value = ''
      requests.push(runTabRequest(
        getStockCorpEvents(requestMarket, requestSymbol, stockAbort?.signal),
        (result) => {
          corpEvents.value = result
          corpEventsError.value = ''
          const issue = corpEventsAvailabilityIssue(result)
          markCoverage('events', issue || undefined)
        },
        stockToken, requestToken, tab, requestMarket, requestSymbol,
      ).then((outcome) => {
        if (outcome.error) corpEventsError.value = outcome.error
        return outcome
      }))
    }
  } else if (tab === 'fundamental') {
    requests.push(runTabRequest(
      getValuation(requestMarket, requestSymbol),
      (result) => { valuation.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol,
    ))
    requests.push(runTabRequest(
      getScore(requestMarket, requestSymbol),
      (result) => { score.value = result },
      stockToken, requestToken, tab, requestMarket, requestSymbol,
    ))
    if (requestMarket === 'cn' && !requestIsFund) {
      requests.push(runTabRequest(
        getStockFinance(requestMarket, requestSymbol),
        (result) => { finance.value = result },
        stockToken, requestToken, tab, requestMarket, requestSymbol, 'finance',
      ))
    }
  } else {
    if (requestMarket === 'cn' && !requestIsFund) {
      requests.push(runTabRequest(
        getStockOrgView(requestMarket, requestSymbol, quote.value?.price),
        (result) => { orgview.value = result },
        stockToken, requestToken, tab, requestMarket, requestSymbol, 'institutions',
      ))
    }
  }

  const outcomes = await Promise.all(requests)
  if (!stockRequestIsCurrent(stockToken, requestMarket, requestSymbol) || !tabEpochs[tab].isCurrent(requestToken)) return
  minuteLoading.value = false
  const errors = outcomes.flatMap((outcome) => outcome.error ? [outcome.error] : [])
  const completed = outcomes.filter((outcome) => outcome.ok).length
  state.error = errors.join('；')
  state.updatedAt = observedAt()
  state.phase = completed === 0 && errors.length
    ? 'error'
    : tabHasData(tab) ? 'ready' : 'empty'
  await nextTick()
  if (tab === 'trend') {
    renderChart()
    renderChipCharts()
    renderFundFlowChart()
  } else if (tab === 'fundamental') {
    renderFinanceChart()
  }
}

// alignByDate 把指标序列按交易日对齐到 K 线（两次独立请求可能相差末根，按日期匹配防画歪）。
function alignByDate(vals: (number | null)[]): (number | null)[] {
  const ind = indicators.value
  if (!ind) return []
  const m = new Map<string, number | null>()
  ind.dates.forEach((d, i) => m.set(d, vals[i] ?? null))
  return bars.value.map((b) => m.get(b.trade_date) ?? null)
}

function renderChart() {
  if (chartMode.value === 'minute') {
    renderMinuteChart()
    return
  }
  if (!chartEl.value || !bars.value.length) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const down = vars.value.successColor
  const dates = bars.value.map((b) => b.trade_date)
  const kline = {
    type: 'candlestick' as const,
    name: '日K',
    data: bars.value.map((b) => [b.open, b.close, b.low, b.high]),
    itemStyle: { color: up, color0: down, borderColor: up, borderColor0: down },
  }

  // 指标未就绪/失败：退回单图 K 线。
  if (!indicators.value) {
    chart.setOption({
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
      grid: { left: 52, right: 16, top: 16, bottom: 36 },
      xAxis: { type: 'category', data: dates, boundaryGap: false },
      yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
      series: [kline],
    })
    return
  }

  // 主图 K 线 + BOLL(20,2σ) 叠加，副图 MACD(12,26,9)（柱=2×(DIF−DEA) A 股口径）。
  const bollColor = vars.value.warningColor
  const midColor = vars.value.primaryColor
  const difColor = vars.value.primaryColor
  const deaColor = vars.value.warningColor
  const line = (name: string, data: (number | null)[], color: string, opacity = 1, extra: object = {}) => ({
    type: 'line' as const,
    name,
    data,
    symbol: 'none',
    lineStyle: { width: 1, color, opacity },
    itemStyle: { color },
    emphasis: { disabled: true },
    ...extra,
  })
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    legend: {
      top: 0,
      type: 'scroll',
      data: ['上轨', '中轨', '下轨', 'DIF', 'DEA', 'MACD'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: [
      { left: 52, right: 16, top: 26, height: '58%' },
      { left: 52, right: 16, top: '76%', height: '18%' },
    ],
    xAxis: [
      { type: 'category', data: dates, boundaryGap: false },
      {
        type: 'category',
        gridIndex: 1,
        data: dates,
        boundaryGap: false,
        axisLabel: { show: false },
        axisTick: { show: false },
      },
    ],
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
      { type: 'value', gridIndex: 1, scale: true, splitNumber: 2, splitLine: { show: false } },
    ],
    series: [
      kline,
      line('上轨', alignByDate(indicators.value.boll_up), bollColor, 0.65),
      line('中轨', alignByDate(indicators.value.boll_mid), midColor, 0.85),
      line('下轨', alignByDate(indicators.value.boll_low), bollColor, 0.65),
      {
        type: 'bar',
        name: 'MACD',
        xAxisIndex: 1,
        yAxisIndex: 1,
        data: alignByDate(indicators.value.hist),
        itemStyle: {
          color: (p: { value: number | null }) => ((p.value ?? 0) >= 0 ? up : down),
        },
        barWidth: '60%',
      },
      line('DIF', alignByDate(indicators.value.dif), difColor, 1, { xAxisIndex: 1, yAxisIndex: 1 }),
      line('DEA', alignByDate(indicators.value.dea), deaColor, 1, { xAxisIndex: 1, yAxisIndex: 1 }),
    ],
  })
}

function renderMinuteChart() {
  const line = minute.value
  if (!chartEl.value || !line?.points.length) {
    chart?.dispose()
    chart = null
    return
  }
  chart?.dispose()
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  const prices = line.points.map((p) => p.price)
  const avgs = line.points.map((p) => p.avg)
  const volumes = line.points.map((p) => p.volume)
  const times = line.points.map((p) => p.time)
  const priceColor = pctColor(line.last - line.prev_close)
  const avgColor = vars.value.warningColor
  const baseColor = vars.value.textColor3
  const maxDeviation = Math.max(
    Math.abs(line.high - line.prev_close),
    Math.abs(line.low - line.prev_close),
    line.prev_close * 0.002,
  )

  chart.setOption({
    backgroundColor: 'transparent',
    animationDuration: 300,
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'cross' },
      confine: true,
      formatter: (params: { seriesName: string; value: number; axisValue: string }[]) => {
        if (!params.length) return ''
        const i = times.indexOf(params[0].axisValue)
        const p = line.points[i]
        if (!p) return ''
        const change = line.prev_close ? ((p.price / line.prev_close - 1) * 100).toFixed(2) : '-'
        return `${line.trade_date} ${p.time}<br/>价格 ${p.price.toFixed(2)}（${Number(change) > 0 ? '+' : ''}${change}%）<br/>估算均价 ${p.avg.toFixed(3)}<br/>成交量 ${fmtVol(p.volume)}`
      },
    },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    legend: {
      top: 0,
      data: ['价格', '估算均价'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 16,
      itemHeight: 8,
    },
    grid: [
      { left: 54, right: 18, top: 30, height: '62%' },
      { left: 54, right: 18, top: '76%', height: '17%' },
    ],
    xAxis: [
      {
        type: 'category',
        data: times,
        boundaryGap: false,
        axisLabel: { interval: Math.max(1, Math.floor(times.length / 5)), fontSize: 10 },
      },
      {
        type: 'category',
        gridIndex: 1,
        data: times,
        boundaryGap: true,
        axisLabel: { show: false },
        axisTick: { show: false },
      },
    ],
    yAxis: [
      {
        type: 'value',
        min: Number((line.prev_close - maxDeviation).toFixed(3)),
        max: Number((line.prev_close + maxDeviation).toFixed(3)),
        scale: true,
        axisLabel: { formatter: (v: number) => v.toFixed(2) },
        splitLine: { lineStyle: { color: vars.value.dividerColor } },
      },
      {
        type: 'value',
        gridIndex: 1,
        splitNumber: 2,
        axisLabel: { formatter: (v: number) => (v >= 10000 ? `${(v / 10000).toFixed(0)}万` : String(v)) },
        splitLine: { show: false },
      },
    ],
    series: [
      {
        type: 'line',
        name: '价格',
        data: prices,
        symbol: 'none',
        lineStyle: { width: 1.5, color: priceColor },
        itemStyle: { color: priceColor },
        markLine: {
          symbol: 'none',
          silent: true,
          label: { formatter: line.base_from_open ? '开盘基准' : '昨收', color: baseColor },
          lineStyle: { type: 'dashed', color: baseColor, opacity: 0.75 },
          data: [{ yAxis: line.prev_close }],
        },
      },
      {
        type: 'line',
        name: '估算均价',
        data: avgs,
        symbol: 'none',
        lineStyle: { width: 1.2, color: avgColor },
        itemStyle: { color: avgColor },
      },
      {
        type: 'bar',
        name: '成交量',
        xAxisIndex: 1,
        yAxisIndex: 1,
        data: volumes,
        barWidth: '70%',
        itemStyle: {
          color: (p: { dataIndex: number }) => {
            const prev = p.dataIndex > 0 ? prices[p.dataIndex - 1] : line.prev_close
            return pctColor(prices[p.dataIndex] - prev)
          },
          opacity: 0.75,
        },
      },
    ],
  })
}

// 筹码峰：横向分布（获利/套牢按现价分色）+ 获利比例近 90 日趋势迷你图。
function renderChipCharts() {
  const c = chips.value
  if (!c) return
  const up = vars.value.errorColor
  const down = vars.value.successColor
  if (chipEl.value) {
    chipChart?.dispose()
    chipChart = echarts.init(chipEl.value, isDark.value ? 'dark' : undefined)
    const profit: (number | null)[] = []
    const trapped: (number | null)[] = []
    c.prices.forEach((p, i) => {
      const v = Math.round(c.chips[i] * 10000) / 100 // 占比 %
      profit.push(p <= c.last_close ? v : null)
      trapped.push(p > c.last_close ? v : null)
    })
    chipChart.setOption({
      backgroundColor: 'transparent',
      tooltip: {
        trigger: 'axis',
        confine: true,
        formatter: (ps: { axisValue: string; value: number | null }[]) => {
          const row = ps.find((x) => x.value != null)
          return row ? `价位 ${row.axisValue}<br/>筹码占比 ${row.value}%` : ''
        },
      },
      grid: { left: 56, right: 12, top: 8, bottom: 22 },
      xAxis: {
        type: 'value',
        axisLabel: { formatter: '{value}%', fontSize: 10 },
        splitLine: { lineStyle: { opacity: 0.3 } },
      },
      yAxis: {
        type: 'category',
        data: c.prices.map((p) => p.toFixed(2)),
        axisLabel: { interval: 29, fontSize: 10 },
        axisTick: { show: false },
      },
      series: [
        { type: 'bar', name: '获利', stack: 'chip', data: profit, barCategoryGap: '0%', itemStyle: { color: up, opacity: 0.85 } },
        { type: 'bar', name: '套牢', stack: 'chip', data: trapped, barCategoryGap: '0%', itemStyle: { color: down, opacity: 0.85 } },
      ],
    })
  }
  if (chipTrendEl.value) {
    chipTrendChart?.dispose()
    chipTrendChart = echarts.init(chipTrendEl.value, isDark.value ? 'dark' : undefined)
    chipTrendChart.setOption({
      backgroundColor: 'transparent',
      tooltip: {
        trigger: 'axis',
        confine: true,
        formatter: (ps: { axisValue: string; value: number }[]) =>
          ps.length ? `${ps[0].axisValue}<br/>获利比例 ${ps[0].value}%` : '',
      },
      grid: { left: 4, right: 4, top: 4, bottom: 4 },
      xAxis: { type: 'category', data: c.days.map((d) => d.date), show: false },
      yAxis: { type: 'value', min: 0, max: 100, show: false },
      series: [
        {
          type: 'line',
          data: c.days.map((d) => d.profit),
          symbol: 'none',
          lineStyle: { width: 1.5, color: vars.value.primaryColor },
          areaStyle: { color: withAlpha(vars.value.primaryColor, 0.12) },
        },
      ],
    })
  }
}

// 财务摘要图（F2）：近 8 期营收/净利柱（左轴，亿元）+ ROE/毛利率线（右轴，%）。
function renderFinanceChart() {
  const inds = finance.value?.indicators
  if (!finEl.value || !inds?.length) return
  finChart?.dispose()
  finChart = echarts.init(finEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const primary = vars.value.primaryColor
  const warn = vars.value.warningColor
  const labels = inds.map((r) => r.report_name || r.report_date)
  finChart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', confine: true },
    legend: {
      top: 0,
      // 窄屏四项一行放不下，scroll 防换行压住柱顶（K 线 legend 同款）
      type: 'scroll',
      data: ['营收(亿)', '净利(亿)', 'ROE%', '毛利率%'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 56, right: 48, top: 30, bottom: 28 },
    xAxis: { type: 'category', data: labels, axisLabel: { fontSize: 10, interval: 0, rotate: labels.length > 5 ? 30 : 0 } },
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.3 } }, axisLabel: { fontSize: 10 } },
      { type: 'value', scale: true, splitLine: { show: false }, axisLabel: { formatter: '{value}%', fontSize: 10 } },
    ],
    series: [
      { type: 'bar', name: '营收(亿)', data: inds.map((r) => Math.round(r.revenue / 1e6) / 100), itemStyle: { color: withAlpha(primary, 0.75) }, barMaxWidth: 22 },
      { type: 'bar', name: '净利(亿)', data: inds.map((r) => Math.round(r.net_profit / 1e6) / 100), itemStyle: { color: withAlpha(up, 0.75) }, barMaxWidth: 22 },
      { type: 'line', name: 'ROE%', yAxisIndex: 1, data: inds.map((r) => r.roe), symbolSize: 5, lineStyle: { width: 2, color: warn }, itemStyle: { color: warn } },
      { type: 'line', name: '毛利率%', yAxisIndex: 1, data: inds.map((r) => r.gross_margin), symbolSize: 5, lineStyle: { width: 2, type: 'dashed', color: vars.value.infoColor }, itemStyle: { color: vars.value.infoColor } },
    ],
  })
}

const finLatest = computed(() => {
  const inds = finance.value?.indicators
  return inds?.length ? inds[inds.length - 1] : null
})

// 主力资金图（M3a）：逐日主力净额柱（红入绿出，亿元）+ 累计净额线（右轴）。
function renderFundFlowChart() {
  const ff = fundflow.value
  if (!ffEl.value || !ff?.days.length) return
  ffChart?.dispose()
  ffChart = echarts.init(ffEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const down = vars.value.successColor
  let acc = 0
  const cum = ff.days.map((d) => {
    acc += d.main_net_yi
    return Math.round(acc * 100) / 100
  })
  ffChart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      trigger: 'axis',
      confine: true,
      formatter: (ps: { axisValue: string; seriesName: string; value: number }[]) =>
        ps.length
          ? `${ps[0].axisValue}<br/>` + ps.map((p) => `${p.seriesName} ${p.value} 亿`).join('<br/>')
          : '',
    },
    legend: {
      top: 0,
      data: ['主力净额(亿)', '区间累计(亿)'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 52, right: 52, top: 28, bottom: 24 },
    xAxis: { type: 'category', data: ff.days.map((d) => d.date), axisLabel: { fontSize: 10 } },
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.3 } }, axisLabel: { fontSize: 10 } },
      { type: 'value', scale: true, splitLine: { show: false }, axisLabel: { fontSize: 10 } },
    ],
    series: [
      {
        type: 'bar',
        name: '主力净额(亿)',
        data: ff.days.map((d) => d.main_net_yi),
        itemStyle: { color: (p: { value: number }) => (p.value >= 0 ? up : down) },
        barMaxWidth: 8,
      },
      {
        type: 'line',
        name: '区间累计(亿)',
        yAxisIndex: 1,
        data: cum,
        symbol: 'none',
        lineStyle: { width: 1.5, color: vars.value.primaryColor },
      },
    ],
  })
}

/* 龙虎榜展示辅助 */
function fmtNetYi(n: number) {
  return (n / 1e8).toFixed(2)
}

/* B9 解禁 / 分红展示辅助 */
// 解禁按解禁日拆「未来」与「已过去」：未来的是决策变量，已过去的是背景（刚解禁完的抛压）。
const todayStr = computed(() => new Date().toLocaleDateString('sv-SE'))
const upcomingLifts = computed(() => (corpEvents.value?.lifts || []).filter((l) => l.free_date >= todayStr.value))
const pastLifts = computed(() => (corpEvents.value?.lifts || []).filter((l) => l.free_date < todayStr.value))
// C10 当前股息率（后端 pickLatestDividendYield 统一挑选，前端不再自己从 actions 里挑，
// 避免和 AI 快照/选股因子算出不同的数字）。undefined = 无数据，估值卡该项整项缺席。
const dividendYield = computed(() => corpEvents.value?.dividend_yield)
function fmtWanShares(n: number) {
  return (n / 1e4).toFixed(0)
}
function liftDaysLeft(date: string) {
  const d = Math.round((new Date(date + 'T00:00:00').getTime() - new Date(todayStr.value + 'T00:00:00').getTime()) / 86400000)
  if (d === 0) return '今天'
  if (d < 0) return `${-d} 天前`
  return `${d} 天后`
}
// 占流通股比例 ≥10% 视为显著抛压（与后端风险闸门 riskLiftRatioWarn 同阈值）。
// 用 pctColor(-1) 取「下跌方向」色：解禁是利空，颜色随 6 主题的涨跌色配置走，
// 不硬编码红绿（涨绿跌红的主题下同样正确）。
function liftRatioColor(ratio: number) {
  return ratio >= 10 ? pctColor(-1) : undefined
}
// 分红方案一句话（每 10 股口径原文优先，缺失时按比例拼）。
function planText(a: { plan_profile: string; bonus_ratio: number; transfer_ratio: number; dividend_pretax: number }) {
  if (a.plan_profile) return a.plan_profile
  const parts: string[] = []
  if (a.bonus_ratio > 0) parts.push(`送 ${a.bonus_ratio}`)
  if (a.transfer_ratio > 0) parts.push(`转 ${a.transfer_ratio}`)
  if (a.dividend_pretax > 0) parts.push(`派 ${a.dividend_pretax} 元`)
  return parts.length ? `每 10 股 ${parts.join(' ')}` : '不分配'
}
function streakText(ff: StockFundFlow) {
  if (ff.streak_days > 0) return `连续净流入 ${ff.streak_days} 天`
  if (ff.streak_days < 0) return `连续净流出 ${-ff.streak_days} 天`
  return '—'
}

/* 机构观点展示辅助（P3a） */
const ovTp = computed(() => orgview.value?.summary?.target_price || null)
const ovSv = computed(() => orgview.value?.summary?.survey || null)
const ovLc = computed(() => orgview.value?.summary?.latest_rating_change || null)

function latestAsOf(values: Array<string | undefined>): string | undefined {
  return values.filter((value): value is string => !!value).sort().at(-1)
}
function isOlderThan(value: string | undefined, days: number): boolean {
  if (!value) return false
  const timestamp = Date.parse(value)
  return Number.isFinite(timestamp) && Date.now() - timestamp > days * 86_400_000
}
function coveragePhase(key: CoverageKey): StockSectionPhase {
  if (key === 'quote') return quoteState.phase
  if (key === 'daily' || key === 'funds') return tabStates.trend.phase
  if (key === 'finance') return tabStates.fundamental.phase
  if (key === 'institutions') return tabStates.research.phase
  return tabStates.event.phase
}
function coverageNote(key: CoverageKey, status: StockCoverageItem['status'], extra = ''): string | undefined {
  if (coverageErrors[key]) return coverageErrors[key]
  if (extra) return extra
  if (status === 'missing') return '读取成功，当前没有该类记录'
  if (status === 'unknown') {
    const phase = coveragePhase(key)
    if (phase === 'idle') return '尚未请求；进入对应页签后按需加载'
    if (phase === 'loading') return '请求进行中，尚不能判断是否缺失'
    if (phase === 'refreshing') return '保留最近已知数据，正在静默刷新'
    if (phase === 'error') return '读取失败，当前状态未知'
    return '本地接口明确返回不可用，当前状态未知'
  }
  if (status === 'stale') return '已有数据早于当前有效时点'
  return undefined
}

const coverageItems = computed<StockCoverageItem[]>(() => {
  const latestBar = latestAsOf(bars.value.map((item) => item.trade_date))
  const expectedBarDate = quote.value?.freshness?.expected_as_of?.slice(0, 10)
  const financeAsOf = latestAsOf([
    ...(finance.value?.indicators || []).map((item) => item.report_date),
    ...(finance.value?.statements || []).map((item) => item.report_date),
  ])
  const newsAsOf = latestAsOf(news.value.map((item) => item.publish_time))
  const announcementAsOf = latestAsOf(announcements.value.map((item) => item.notice_date))
  const institutionAsOf = latestAsOf([
    ...(orgview.value?.reports || []).map((item) => item.report_date),
    ...(orgview.value?.surveys || []).map((item) => item.survey_date),
  ])
  const eventAsOf = latestAsOf([
    ...(corpEvents.value?.lifts || []).map((item) => item.free_date),
    ...(corpEvents.value?.actions || []).map((item) => item.notice_date || item.report_date),
  ])
  const newsSources = Array.from(new Set(news.value.map(newsSourceLabel))).join(' / ')
  const financeAvailable = !!finance.value && (finance.value.indicators.length > 0 || finance.value.statements.length > 0)
  const institutionAvailable = !!orgview.value && (orgview.value.reports.length > 0 || orgview.value.surveys.length > 0)
  const eventAvailable = !!corpEvents.value && (
    corpEvents.value.lifts.length > 0 || corpEvents.value.actions.length > 0 || !!corpEvents.value.dividend_yield
  )
  const eventUnavailable = !!corpEvents.value && (corpEvents.value.lift_unavailable || corpEvents.value.action_unavailable)
  const eventPartial = eventAvailable && eventUnavailable

  const quoteStatus = resolveCoverageStatus({
    observed: coverageObserved.quote,
    available: !!quote.value,
    error: coverageErrors.quote,
    stale: quote.value?.freshness?.freshness_status === 'stale',
  })
  const dailyStatus = resolveCoverageStatus({
    observed: coverageObserved.daily,
    available: bars.value.length > 0,
    error: coverageErrors.daily,
    stale: !!latestBar && !!expectedBarDate && latestBar < expectedBarDate,
  })
  const financeStatus = resolveCoverageStatus({
    observed: coverageObserved.finance,
    available: financeAvailable,
    error: coverageErrors.finance,
    stale: financeAvailable && isOlderThan(financeAsOf, 220),
  })
  const newsStatus = resolveCoverageStatus({
    observed: coverageObserved.news,
    available: news.value.length > 0,
    error: coverageErrors.news,
    stale: news.value.length > 0 && isOlderThan(newsAsOf, 7),
  })
  const announcementStatus = resolveCoverageStatus({
    observed: coverageObserved.announcements,
    available: announcements.value.length > 0,
    error: coverageErrors.announcements,
  })
  const institutionStatus = resolveCoverageStatus({
    observed: coverageObserved.institutions,
    available: institutionAvailable,
    error: coverageErrors.institutions,
  })
  const fundsStatus = resolveCoverageStatus({
    observed: coverageObserved.funds,
    available: !!fundflow.value?.days.length,
    error: coverageErrors.funds,
    stale: !!fundflow.value?.days.length && !fundflow.value.fresh,
  })
  const eventStatus = resolveCoverageStatus({
    observed: coverageObserved.events,
    available: eventAvailable,
    error: coverageErrors.events,
    unknown: eventUnavailable && !eventAvailable,
  })

  return [
    {
      key: 'quote', label: '行情', status: quoteStatus,
      source: quote.value?.source || '行情聚合', asOf: quote.value?.data_time,
      note: coverageNote('quote', quoteStatus, quote.value?.freshness?.stale_reason || ''),
    },
    {
      key: 'daily', label: '日线', status: dailyStatus,
      source: '本地日线 / 行情源', asOf: latestBar,
      note: coverageNote('daily', dailyStatus),
    },
    {
      key: 'finance', label: '财务', status: financeStatus,
      source: '东财 F10', asOf: financeAsOf,
      note: coverageNote('finance', financeStatus),
    },
    {
      key: 'news', label: '新闻', status: newsStatus,
      source: newsSources || '财联社 / 东财', asOf: newsAsOf,
      note: coverageNote('news', newsStatus),
    },
    {
      key: 'announcements', label: '公告', status: announcementStatus,
      source: '东财公告', asOf: announcementAsOf,
      note: coverageNote('announcements', announcementStatus),
    },
    {
      key: 'institutions', label: '机构', status: institutionStatus,
      source: '东财研报 / 调研', asOf: institutionAsOf,
      note: coverageNote('institutions', institutionStatus),
    },
    {
      key: 'funds', label: '资金', status: fundsStatus,
      source: '东财资金流', asOf: fundflow.value?.last_date,
      note: coverageNote('funds', fundsStatus),
    },
    {
      key: 'events', label: '事件', status: eventStatus,
      source: '本地事件表 / 东财', asOf: eventAsOf,
      note: coverageNote(
        'events',
        eventStatus,
        eventPartial ? '部分事件类型不可用，当前结果可能不完整' : (corpEvents.value?.note || ''),
      ),
    },
  ]
})
const ovDistText = computed(() => {
  const d = orgview.value?.summary?.rating_dist_90d
  if (!d || !d.total) return ''
  const names: Array<[string, string]> = [
    ['buy', '买入'],
    ['overweight', '增持'],
    ['neutral', '中性'],
    ['reduce', '减持'],
    ['sell', '卖出'],
    ['other', '其他'],
  ]
  return names
    .filter(([k]) => d[k])
    .map(([k, label]) => `${label} ${d[k]}`)
    .join(' · ')
})
const ovChgText = computed(() => {
  const c = orgview.value?.summary?.rating_changes_90d
  if (!c) return ''
  return `上调 ${c.upgrades} · 下调 ${c.downgrades} · 首次 ${c.first_covers}`
})
function ratingChangeMark(rc: number): { text: string; dir: number } | null {
  if (rc === 0) return { text: '上调', dir: 1 }
  if (rc === 1) return { text: '下调', dir: -1 }
  return null
}
function fmtSignedPct(v: number) {
  return (v > 0 ? '+' : '') + v.toFixed(1)
}

// 主题变化必须整套重绘：6 套主题里明暗只是其一，同为浅色的樱桃红↔天青蓝换主题时
// isDark 不变而 primaryColor/errorColor/dividerColor 全变——只监听 isDark 会让图表
// 一直用上一套主题的色板（§4.1 图表主题感知硬约束）。vars 是 useThemeVars 的 computed，
// 主题 override 换对象即触发。
watch([isDark, vars], () => {
  renderChart()
  renderChipCharts()
  renderFinanceChart()
  renderFundFlowChart()
})
watch(chartMode, async () => {
  await nextTick()
  renderChart()
})
function startStockLoad() {
  currentStockToken = stockEpoch.next()
  quoteEpoch.invalidate()
  relationshipEpoch.invalidate()
  eventPreviewEpoch.invalidate()
  for (const epoch of Object.values(tabEpochs)) epoch.invalidate()
  stockAbort?.abort()
  stockAbort = new AbortController()
  quote.value = null
  bars.value = []
  valuation.value = null
  score.value = null
  minute.value = null
  minuteError.value = ''
  minuteLoading.value = false
  chartMode.value = market.value === 'cn' ? 'minute' : 'daily'
  indicators.value = null
  chips.value = null
  finance.value = null
  fundflow.value = null
  lhbRecords.value = []
  orgview.value = null
  corpEvents.value = null
  corpEventsError.value = ''
  news.value = []
  announcements.value = []
  positions.value = []
  watchlistGroups.value = []
  positionsKnown.value = false
  watchlistKnown.value = false
  relationshipAsOf.value = ''
  quoteState.phase = 'idle'
  quoteState.error = ''
  quoteState.updatedAt = ''
  relationshipState.phase = 'idle'
  relationshipState.error = ''
  relationshipState.updatedAt = ''
  eventPreviewState.phase = 'idle'
  eventPreviewState.error = ''
  eventPreviewState.updatedAt = ''
  for (const state of Object.values(tabStates)) {
    state.phase = 'idle'
    state.error = ''
    state.updatedAt = ''
  }
  activeTab.value = 'trend'
  resetCoverageState()
  disposeCharts()
  void loadQuote(false, currentStockToken)
  void loadRelationships(false, currentStockToken)
  void loadTab('trend', false, currentStockToken)
  void loadEventPreview(currentStockToken)
}

watch([market, symbol], startStockLoad)
watch(activeTab, async (tab) => {
  void loadTab(tab)
  await nextTick()
  if (tab === 'trend') {
    renderChart()
    renderChipCharts()
    renderFundFlowChart()
  } else if (tab === 'fundamental') {
    renderFinanceChart()
  }
})

onMounted(() => {
  startStockLoad()
  window.addEventListener('resize', onResize)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  stockEpoch.invalidate()
  eventPreviewEpoch.invalidate()
  stockAbort?.abort()
  stockAbort = null
  disposeCharts()
})
function onResize() {
  chart?.resize()
  chipChart?.resize()
  chipTrendChart?.resize()
  finChart?.resize()
  ffChart?.resize()
}
useAutoRefresh(() => loadQuote(true), 60_000)

function refreshVisible() {
  void loadQuote(!!quote.value)
  void loadRelationships(positionsKnown.value || watchlistKnown.value)
  void loadTab(activeTab.value, true)
  if (activeTab.value !== 'event' && eventPreviewState.phase !== 'idle') {
    void loadEventPreview()
  }
}

async function handleSummaryAction(action: 'watch' | 'alert' | 'analysis' | 'position') {
  if (action === 'watch') {
    if (!watchlistKnown.value || inWatchlist.value) {
      await goWatchlist(stockRef.value)
      return
    }
    if (await addToWatchlist(stockRef.value)) await loadRelationships(true)
    return
  }
  if (action === 'alert') {
    await goAlert(stockRef.value)
    return
  }
  if (action === 'analysis') {
    await goAnalysis(stockRef.value)
    return
  }
  await openPosition(stockRef.value, positionsKnown.value ? !!positionSummary.value : undefined)
}

async function selectMoreAction(key: string | number) {
  if (key === 'qa') await goQa(stockRef.value)
  else if (key === 'compare') await goCompare(stockRef.value)
  else if (key === 'thesis') await goThesis(stockRef.value)
  else if (key === 'note') await goNote(stockRef.value)
  else if (key === 'position') await handleSummaryAction('position')
}

/* 展示辅助（口径与首页一致：量为手、额为元） */
function fmt(n: number | undefined | null) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtVol(n: number | undefined) {
  if (!n) return '-'
  return n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万手' : n + ' 手'
}
function fmtCap(n: number | undefined) {
  if (!n) return '-'
  return (n / 1e8).toFixed(0) + ' 亿'
}
function fmtNewsTime(t: string) {
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

const scoreDims = computed(() => {
  const s = score.value
  if (!s) return []
  return [
    { label: '趋势', value: s.trend },
    { label: '动量', value: s.momentum },
    { label: '位置', value: s.position },
    { label: '量能', value: s.volume },
    { label: '风险(稳)', value: s.risk },
  ]
})
function scoreType(total: number) {
  if (total >= 60) return 'error' // 偏强用涨色
  if (total < 45) return 'success' // 偏弱用跌色
  return 'default'
}
</script>

<template>
  <PageContainer :title="quote ? `${quote.name} ${symbol}` : `个股详情 ${symbol}`" subtitle="决策摘要 · 分区研究 · 原始证据">
    <template #actions>
      <n-button size="small" quaternary :loading="pageRefreshing" @click="refreshVisible">刷新当前</n-button>
    </template>
    <div class="detail">
      <StockDecisionSummary
        :quote="quote"
        :quote-phase="quoteState.phase"
        :quote-error="quoteState.error"
        :relationship-phase="relationshipState.phase"
        :relationship-error="relationshipState.error"
        :relationship-as-of="relationshipAsOf"
        :watchlist-known="watchlistKnown"
        :position-known="positionsKnown"
        :in-watchlist="inWatchlist"
        :position="positionSummary"
        :summary="decisionSummary"
        :adding="adding"
        @retry-quote="loadQuote(false)"
        @action="handleSummaryAction"
      />

      <n-tabs v-model:value="activeTab" type="line" :animated="false" pane-class="stock-tab-pane">
        <n-tab-pane name="trend" tab="走势" display-directive="show:lazy">
          <StockTabState
            :phase="tabStates.trend.phase"
            :error="tabStates.trend.error"
            :has-data="tabDataState.trend"
            @retry="loadTab('trend', true)"
          >
            <div class="tab-content">
        <!-- 分时与日 K 共用稳定尺寸图表容器，切换时重绘。 -->
        <SectionCard title="行情走势">
          <template #extra>
            <n-button-group v-if="market === 'cn'" size="small">
              <n-button :type="chartMode === 'minute' ? 'primary' : 'default'" @click="chartMode = 'minute'">分时</n-button>
              <n-button :type="chartMode === 'daily' ? 'primary' : 'default'" @click="chartMode = 'daily'">日 K</n-button>
            </n-button-group>
          </template>
          <n-spin :show="chartMode === 'minute' && minuteLoading">
            <div v-if="chartMode === 'minute' && minute" class="minute-meta">
              <span>{{ minute.trade_date }}</span>
              <span>累计 {{ fmtVol(minute.total_volume) }}</span>
              <span>高 {{ fmt(minute.high) }}</span>
              <span>低 {{ fmt(minute.low) }}</span>
              <span>{{ minute.base_from_open ? '昨收缺失，基准线使用开盘价' : '基准线为昨收' }}</span>
            </div>
            <div v-show="chartMode === 'daily' ? bars.length > 0 : !!minute?.points.length" ref="chartEl" class="kchart"></div>
            <div v-if="chartMode === 'minute' && minute" class="src-hint minute-note">{{ minute.avg_note }}</div>
            <n-alert v-if="chartMode === 'minute' && minuteError" type="error" :bordered="false">
              {{ minuteError }}
            </n-alert>
            <n-empty
              v-else-if="chartMode === 'minute' && !minuteLoading && !minute"
              description="分时数据暂不可用"
            />
            <n-empty v-else-if="chartMode === 'daily' && !bars.length" description="日线数据暂不可用" />
            <div v-if="chartMode === 'daily' && bars.length" class="src-hint minute-note">
              近 120 交易日 · BOLL(20,2σ) · MACD(12,26,9)
            </div>
          </n-spin>
        </SectionCard>

        <!-- 筹码分布（T1）：日K+换手率三角衰减本地复算，与东财展示或有复权口径差异 -->
        <SectionCard title="筹码分布">
          <template #extra>
            <span class="src-hint">本地复算 · 前复权口径</span>
          </template>
          <div v-if="chips" class="chip-wrap">
            <div ref="chipEl" class="chip-chart"></div>
            <div class="chip-side">
              <div class="chip-hero">
                <span class="qc-k">获利比例</span>
                <span class="chip-profit qv-figure" :style="{ color: pctColor(chips.profit >= 50 ? 1 : -1) }">
                  {{ chips.profit.toFixed(1) }}%
                </span>
              </div>
              <div class="quote-grid chip-grid">
                <div class="qc"><span class="qc-k">平均成本</span><span class="qc-v qv-tnum">{{ chips.avg_cost.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">现价</span><span class="qc-v qv-tnum">{{ chips.last_close.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">90% 成本区间</span><span class="qc-v qv-tnum">{{ chips.c90_low.toFixed(2) }} ~ {{ chips.c90_high.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">90% 集中度</span><span class="qc-v qv-tnum">{{ chips.conc_90.toFixed(1) }}%</span></div>
                <div class="qc"><span class="qc-k">70% 成本区间</span><span class="qc-v qv-tnum">{{ chips.c70_low.toFixed(2) }} ~ {{ chips.c70_high.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">70% 集中度</span><span class="qc-v qv-tnum">{{ chips.conc_70.toFixed(1) }}%</span></div>
              </div>
              <div class="chip-trend-block">
                <span class="qc-k">获利比例 · 近 {{ chips.days.length }} 交易日</span>
                <div ref="chipTrendEl" class="chip-trend"></div>
              </div>
              <div class="src-hint">
                基于近 {{ chips.bar_count }} 根日K与换手率的三角分布衰减模型本地复算<span v-if="chips.data_limited">（上市时间较短，窗口不足 210 根，精度受限）</span>；仅研究参考。
              </div>
            </div>
          </div>
          <n-empty v-else description="筹码数据暂不可用（需 ≥120 根日线与换手率，A 股标的）" />
        </SectionCard>

        <!-- 主力资金（M3a）：逐日主力净额 + 累计线，汇总格；按需拉取首访可能为空 -->
        <SectionCard v-if="market === 'cn' && !isFund" title="主力资金（近 90 交易日）">
          <template #extra>
            <span class="src-hint">东财资金流 · 主力=超大单+大单</span>
          </template>
          <div v-if="fundflow && fundflow.days.length" class="ff-wrap">
            <div class="quote-grid ff-grid">
              <div class="qc"><span class="qc-k">最新一日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_1d_yi) }">{{ fundflow.main_net_1d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 5 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_5d_yi) }">{{ fundflow.main_net_5d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 10 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_10d_yi) }">{{ fundflow.main_net_10d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 20 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_20d_yi) }">{{ fundflow.main_net_20d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">连续方向</span><span class="qc-v" :style="{ color: pctColor(fundflow.streak_days) }">{{ streakText(fundflow) }}</span></div>
            </div>
            <div ref="ffEl" class="ff-chart"></div>
            <div class="src-hint">
              主力净额=超大单+大单口径（东财），资金流向≠股价必然方向；数据截至 {{ fundflow.last_date }}<span v-if="!fundflow.fresh">（缓存偏旧，稍后自动刷新）</span>；仅研究参考。
            </div>
          </div>
          <n-empty v-else description="暂无资金流数据（东财源，A 股标的；首次访问自动拉取，可稍后刷新）" />
        </SectionCard>

        <!-- 龙虎榜上榜记录（M3a）：本地缓存表（近 30 天回填 + 每日盘后采集） -->
        <SectionCard v-if="market === 'cn' && !isFund" title="龙虎榜上榜记录">
          <template #extra>
            <span class="src-hint">近 10 次 · 同日多原因各自成行</span>
          </template>
          <div v-if="lhbRecords.length" class="lhb-list">
            <div v-for="(r, i) in lhbRecords" :key="i" class="lhb-row">
              <span class="news-time qv-tnum">{{ r.trade_date }}</span>
              <span class="lhb-reason">{{ r.reason }}<span v-if="r.note" class="lhb-note">（{{ r.note }}）</span></span>
              <span class="lhb-num qv-tnum">当日 <ChangeTag :value="r.change_pct" /></span>
              <span class="lhb-num qv-tnum" :style="{ color: pctColor(r.net_buy) }">净买 {{ fmtNetYi(r.net_buy) }} 亿</span>
              <span v-if="r.org_net_buy" class="lhb-num qv-tnum" :style="{ color: pctColor(r.org_net_buy) }">机构 {{ fmtNetYi(r.org_net_buy) }} 亿</span>
            </div>
          </div>
          <n-empty v-else description="近期无上榜记录（覆盖近 30 天龙虎榜采集）" />
        </SectionCard>

            </div>
          </StockTabState>
        </n-tab-pane>

        <n-tab-pane name="event" tab="事件" display-directive="show:lazy">
          <StockTabState
            :phase="tabStates.event.phase"
            :error="tabStates.event.error"
            :has-data="tabDataState.event"
            @retry="loadTab('event', true)"
          >
            <div class="tab-content">
        <!-- 解禁 / 分红（B9）：每日 19:25 同步的本地表。
             **状态三分**：读取失败 / 数据不可用 / 确实没有——空态文案严格区分，
             绝不把「查不到」显示成「无解禁」（与 AI 侧 riskGateNoteFor 同一纪律）。 -->
        <SectionCard v-if="market === 'cn' && !isFund" title="解禁 / 分红">
          <template #extra>
            <span class="src-hint">解禁前瞻半年 · 分红近 8 期</span>
          </template>
          <n-spin :show="tabStates.event.phase === 'refreshing' && !corpEvents">
            <n-alert v-if="corpEventsError" type="error" :bordered="false" title="解禁 / 分红读取失败">
              {{ corpEventsError }}——这不代表无解禁，请稍后刷新或自行核查公告。
            </n-alert>
            <n-alert
              v-else-if="corpEvents && corpEvents.lift_unavailable"
              type="warning"
              :bordered="false"
              title="解禁数据本次不可用"
            >
              {{ corpEvents.note || '同步未完成或查询失败，无法判断解禁风险，请自行核查。' }}
            </n-alert>
            <template v-else-if="corpEvents">
              <div class="corp-sub">限售解禁</div>
              <div v-if="upcomingLifts.length || pastLifts.length" class="lhb-list">
                <div v-for="(l, i) in upcomingLifts" :key="'u' + i" class="lhb-row">
                  <span class="news-time qv-tnum">{{ l.free_date }}</span>
                  <n-tag size="tiny" round :bordered="false" type="warning">{{ liftDaysLeft(l.free_date) }}</n-tag>
                  <span class="lhb-reason">{{ l.free_type || '限售股解禁' }}</span>
                  <span class="lhb-num qv-tnum">{{ fmtWanShares(l.free_shares) }} 万股</span>
                  <span class="lhb-num qv-tnum">{{ (l.lift_market_cap / 1e8).toFixed(2) }} 亿元</span>
                  <span class="lhb-num qv-tnum" :style="{ color: liftRatioColor(l.free_ratio) }"
                    >占流通 {{ l.free_ratio.toFixed(2) }}%</span
                  >
                </div>
                <div v-for="(l, i) in pastLifts" :key="'p' + i" class="lhb-row corp-past">
                  <span class="news-time qv-tnum">{{ l.free_date }}</span>
                  <n-tag size="tiny" round :bordered="false">{{ liftDaysLeft(l.free_date) }}</n-tag>
                  <span class="lhb-reason">{{ l.free_type || '限售股解禁' }}</span>
                  <span class="lhb-num qv-tnum">{{ fmtWanShares(l.free_shares) }} 万股</span>
                  <span class="lhb-num qv-tnum">占流通 {{ l.free_ratio.toFixed(2) }}%</span>
                </div>
              </div>
              <n-empty v-else description="近 3 个月至未来半年内无解禁安排（数据已同步，非缺失）" :show-icon="false" />

              <div class="corp-sub">分红送转</div>
              <n-alert v-if="corpEvents.action_unavailable" type="warning" :bordered="false" :show-icon="false">
                分红数据本次不可用，无法判断分红情况。
              </n-alert>
              <div v-else-if="corpEvents.actions.length" class="lhb-list">
                <div v-for="(a, i) in corpEvents.actions" :key="i" class="lhb-row">
                  <span class="news-time qv-tnum">{{ a.report_date }}</span>
                  <span class="lhb-reason">{{ planText(a) }}</span>
                  <span v-if="a.ex_date" class="lhb-num qv-tnum">除权 {{ a.ex_date }}</span>
                  <span v-else class="lhb-num">除权日待定</span>
                  <span v-if="a.dividend_yield > 0" class="lhb-num qv-tnum"
                    >股息率 {{ a.dividend_yield.toFixed(2) }}%</span
                  >
                  <n-tag v-if="a.progress" size="tiny" round :bordered="false">{{ a.progress }}</n-tag>
                </div>
              </div>
              <n-empty v-else description="暂无分红送转方案记录（数据已同步，非缺失）" :show-icon="false" />
            </template>
            <n-empty v-else description="解禁 / 分红数据加载中" :show-icon="false" />
          </n-spin>
        </SectionCard>

        <SectionCard title="公告">
          <div v-if="announcements.length" class="news-list">
            <div v-for="a in announcements" :key="a.art_code" class="news-row">
              <span class="news-time qv-tnum">{{ a.notice_date }}</span>
              <a :href="a.url" target="_blank" rel="noopener noreferrer" class="news-title">{{ a.title }}</a>
              <span v-if="a.notice_type" class="news-src">{{ a.notice_type }}</span>
            </div>
          </div>
          <n-empty v-else description="请求成功，当前没有公告记录" />
        </SectionCard>

        <SectionCard title="相关新闻">
          <template #extra>
            <RouterLink :to="{ name: 'news', query: { symbol } }" class="news-more">更多快讯 →</RouterLink>
          </template>
          <div v-if="news.length" class="news-list">
            <div v-for="n in news" :key="n.id" class="news-row">
              <span class="news-time qv-tnum">{{ fmtNewsTime(n.publish_time) }}</span>
              <a v-if="n.url" :href="n.url" target="_blank" rel="noopener noreferrer" class="news-title">{{ n.title }}</a>
              <span v-else class="news-title">{{ n.title }}</span>
              <span
                v-if="sentiView(n)"
                class="news-senti"
                :style="{ color: sentiView(n)!.color, background: withAlpha(sentiView(n)!.color, isDark ? 0.16 : 0.1) }"
              >{{ sentiView(n)!.text }}</span>
              <span class="news-src">{{ newsSourceLabel(n) }}</span>
            </div>
          </div>
          <n-empty v-else description="请求成功，当前没有相关新闻记录" />
        </SectionCard>
            </div>
          </StockTabState>
        </n-tab-pane>

        <n-tab-pane name="fundamental" tab="基本面" display-directive="show:lazy">
          <StockTabState
            :phase="tabStates.fundamental.phase"
            :error="tabStates.fundamental.error"
            :has-data="tabDataState.fundamental"
            @retry="loadTab('fundamental', true)"
          >
            <div class="tab-content">
        <!-- 估值 + 评分 -->
        <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
          <n-gi>
            <SectionCard title="估值快照">
              <template v-if="valuation" #extra>
                <span class="src-hint">{{ valuation.source }}</span>
              </template>
              <div v-if="valuation" class="quote-grid">
                <template v-if="!isFund">
                  <div class="qc"><span class="qc-k">PE-TTM</span><span class="qc-v qv-tnum">{{ fmt(valuation.pe_ttm) }}</span></div>
                  <div class="qc"><span class="qc-k">PE(动)</span><span class="qc-v qv-tnum">{{ fmt(valuation.pe_dynamic) }}</span></div>
                  <div class="qc"><span class="qc-k">市净率</span><span class="qc-v qv-tnum">{{ fmt(valuation.pb) }}</span></div>
                  <div class="qc"><span class="qc-k">总市值</span><span class="qc-v qv-tnum">{{ fmtCap(valuation.total_cap) }}</span></div>
                  <div class="qc"><span class="qc-k">流通市值</span><span class="qc-v qv-tnum">{{ fmtCap(valuation.float_cap) }}</span></div>
                  <!-- C10 股息率：来源与本卡其余项不同（东财分红方案，非腾讯实时行情），
                       故必须带报告期时点与来源说明；无数据时整项缺席，绝不显示 0%
                       （0% 会被读成「不分红」，而实际只是本地还没有该股的方案数据）。 -->
                  <div v-if="dividendYield" class="qc">
                    <span class="qc-k">股息率</span>
                    <n-tooltip trigger="hover" :style="{ maxWidth: '320px' }">
                      <template #trigger>
                        <span class="qc-v qv-tnum qc-hint"
                          >{{ dividendYield.yield_pct.toFixed(2) }}%
                          <span class="qc-asof">{{ dividendYield.report_date }}</span></span
                        >
                      </template>
                      {{ dividendYield.note }}
                    </n-tooltip>
                  </div>
                </template>
                <div v-else class="qc qc-wide"><span class="qc-k">类型</span><span class="qc-v">ETF/场内基金（无 PE/PB 个股估值指标）</span></div>
                <div class="qc"><span class="qc-k">换手率</span><span class="qc-v qv-tnum">{{ fmt(valuation.turnover_rate) }}%</span></div>
                <div class="qc"><span class="qc-k">振幅</span><span class="qc-v qv-tnum">{{ fmt(valuation.amplitude) }}%</span></div>
                <div class="qc"><span class="qc-k">量比</span><span class="qc-v qv-tnum">{{ fmt(valuation.volume_ratio) }}</span></div>
                <div class="qc"><span class="qc-k">涨停/跌停</span><span class="qc-v qv-tnum">{{ fmt(valuation.limit_up) }} / {{ fmt(valuation.limit_down) }}</span></div>
              </div>
              <n-empty v-else description="估值数据暂不可用（腾讯源）" />
            </SectionCard>
          </n-gi>
          <n-gi>
            <SectionCard title="技术面评分">
              <template v-if="score" #extra>
                <span class="src-hint">{{ score.trade_date }}</span>
              </template>
              <div v-if="score" class="score">
                <div class="score-hero">
                  <span class="score-total qv-figure">{{ score.total.toFixed(0) }}</span>
                  <n-tag :type="scoreType(score.total)" round :bordered="false">{{ score.label }}</n-tag>
                  <span v-if="score.data_limited" class="src-hint">（日线不足，精度受限）</span>
                </div>
                <div v-for="d in scoreDims" :key="d.label" class="score-dim">
                  <span class="sd-k">{{ d.label }}</span>
                  <div class="sd-bar">
                    <div class="sd-fill" :style="{ width: d.value + '%', background: vars.primaryColor }"></div>
                  </div>
                  <span class="sd-v qv-tnum">{{ d.value.toFixed(0) }}</span>
                </div>
                <div class="src-hint" style="margin-top: 6px">
                  纯技术面五维（趋势/动量/位置/量能/回撤风险），无财务维度；仅研究参考。
                </div>
              </div>
              <n-empty v-else description="评分暂不可用" />
            </SectionCard>
          </n-gi>
        </n-grid>

        <!-- 财务摘要（F2）：F10 主要指标近 8 期，营收/净利柱 + ROE/毛利率线 -->
        <SectionCard v-if="market === 'cn' && !isFund" title="财务摘要（近 8 期）">
          <template #extra>
            <span class="src-hint">东财 F10 · 季报口径</span>
          </template>
          <div v-if="finance && finance.indicators.length" class="fin-wrap">
            <div v-if="finLatest" class="quote-grid fin-grid">
              <div class="qc"><span class="qc-k">报告期</span><span class="qc-v">{{ finLatest.report_name }}</span></div>
              <div class="qc"><span class="qc-k">EPS</span><span class="qc-v qv-tnum">{{ finLatest.eps.toFixed(2) }}</span></div>
              <div class="qc"><span class="qc-k">ROE</span><span class="qc-v qv-tnum">{{ finLatest.roe.toFixed(2) }}%</span></div>
              <div class="qc"><span class="qc-k">营收同比</span><span class="qc-v qv-tnum" :style="{ color: pctColor(finLatest.revenue_yoy) }">{{ finLatest.revenue_yoy.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">净利同比</span><span class="qc-v qv-tnum" :style="{ color: pctColor(finLatest.net_profit_yoy) }">{{ finLatest.net_profit_yoy.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">毛利率</span><span class="qc-v qv-tnum">{{ finLatest.gross_margin.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">净利率</span><span class="qc-v qv-tnum">{{ finLatest.net_margin.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">资产负债率</span><span class="qc-v qv-tnum">{{ finLatest.debt_ratio.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">每股经营现金流</span><span class="qc-v qv-tnum">{{ finLatest.ocf_ps.toFixed(2) }}</span></div>
            </div>
            <div ref="finEl" class="fin-chart"></div>
            <div class="src-hint">季报为累计口径且有披露滞后；0 值可能表示上游数据缺失；仅研究参考。</div>
          </div>
          <n-empty v-else description="暂无财务数据（东财 F10，A 股标的；首次访问自动拉取，可稍后刷新）" />
        </SectionCard>

            </div>
          </StockTabState>
        </n-tab-pane>

        <n-tab-pane name="research" tab="研究" display-directive="show:lazy">
          <StockTabState
            :phase="tabStates.research.phase"
            :error="tabStates.research.error"
            :has-data="tabDataState.research"
            @retry="loadTab('research', true)"
          >
            <div class="tab-content">
        <StockCoverageMatrix :items="coverageItems" />

        <!-- 机构观点（P3a）：研报评级分布/变动/目标价 + 机构调研；按需拉取首访可能为空 -->
        <SectionCard v-if="market === 'cn' && !isFund" title="机构观点">
          <template #extra>
            <span class="src-hint">东财研报/调研 · 汇总窗口 90/180 天</span>
          </template>
          <div v-if="orgview && (orgview.reports.length || orgview.surveys.length)" class="ov-wrap">
            <div v-if="orgview.summary" class="quote-grid ov-grid">
              <div v-if="ovDistText" class="qc"><span class="qc-k">评级分布(90天)</span><span class="qc-v">{{ ovDistText }}</span></div>
              <div v-if="ovChgText" class="qc"><span class="qc-k">评级变动(90天)</span><span class="qc-v">{{ ovChgText }}</span></div>
              <div v-if="ovTp" class="qc">
                <span class="qc-k">目标价中位({{ ovTp.count }}份)</span>
                <span class="qc-v qv-tnum">{{ ovTp.median.toFixed(2) }}<span v-if="ovTp.median_vs_price_pct != null" :style="{ color: pctColor(ovTp.median_vs_price_pct) }">（{{ fmtSignedPct(ovTp.median_vs_price_pct) }}%）</span></span>
              </div>
              <div v-if="ovSv" class="qc"><span class="qc-k">调研批次(30/90天)</span><span class="qc-v qv-tnum">{{ ovSv.batches_30d }} / {{ ovSv.batches_90d }}</span></div>
            </div>
            <div v-if="ovLc" class="ov-change">
              最近评级变动：<span class="qv-tnum">{{ ovLc.date }}</span> {{ ovLc.org }}
              <span :style="{ color: pctColor(ovLc.kind === '上调' ? 1 : -1), fontWeight: 600 }">{{ ovLc.kind }}</span>
              （{{ ovLc.from }} → {{ ovLc.to }}）
            </div>
            <div v-if="orgview.reports.length" class="lhb-list">
              <div v-for="(r, i) in orgview.reports" :key="i" class="lhb-row">
                <span class="news-time qv-tnum">{{ r.report_date }}</span>
                <span class="ov-org">{{ r.org_name }}</span>
                <span class="ov-rating">
                  {{ r.rating || '未评级' }}<template v-if="ratingChangeMark(r.rating_change)"><span :style="{ color: pctColor(ratingChangeMark(r.rating_change)!.dir) }">（{{ ratingChangeMark(r.rating_change)!.text }}）</span></template><span v-else-if="r.rating_change === 2" class="lhb-note">（首次）</span>
                </span>
                <span v-if="r.target_price" class="lhb-num qv-tnum">目标 {{ r.target_price.toFixed(2) }}</span>
                <span class="ov-title" :title="r.title">{{ r.title }}</span>
              </div>
            </div>
            <div v-if="orgview.surveys.length" class="ov-svy">
              <div class="ov-svy-head">机构调研</div>
              <div v-for="(s, i) in orgview.surveys" :key="i" class="lhb-row">
                <span class="news-time qv-tnum">{{ s.survey_date }}</span>
                <span class="lhb-num qv-tnum">{{ s.org_count }} 家机构</span>
                <span class="ov-title" :title="s.org_names">{{ s.org_names }}<span v-if="s.receive_way" class="lhb-note">（{{ s.receive_way }}）</span></span>
              </div>
            </div>
            <div class="src-hint">
              卖方研报评级普遍乐观（九成为买入/增持），更有参考价值的是评级下调、目标价与现价的偏离、调研密度变化；目标价样本少时代表性有限；仅研究参考。
            </div>
          </div>
          <n-empty v-else description="暂无机构观点数据（研报/调研覆盖 A 股，首次访问自动拉取，可稍后刷新）" />
        </SectionCard>
            </div>
          </StockTabState>
        </n-tab-pane>
      </n-tabs>
    </div>

    <nav class="mobile-action-bar" aria-label="当前股票快捷动作">
      <button type="button" :disabled="adding" @click="handleSummaryAction('watch')">
        {{ inWatchlist ? '观察中' : '观察' }}
      </button>
      <button type="button" @click="handleSummaryAction('alert')">提醒</button>
      <button type="button" class="primary" @click="handleSummaryAction('analysis')">分析</button>
      <n-dropdown trigger="click" placement="top-end" :options="moreOptions" @select="selectMoreAction">
        <button type="button" aria-label="更多股票操作" title="更多股票操作">⋯</button>
      </n-dropdown>
    </nav>
  </PageContainer>
</template>

<style scoped>
.detail {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 16px;
}
.tab-content {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 16px;
  padding-top: 4px;
}
.mobile-action-bar {
  display: none;
}
.quote-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(130px, 1fr));
  gap: 10px 14px;
}
.qc {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qc-wide {
  grid-column: 1 / -1;
}
.qc-k {
  font-size: 12px;
  opacity: 0.55;
}
.qc-v {
  font-weight: 600;
}
/* C10 股息率：与本卡其余项来源不同，用虚线下划线提示「悬停看口径」，
   报告期弱化随行展示（不带时点的股息率会被读成实时值）。 */
.qc-hint {
  cursor: help;
  border-bottom: 1px dotted currentColor;
}
.qc-asof {
  font-weight: 400;
  font-size: 11px;
  opacity: 0.55;
  margin-left: 2px;
}
.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
}
.kchart {
  width: 100%;
  height: 460px;
}
.minute-meta {
  display: flex;
  align-items: center;
  gap: 8px 16px;
  min-height: 24px;
  flex-wrap: wrap;
  font-size: 12px;
  opacity: 0.68;
}
.minute-note {
  margin-top: 6px;
  line-height: 1.6;
}
@media (max-width: 768px) {
  .detail {
    max-width: 100%;
    padding-bottom: 68px;
    overflow-x: clip;
  }
  .mobile-action-bar {
    position: fixed;
    right: 12px;
    bottom: calc(57px + env(safe-area-inset-bottom, 0px));
    left: 12px;
    z-index: 89;
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr)) 48px;
    min-height: 52px;
    box-sizing: border-box;
    overflow: hidden;
    border: 1px solid v-bind('vars.dividerColor');
    border-radius: 8px 8px 0 0;
    background: v-bind('vars.cardColor');
    box-shadow: 0 -4px 18px v-bind('vars.boxShadow2');
  }
  .mobile-action-bar button {
    width: 100%;
    min-width: 0;
    min-height: 50px;
    padding: 0 6px;
    border: 0;
    border-right: 1px solid v-bind('vars.dividerColor');
    background: transparent;
    color: v-bind('vars.textColor1');
    font: inherit;
    font-size: 13px;
    font-weight: 600;
    letter-spacing: 0;
  }
  .mobile-action-bar button.primary {
    background: v-bind('vars.primaryColor');
    color: v-bind('vars.baseColor');
  }
  .mobile-action-bar button:disabled {
    opacity: 0.5;
  }
  /* 小屏 460px ≈ 72vh 占满一屏还多，与 chip/ff/fin 图同做降高 */
  .kchart {
    height: 360px;
  }
  .minute-meta {
    gap: 4px 12px;
  }
}

@media (max-width: 768px) and (max-height: 600px) {
  .detail {
    padding-bottom: 0;
  }
  .mobile-action-bar {
    position: static;
    margin-top: 12px;
  }
}

/* ---------- 筹码分布 ---------- */
.chip-wrap {
  display: grid;
  grid-template-columns: minmax(0, 1.2fr) minmax(0, 1fr);
  gap: 16px;
  align-items: stretch;
}
.chip-chart {
  width: 100%;
  height: 320px;
  min-width: 0;
}
.chip-side {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
}
.chip-hero {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.chip-profit {
  font-size: 30px;
  font-weight: 700;
  line-height: 1;
}
.chip-grid {
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
}
.chip-trend-block {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.chip-trend {
  width: 100%;
  height: 64px;
}
@media (max-width: 768px) {
  .chip-wrap {
    grid-template-columns: 1fr;
  }
  .chip-chart {
    height: 260px;
  }
}
.src-hint {
  font-size: 12px;
  opacity: 0.55;
}

/* ---------- 主力资金 / 龙虎榜 ---------- */
.ff-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.ff-grid {
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
}
.ff-chart {
  width: 100%;
  height: 280px;
}
.lhb-list {
  display: flex;
  flex-direction: column;
}
.lhb-row {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 8px 0;
  flex-wrap: wrap;
}
.lhb-row + .lhb-row {
  border-top: 1px dashed rgba(128, 128, 128, 0.22);
}
.lhb-reason {
  flex: 1;
  min-width: 200px;
  font-size: 13px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.lhb-note {
  opacity: 0.6;
  font-size: 12px;
}
/* B9 解禁 / 分红块 */
.corp-sub {
  font-size: 12px;
  font-weight: 600;
  opacity: 0.7;
  margin: 10px 0 2px;
}
.corp-sub:first-child {
  margin-top: 0;
}
/* 已过去的解禁淡化：是背景不是决策变量 */
.corp-past {
  opacity: 0.6;
}
.lhb-num {
  flex-shrink: 0;
  font-size: 12px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
@media (max-width: 768px) {
  .ff-chart {
    height: 220px;
  }
  .lhb-reason {
    flex-basis: 100%;
    order: 3;
  }
}

/* ---------- 财务摘要 ---------- */
.fin-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.fin-grid {
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
}
.fin-chart {
  width: 100%;
  height: 300px;
}
@media (max-width: 768px) {
  .fin-chart {
    height: 240px;
  }
}

/* ---------- 机构观点 ---------- */
.ov-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.ov-grid {
  grid-template-columns: repeat(auto-fill, minmax(170px, 1fr));
}
.ov-change {
  font-size: 13px;
}
.ov-org {
  flex-shrink: 0;
  font-size: 13px;
  min-width: 72px;
}
.ov-rating {
  flex-shrink: 0;
  font-size: 13px;
}
.ov-title {
  flex: 1;
  min-width: 200px;
  font-size: 13px;
  line-height: 1.55;
  opacity: 0.85;
  overflow-wrap: anywhere;
}
.ov-svy-head {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 2px;
}
@media (max-width: 768px) {
  .ov-title {
    flex-basis: 100%;
    order: 5;
  }
}
.score {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.score-hero {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 4px;
}
.score-total {
  font-size: 34px;
  font-weight: 700;
  line-height: 1;
}
.score-dim {
  display: flex;
  align-items: center;
  gap: 10px;
}
.sd-k {
  width: 58px;
  font-size: 12px;
  opacity: 0.7;
  flex-shrink: 0;
}
.sd-bar {
  flex: 1;
  height: 6px;
  border-radius: 3px;
  background: color-mix(in srgb, currentColor 12%, transparent);
  overflow: hidden;
}
.sd-fill {
  height: 100%;
  border-radius: 3px;
  transition: width 0.3s ease;
}
.sd-v {
  width: 28px;
  text-align: right;
  font-size: 12px;
}

/* ---------- 相关新闻 ---------- */
.news-more {
  font-size: 12px;
  color: var(--qv-primary);
  text-decoration: none;
  opacity: 0.85;
}
.news-more:hover {
  opacity: 1;
}
.news-list {
  display: flex;
  flex-direction: column;
}
.news-row {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 8px 0;
}
.news-row + .news-row {
  border-top: 1px dashed rgba(128, 128, 128, 0.22);
}
.news-time {
  flex-shrink: 0;
  font-size: 12px;
  opacity: 0.55;
}
.news-title {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  line-height: 1.55;
  color: inherit;
  text-decoration: none;
  overflow-wrap: anywhere;
}
a.news-title:hover {
  color: var(--qv-primary);
}
.news-src {
  flex-shrink: 0;
  font-size: 11px;
  opacity: 0.5;
}
.news-senti {
  flex-shrink: 0;
  font-size: 11px;
  font-weight: 600;
  padding: 0 7px;
  border-radius: 999px;
  line-height: 18px;
}

@media (max-width: 768px) {
  .news-row {
    flex-wrap: wrap;
    gap: 4px 10px;
  }
  .news-title {
    flex-basis: 100%;
    order: 3;
  }
}
</style>
