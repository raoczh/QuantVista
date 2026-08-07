<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick, watch, computed } from 'vue'
import { useRouter } from 'vue-router'
import {
  NInput,
  NButton,
  NGrid,
  NGi,
  NSpin,
  NEmpty,
  NAlert,
  NTag,
  NRadioGroup,
  NRadioButton,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getOverview,
  getValuation,
  type Quote,
  type Bar,
  type Overview,
  type Valuation,
} from '@/api/market'
import { listPositions, type Position } from '@/api/position'
import { getTodos, type TodoItem, type TodoResult } from '@/api/todo'
import { listWatchlists, type WatchlistGroup, type WatchlistItem } from '@/api/watchlist'
import { listRecommendations, type RecommendationBatch } from '@/api/recommendation'
import {
  generateDailyReport,
  getLatestDailyReport,
  type DailyReportView,
} from '@/api/report'
import {
  formatTaskCompactTime,
  listTasks,
  taskKindLabel,
  taskResultRoute,
  taskStatusLabel,
  type TaskCenterItem,
  type TaskStatus,
} from '@/api/taskCenter'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useStockActions } from '@/composables/useStockActions'
import { useAuthStore } from '@/stores/auth'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import RankList from '@/components/RankList.vue'
import ChangeTag from '@/components/ChangeTag.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'
import HomeWorkspaceSection from '@/components/home/HomeWorkspaceSection.vue'
import {
  defaultExpandedSections,
  rankPinnedWatchChanges,
  resolveHomeSession,
  sortHomeSections,
  sortPositionRisks,
  sortPriorityItems,
  type HomeModePreference,
  type HomeWorkspaceMode,
  type HomeWorkspaceSectionID,
} from '@/components/home/homeWorkspace'

const message = useMessage()
const router = useRouter()
const auth = useAuthStore()
const { vars, isDark, pctColor, upColor, downColor, flatColor, withAlpha } = useUi()
const { llmLabel } = useLlmLabel()
const { adding, goAnalysis, goQa, goCompare, goAlert, goDetail, addToWatchlist } = useStockActions()

// ---------- 市场概览 ----------
const overview = ref<Overview | null>(null)
const ovLoading = ref(false)
const ovError = ref('')

async function loadOverview(silent = false) {
  if (!silent) ovLoading.value = true
  ovError.value = ''
  try {
    overview.value = await getOverview('cn')
  } catch (e) {
    ovError.value = errorText(e, '市场概览暂不可用')
  } finally {
    if (!silent) ovLoading.value = false
  }
}

// ---------- 个股速查（行情状态同时为首页自动模式提供交易状态） ----------
const symbol = ref('600000')
const quote = ref<Quote | null>(null)
const valuation = ref<Valuation | null>(null)
const loading = ref(false)
const quoteError = ref('')
const barsError = ref('')
const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const lastBars = ref<Bar[]>([])
let stockLoadSeq = 0

// ---------- 我的今日：每个数据块独立加载，刷新失败保留最近已知值 ----------
const positions = ref<Position[]>([])
const positionsLoading = ref(false)
const positionsLoaded = ref(false)
const positionsError = ref('')
const mineTodo = ref<TodoResult | null>(null)
const mineTodoLoading = ref(false)
const mineTodoLoaded = ref(false)
const mineTodoError = ref('')
const watchGroups = ref<WatchlistGroup[]>([])
const watchLoading = ref(false)
const watchLoaded = ref(false)
const watchError = ref('')
const mineRec = ref<RecommendationBatch | null>(null)
const mineRecLoading = ref(false)
const mineRecLoaded = ref(false)
const mineRecError = ref('')
const latestReport = ref<DailyReportView | null>(null)
const reportLoading = ref(false)
const reportLoaded = ref(false)
const reportError = ref('')
const reportGenerating = ref(false)
const recentTasks = ref<TaskCenterItem[]>([])
const tasksLoading = ref(false)
const tasksLoaded = ref(false)
const tasksError = ref('')

function errorText(error: unknown, fallback: string) {
  return error instanceof Error && error.message ? error.message : fallback
}

async function loadPositions(silent = false) {
  if (!silent || !positions.value.length) positionsLoading.value = true
  positionsError.value = ''
  try {
    positions.value = await listPositions('holding')
  } catch (error) {
    positionsError.value = errorText(error, '持仓数据暂不可用')
  } finally {
    positionsLoaded.value = true
    positionsLoading.value = false
  }
}

async function loadTodos(silent = false) {
  if (!silent || !mineTodo.value) mineTodoLoading.value = true
  mineTodoError.value = ''
  try {
    const value = await getTodos()
    mineTodo.value = value
    if (!value.complete) mineTodoError.value = value.errors?.join('；') || '待办清单读取不完整'
  } catch (error) {
    mineTodoError.value = errorText(error, '待办数据暂不可用')
  } finally {
    mineTodoLoaded.value = true
    mineTodoLoading.value = false
  }
}

async function loadWatchlists(silent = false) {
  if (!silent || !watchGroups.value.length) watchLoading.value = true
  watchError.value = ''
  try {
    watchGroups.value = await listWatchlists()
  } catch (error) {
    watchError.value = errorText(error, '自选数据暂不可用')
  } finally {
    watchLoaded.value = true
    watchLoading.value = false
  }
}

async function loadRecommendations(silent = false) {
  if (!silent || !mineRecLoaded.value) mineRecLoading.value = true
  mineRecError.value = ''
  try {
    mineRec.value = (await listRecommendations(undefined, 1))[0] ?? null
  } catch (error) {
    mineRecError.value = errorText(error, '推荐结果暂不可用')
  } finally {
    mineRecLoaded.value = true
    mineRecLoading.value = false
  }
}

async function loadLatestReport(silent = false) {
  if (!silent || !reportLoaded.value) reportLoading.value = true
  reportError.value = ''
  try {
    latestReport.value = await getLatestDailyReport()
  } catch (error) {
    reportError.value = errorText(error, '收盘日报暂不可用')
  } finally {
    reportLoaded.value = true
    reportLoading.value = false
  }
}

async function loadRecentTasks(silent = false) {
  if (!silent || !tasksLoaded.value) tasksLoading.value = true
  tasksError.value = ''
  try {
    recentTasks.value = await listTasks({ limit: 3 })
  } catch (error) {
    tasksError.value = errorText(error, '最近任务暂不可用')
  } finally {
    tasksLoaded.value = true
    tasksLoading.value = false
  }
}

async function loadPersonalWorkspace(silent = false) {
  await Promise.allSettled([
    loadPositions(silent),
    loadTodos(silent),
    loadWatchlists(silent),
    loadRecommendations(silent),
    loadLatestReport(silent),
    loadRecentTasks(silent),
  ])
}

const holdingPositions = computed(() => positions.value.filter((item) => item.status === 'holding'))
const pricedPositions = computed(() => holdingPositions.value.filter((item) => item.quote_ok))
const quoteGapPositions = computed(() => holdingPositions.value.filter((item) => !item.quote_ok))
const minePos = computed(() => {
  let cost = 0
  let pnl = 0
  for (const item of pricedPositions.value) {
    cost += item.cost
    pnl += item.profit_amount
  }
  return {
    pnl,
    pct: cost > 0 ? (pnl / cost) * 100 : 0,
    n: holdingPositions.value.length,
    priced: pricedPositions.value.length,
  }
})
const positionRisks = computed(() => sortPositionRisks(holdingPositions.value))
const priorityTodos = computed(() => sortPriorityItems(mineTodo.value?.items || []))
const watchItems = computed<WatchlistItem[]>(() => watchGroups.value.flatMap((group) => group.items))
const focusedWatchItems = computed(() => rankPinnedWatchChanges(watchItems.value))
const watchFreshCount = computed(() => watchItems.value.filter((item) => item.quote_ok).length)

function positionRiskLabels(position: Position) {
  const labels: string[] = []
  if (position.below_stop_loss) labels.push('已跌破计划止损')
  if (position.near_stop_loss) labels.push('接近计划止损')
  if (position.short_term_review) labels.push('短线持有待复盘')
  if (position.analysis_stale) labels.push('研究结论超过 7 天')
  return labels
}

function newestAsOf(values: Array<string | undefined>) {
  return values.filter((value): value is string => !!value).sort().at(-1) || 'unknown'
}

const positionAsOf = computed(() => newestAsOf(holdingPositions.value.map((item) => item.quote_as_of)))
const todoSummary = computed(() => {
  if (!mineTodo.value) return mineTodoError.value ? '状态 unknown' : '正在读取'
  if (!mineTodo.value.total) return mineTodo.value.complete ? '今日已全部处理' : '最近已知 0 项，完整性 unknown'
  return `${mineTodoError.value ? '最近已知 ' : mineTodo.value.complete ? '' : '至少 '}${mineTodo.value.total} 项待处理`
})
const positionSummary = computed(() => {
  if (!positionsLoaded.value && positionsLoading.value) return '正在读取'
  if (positionsError.value && !holdingPositions.value.length) return '状态 unknown'
  if (!holdingPositions.value.length) return '尚未记录持仓'
  return `${positionsError.value ? '最近已知 ' : ''}${positionRisks.value.length} 笔需关注 · ${quoteGapPositions.value.length} 笔行情缺口`
})
const watchSummary = computed(() => {
  if (!watchLoaded.value && watchLoading.value) return '正在读取'
  if (watchError.value && !watchItems.value.length) return '状态 unknown'
  if (!watchItems.value.length) return '尚未添加自选'
  const prefix = watchError.value ? '最近已知 ' : ''
  if (!focusedWatchItems.value.length) return `${prefix}${watchItems.value.length} 只自选 · 尚无重点`
  return `${prefix}${focusedWatchItems.value.length} 只重点 · ${watchFreshCount.value}/${watchItems.value.length} 行情有效`
})
const researchSummary = computed(() => {
  if (!tasksLoaded.value && tasksLoading.value) return '正在读取'
  const prefix = tasksError.value || reportError.value || mineRecError.value ? '最近已知 ' : ''
  const task = recentTasks.value[0]
  if (task) return `${prefix}${taskKindLabel(task.kind)} · ${taskStatusLabel(task.status)}`
  if (latestReport.value) return `${prefix}${latestReport.value.trade_date} 收盘日报`
  if (mineRec.value) return `${prefix}${recTypeText(mineRec.value.type)}策略 · ${relDay(mineRec.value.created_at)}`
  if (tasksError.value || reportError.value || mineRecError.value) return '状态 unknown'
  return '尚无研究结果'
})

function sectionTitle(id: HomeWorkspaceSectionID) {
  const titles: Record<HomeWorkspaceSectionID, string> = {
    todos: '待处理事项',
    positions: '持仓风险与盈亏',
    watchlist: '重点自选变化',
    research: '最近任务与研究',
  }
  return titles[id]
}

function sectionSummary(id: HomeWorkspaceSectionID) {
  if (id === 'todos') return todoSummary.value
  if (id === 'positions') return positionSummary.value
  if (id === 'watchlist') return watchSummary.value
  return researchSummary.value
}

function recTypeText(t: string) {
  return t.includes('short') ? '短线' : t.includes('long') ? '长线' : t
}

function relDay(iso: string) {
  const d = new Date(iso)
  const today = new Date()
  const days = Math.floor((today.setHours(0, 0, 0, 0) - new Date(d).setHours(0, 0, 0, 0)) / 86400000)
  if (days <= 0) return '今天'
  if (days === 1) return '昨天'
  return `${days} 天前`
}

function fmtSigned(n: number) {
  return (
    (n >= 0 ? '+' : '') +
    n.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
  )
}

// ---------- 分时模式：后端交易状态优先，缺失时保持 unknown ----------
const now = ref(new Date())
const modePreference = ref<HomeModePreference>('auto')
const modeStorageOwner = ref(0)
const workspaceSections: HomeWorkspaceSectionID[] = ['todos', 'positions', 'watchlist', 'research']
const marketState = computed(() =>
  quoteError.value ? undefined : quote.value?.freshness?.market_state,
)
const autoSession = computed(() => resolveHomeSession(now.value, marketState.value))
const effectiveMode = computed<HomeWorkspaceMode>(() =>
  modePreference.value === 'auto' ? autoSession.value.mode : modePreference.value,
)
const orderedSections = computed(() => sortHomeSections(workspaceSections, effectiveMode.value))
const expandedSections = ref<HomeWorkspaceSectionID[]>([])

function modeStorageKey(userID: number) {
  return `qv:home-mode:${userID}`
}

function readModePreference(userID: number): HomeModePreference {
  if (!userID) return 'auto'
  try {
    const saved = localStorage.getItem(modeStorageKey(userID))
    return saved === 'pre' || saved === 'intraday' || saved === 'post' ? saved : 'auto'
  } catch {
    return 'auto'
  }
}

watch(
  () => auth.user?.id,
  (userID) => {
    modeStorageOwner.value = userID || 0
    modePreference.value = readModePreference(modeStorageOwner.value)
  },
  { immediate: true },
)

watch(modePreference, (value) => {
  if (!modeStorageOwner.value) return
  try {
    if (value === 'auto') localStorage.removeItem(modeStorageKey(modeStorageOwner.value))
    else localStorage.setItem(modeStorageKey(modeStorageOwner.value), value)
  } catch {
    // 本地存储不可用不影响当前会话切换。
  }
})

watch(
  effectiveMode,
  (mode) => {
    expandedSections.value = defaultExpandedSections(mode)
  },
  { immediate: true },
)

function toggleWorkspaceSection(id: HomeWorkspaceSectionID) {
  expandedSections.value = expandedSections.value.includes(id)
    ? expandedSections.value.filter((item) => item !== id)
    : [...expandedSections.value, id]
}

function modeLabel(mode: HomeWorkspaceMode) {
  if (mode === 'pre') return '盘前'
  if (mode === 'intraday') return '盘中'
  return '盘后/休市'
}

function marketStateLabel(state: string) {
  const labels: Record<string, string> = {
    pre_open: '盘前',
    trading: '交易中',
    break: '午间休市',
    post_close: '盘后',
    closed: '休市',
    unknown: 'unknown',
  }
  return labels[state] || 'unknown'
}

const modeStatusText = computed(() => {
  if (modePreference.value !== 'auto') return `${modeLabel(effectiveMode.value)} · 已固定`
  if (!autoSession.value.confirmed) return `${modeLabel(effectiveMode.value)}排序 · 交易状态 unknown`
  return `自动 · ${modeLabel(effectiveMode.value)}`
})

const modeFocusText = computed(() => {
  if (effectiveMode.value === 'pre') {
    const planState = latestReport.value?.review?.tomorrow_plan
      ? `${reportError.value ? '最近已知：' : ''}已就绪`
      : reportLoaded.value && !reportError.value
        ? '待补充'
        : 'unknown'
    const todoCount = mineTodo.value
      ? `${mineTodoError.value ? '最近已知 ' : ''}${mineTodo.value.total}`
      : 'unknown'
    return `待办 ${todoCount} 项 · 明日计划 ${planState}`
  }
  if (effectiveMode.value === 'intraday') {
    const riskCount = positionsLoaded.value && (!positionsError.value || holdingPositions.value.length)
      ? `${positionsError.value ? '最近已知 ' : ''}${positionRisks.value.length}`
      : 'unknown'
    const gapCount = positionsLoaded.value && (!positionsError.value || holdingPositions.value.length)
      ? `${positionsError.value ? '最近已知 ' : ''}${quoteGapPositions.value.length}`
      : 'unknown'
    const alertCount = mineTodo.value
      ? `${mineTodoError.value ? '最近已知 ' : ''}${mineTodo.value.alerts}`
      : 'unknown'
    return `持仓需关注 ${riskCount} 笔 · 行情缺口 ${gapCount} 笔 · 触发提醒 ${alertCount} 项`
  }
  const reportState = latestReport.value
    ? `${reportError.value ? '最近已知：' : ''}已生成`
    : reportLoaded.value && !reportError.value
      ? '未生成'
      : 'unknown'
  const pnl = minePos.value.priced
    ? `${positionsError.value ? '最近已知 ' : ''}${fmtSigned(minePos.value.pnl)}`
    : 'unknown'
  const reviewCount = mineTodo.value
    ? `${mineTodoError.value ? '最近已知 ' : ''}${mineTodo.value.reviews}`
    : 'unknown'
  return `持仓浮盈亏 ${pnl} · 待复盘 ${reviewCount} 项 · 日报 ${reportState}`
})

function openGlobalSearch() {
  window.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'k', code: 'KeyK', ctrlKey: true, bubbles: true }),
  )
}

function openTodo(item: TodoItem) {
  void router.push(item.deep_link || { name: 'today' })
}

function openTask(task: TaskCenterItem) {
  const target = taskResultRoute(task)
  if (target) void router.push(target)
}

function taskTagType(status: TaskStatus): 'info' | 'success' | 'warning' | 'error' {
  if (status === 'queued' || status === 'running') return 'info'
  if (status === 'success') return 'success'
  if (status === 'degraded' || status === 'canceled') return 'warning'
  return 'error'
}

async function generateReportOnce() {
  reportGenerating.value = true
  reportError.value = ''
  try {
    latestReport.value = await generateDailyReport()
    reportLoaded.value = true
    message.success('日报任务已提交，可在最近任务或日报页查看进度')
    void loadRecentTasks(true)
  } catch (error) {
    reportError.value = errorText(error, '日报生成失败')
  } finally {
    reportGenerating.value = false
  }
}

async function refreshResearch() {
  await Promise.allSettled([loadRecentTasks(), loadLatestReport(), loadRecommendations()])
}

const homeVars = computed(() => ({
  '--home-border': vars.value.borderColor,
  '--home-divider': vars.value.dividerColor,
  '--home-panel-bg': vars.value.cardColor,
  '--home-soft-bg': withAlpha(vars.value.primaryColor, isDark.value ? 0.12 : 0.06),
  '--home-primary-border': withAlpha(vars.value.primaryColor, 0.48),
  '--home-primary': vars.value.primaryColor,
  '--home-muted': vars.value.textColor3,
  '--home-muted-mark': withAlpha(vars.value.textColor3, 0.38),
  '--home-success-bg': withAlpha(vars.value.successColor, 0.1),
  '--home-warning-bg': withAlpha(vars.value.warningColor, 0.1),
}))
const homeRefreshing = computed(
  () =>
    ovLoading.value ||
    positionsLoading.value ||
    mineTodoLoading.value ||
    watchLoading.value ||
    mineRecLoading.value ||
    reportLoading.value ||
    tasksLoading.value ||
    loading.value,
)

async function refreshHome() {
  await Promise.allSettled([loadPersonalWorkspace(), loadOverview(), loadStock()])
}

// 盘中自动刷新：60s，仅交易时段 + 页面可见（见 useAutoRefresh），静默保留旧值。
useAutoRefresh(() => {
  void loadOverview(true)
  void loadPositions(true)
  void loadTodos(true)
  void loadWatchlists(true)
  void loadRecentTasks(true)
  void loadStock(true)
}, 60_000)

async function loadStock(silent = false) {
  if (!symbol.value.trim()) {
    if (!silent) message.warning('请输入股票代码')
    return
  }
  const seq = ++stockLoadSeq
  const sym = symbol.value.trim()
  if (!silent || !quote.value) loading.value = true
  quoteError.value = ''
  barsError.value = ''

  const [quoteResult, barsResult, valuationResult] = await Promise.allSettled([
    getQuote('cn', sym),
    getDailyBars('cn', sym, 120),
    getValuation('cn', sym),
  ])
  if (seq !== stockLoadSeq) return

  if (quoteResult.status === 'fulfilled') {
    quote.value = quoteResult.value
    valuation.value = valuationResult.status === 'fulfilled' ? valuationResult.value : null
    if (barsResult.status === 'fulfilled') {
      lastBars.value = barsResult.value
      await nextTick()
      if (seq === stockLoadSeq) renderChart(barsResult.value)
    } else {
      barsError.value = errorText(barsResult.reason, '日线数据暂不可用')
      lastBars.value = []
      chart?.clear()
    }
  } else {
    quoteError.value = errorText(quoteResult.reason, '行情暂不可用')
    if (quote.value?.symbol !== sym) barsError.value = '本次查询未完成，继续显示上一只股票的最近已知结果'
  }
  if (seq === stockLoadSeq) loading.value = false
}

function renderChart(bars: Bar[]) {
  if (!chartEl.value) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  // 涨红跌绿取自主题语义色，坐标轴/背景交给 echarts 主题跟随明暗。
  const up = vars.value.errorColor
  const down = vars.value.successColor
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    grid: { left: 52, right: 16, top: 16, bottom: 36 },
    xAxis: { type: 'category', data: bars.map((b) => b.trade_date), boundaryGap: false },
    yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
    series: [
      {
        type: 'candlestick',
        data: bars.map((b) => [b.open, b.close, b.low, b.high]),
        itemStyle: { color: up, color0: down, borderColor: up, borderColor0: down },
      },
    ],
  })
}

watch(isDark, () => {
  if (lastBars.value.length) renderChart(lastBars.value)
})

// ---------- 展示辅助 ----------
function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtAmount(n: number) {
  if (!n) return '-'
  return (n / 1e8).toFixed(2) + ' 亿'
}
function fmtVol(n: number) {
  if (!n) return '-'
  return n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万手' : n + ' 手'
}
function fmtTime(t: string | undefined) {
  return t ? new Date(t).toLocaleTimeString('zh-CN', { hour12: false }) : '-'
}
// 元 → 亿元，带符号（资金净流入正负）
function fmtYi(n: number | undefined) {
  if (n == null) return '-'
  const yi = n / 1e8
  return (yi >= 0 ? '+' : '') + yi.toFixed(2) + ' 亿'
}

function sourceText(items: Array<{ source?: string }>) {
  const sources = [...new Set(items.map((item) => item.source).filter(Boolean))]
  return sources.join(' / ') || 'unknown'
}

const sectorsUnavailable = computed(() => !!overview.value?.errors?.sectors)
const breadthUnavailable = computed(() => !!overview.value && !overview.value.breadth)
const fundFlowUnavailable = computed(() => !!overview.value && !overview.value.fund_flow)

// 涨跌家数占比（市场情绪条宽度）
const breadthTotal = computed(() => {
  const b = overview.value?.breadth
  return b ? b.advances + b.declines + b.unchanged : 0
})
function breadthPct(n: number) {
  return breadthTotal.value ? (n / breadthTotal.value) * 100 : 0
}

let clockTimer: number | undefined
onMounted(() => {
  void loadOverview()
  void loadPersonalWorkspace()
  void loadStock()
  clockTimer = window.setInterval(() => {
    now.value = new Date()
  }, 60_000)
  window.addEventListener('resize', onResize)
})
onUnmounted(() => {
  stockLoadSeq++
  if (clockTimer !== undefined) window.clearInterval(clockTimer)
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
})
function onResize() {
  chart?.resize()
}
</script>

<template>
  <PageContainer title="我的今日" subtitle="A 股个人工作台 · 与我有关优先，市场全景在第二层">
    <template #actions>
      <n-tag
        size="small"
        round
        :bordered="false"
        :type="modePreference === 'auto' && !autoSession.confirmed ? 'warning' : 'info'"
      >
        {{ modeStatusText }}
      </n-tag>
      <n-button size="small" secondary :loading="homeRefreshing" @click="refreshHome">刷新</n-button>
    </template>

    <div class="dashboard" :style="homeVars">
      <SectionCard title="与我有关" :hoverable="false" class="personal-workspace">
        <div class="mode-toolbar">
          <div class="mode-current">
            <strong>{{ modeFocusText }}</strong>
            <span>
              上海 {{ autoSession.clock.date }} {{ autoSession.clock.time }} · 市场状态
              {{ marketStateLabel(autoSession.marketState) }} · 来源 行情新鲜度契约 · captured_at
              {{ quoteError ? 'unknown' : quote?.freshness?.captured_at || 'unknown' }}
            </span>
          </div>
          <n-radio-group v-model:value="modePreference" size="small" class="mode-switch">
            <n-radio-button value="auto">自动</n-radio-button>
            <n-radio-button value="pre">盘前</n-radio-button>
            <n-radio-button value="intraday">盘中</n-radio-button>
            <n-radio-button value="post">盘后</n-radio-button>
          </n-radio-group>
        </div>

        <n-alert
          v-if="modePreference === 'auto' && !autoSession.confirmed"
          type="warning"
          :bordered="false"
          :show-icon="false"
          class="mode-unknown"
        >
          交易日状态 unknown；当前仅按 Asia/Shanghai 时间采用{{ modeLabel(effectiveMode) }}排序，不据周末或工作日推断开休市。
        </n-alert>

        <div class="workspace-grid">
          <HomeWorkspaceSection
            v-for="(section, index) in orderedSections"
            :key="section"
            :title="sectionTitle(section)"
            :summary="sectionSummary(section)"
            :expanded="expandedSections.includes(section)"
            :primary="index < 2"
            @toggle="toggleWorkspaceSection(section)"
          >
            <template #actions>
              <n-button
                v-if="section === 'todos'"
                size="tiny"
                quaternary
                :loading="mineTodoLoading"
                @click="loadTodos()"
                >重试</n-button
              >
              <n-button
                v-else-if="section === 'positions'"
                size="tiny"
                quaternary
                :loading="positionsLoading"
                @click="loadPositions()"
                >重试</n-button
              >
              <n-button
                v-else-if="section === 'watchlist'"
                size="tiny"
                quaternary
                :loading="watchLoading"
                @click="loadWatchlists()"
                >重试</n-button
              >
              <n-button
                v-else
                size="tiny"
                quaternary
                :loading="tasksLoading || reportLoading || mineRecLoading"
                @click="refreshResearch"
                >重试</n-button
              >
            </template>

            <template v-if="section === 'todos'">
              <n-spin :show="mineTodoLoading && !mineTodo">
                <n-alert
                  v-if="mineTodoError"
                  :type="mineTodo ? 'warning' : 'error'"
                  :bordered="false"
                  :show-icon="false"
                  class="inline-state"
                >
                  {{ mineTodo ? '清单读取不完整，以下保留最近已知事项：' : '待办清单读取失败：' }}{{ mineTodoError }}。来源 今日待办账本 · as_of {{ mineTodo?.date || 'unknown' }}。
                </n-alert>
                <div v-if="priorityTodos.length" class="work-list">
                  <button
                    v-for="item in priorityTodos.slice(0, 4)"
                    :key="`${item.kind}-${item.ref_id}`"
                    type="button"
                    class="work-row"
                    @click="openTodo(item)"
                  >
                    <span class="work-main">
                      <strong>{{ item.title }}</strong>
                      <small>{{ item.detail || item.name || item.symbol }}</small>
                    </span>
                    <span class="work-side">
                      <n-tag v-if="item.kind === 'alert'" size="tiny" type="warning" :bordered="false">提醒</n-tag>
                      <span v-if="item.time" class="qv-tnum">{{ formatTaskCompactTime(item.time) }}</span>
                    </span>
                  </button>
                  <div class="source-line">
                    来源 今日待办账本（ledger） · as_of {{ mineTodo?.date || 'unknown' }}<template v-if="!mineTodo?.complete"> · 最近已知，完整性 unknown</template>
                  </div>
                </div>
                <div v-else-if="mineTodoLoaded && mineTodo?.complete" class="calm-state">
                  <strong>今日账本待办已全部处理完成</strong>
                  <span>来源 今日待办账本 · as_of {{ mineTodo.date }}</span>
                  <n-button size="small" secondary @click="router.push('/today')">查看已完成</n-button>
                </div>
                <n-empty v-else-if="mineTodoLoaded && !mineTodoError" size="small" description="待办状态 unknown" />
                <div class="section-footer">
                  <n-button size="small" text type="primary" @click="router.push('/today')">查看全部待办</n-button>
                </div>
              </n-spin>
            </template>

            <template v-else-if="section === 'positions'">
              <n-spin :show="positionsLoading && !positionsLoaded">
                <n-alert
                  v-if="positionsError"
                  :type="holdingPositions.length ? 'warning' : 'error'"
                  :bordered="false"
                  :show-icon="false"
                  class="inline-state"
                >
                  {{ holdingPositions.length ? '刷新失败，继续显示最近已知持仓：' : '持仓读取失败：' }}{{ positionsError }}。来源 持仓列表 · as_of {{ holdingPositions.length ? positionAsOf : 'unknown' }}。
                </n-alert>
                <template v-if="holdingPositions.length">
                  <div class="position-strip">
                    <div>
                      <span>浮动盈亏</span>
                      <strong class="qv-figure" :style="minePos.priced ? { color: pctColor(minePos.pnl) } : undefined">
                        {{ minePos.priced ? fmtSigned(minePos.pnl) : 'unknown' }}
                      </strong>
                      <ChangeTag v-if="minePos.priced" :value="minePos.pct" size="small" />
                    </div>
                    <div>
                      <span>有效定价</span>
                      <strong class="qv-tnum">{{ minePos.priced }}/{{ minePos.n }}</strong>
                    </div>
                    <div>
                      <span>行情缺口</span>
                      <strong class="qv-tnum">{{ quoteGapPositions.length }}</strong>
                    </div>
                  </div>
                  <div class="source-line">来源 持仓账本 + 有效行情 · as_of {{ positionAsOf }}</div>

                  <div v-if="positionRisks.length" class="work-list compact-list">
                    <button
                      v-for="item in positionRisks.slice(0, 3)"
                      :key="item.id"
                      type="button"
                      class="work-row"
                      @click="goDetail(item)"
                    >
                      <span class="work-main">
                        <strong>{{ item.name || item.symbol }} <span class="qv-mono muted">{{ item.symbol }}</span></strong>
                        <small>{{ positionRiskLabels(item).join(' · ') }}</small>
                      </span>
                      <ChangeTag v-if="item.quote_ok" :value="item.day_change_pct" size="small" />
                    </button>
                  </div>
                  <div v-else class="calm-inline">当前接口未返回需处理的持仓风险标记</div>

                  <div v-if="quoteGapPositions.length" class="gap-list">
                    <div v-for="item in quoteGapPositions.slice(0, 3)" :key="item.id" class="gap-row">
                      <span><strong>{{ item.name || item.symbol }}</strong> · 行情不可用于盈亏或风险结论</span>
                      <span v-if="item.last_price" class="qv-tnum">最近已知价 {{ fmt(item.last_price) }}</span>
                      <FreshnessTag
                        :status="item.freshness_status || 'unknown'"
                        :as-of="item.quote_as_of"
                        :reason="item.stale_reason"
                      />
                      <small>来源 持仓列表（具体行情源未返回） · as_of {{ item.quote_as_of || 'unknown' }} · 最近已知语义</small>
                    </div>
                  </div>
                </template>
                <div v-else-if="positionsLoaded && !positionsError" class="action-empty">
                  <strong>还没有持仓记录</strong>
                  <span>记录真实成交或导入现有账本后，这里才会计算盈亏与风险。</span>
                  <div class="empty-actions">
                    <n-button size="small" type="primary" @click="router.push({ name: 'positions', query: { add: '1' } })">记录第一笔</n-button>
                    <n-button size="small" secondary @click="router.push({ name: 'positions', query: { import: '1' } })">导入持仓</n-button>
                  </div>
                </div>
                <div class="section-footer">
                  <n-button size="small" text type="primary" @click="router.push('/positions')">打开持仓账本</n-button>
                </div>
              </n-spin>
            </template>

            <template v-else-if="section === 'watchlist'">
              <n-spin :show="watchLoading && !watchLoaded">
                <n-alert
                  v-if="watchError"
                  :type="watchItems.length ? 'warning' : 'error'"
                  :bordered="false"
                  :show-icon="false"
                  class="inline-state"
                >
                  {{ watchItems.length ? '刷新失败，继续显示最近已知自选：' : '自选读取失败：' }}{{ watchError }}。来源 自选列表 · as_of {{ newestAsOf(watchItems.map((item) => item.data_time)) }}。
                </n-alert>
                <div v-if="focusedWatchItems.length" class="work-list">
                  <button
                    v-for="item in focusedWatchItems.slice(0, 4)"
                    :key="item.id"
                    type="button"
                    class="work-row"
                    @click="goDetail(item)"
                  >
                    <span class="work-main">
                      <strong>{{ item.name || item.symbol }} <span class="qv-mono muted">{{ item.symbol }}</span></strong>
                      <small>
                        {{ item.quote_ok ? `行情 ${fmt(item.price)}` : `最近已知价 ${item.price ? fmt(item.price) : 'unknown'}` }}
                        · 来源 自选列表（具体行情源未返回） · as_of {{ item.data_time || 'unknown' }}
                      </small>
                    </span>
                    <span class="work-side">
                      <ChangeTag v-if="item.quote_ok" :value="item.change_pct" size="small" />
                      <FreshnessTag v-else :status="item.freshness_status || 'unknown'" :as-of="item.data_time" />
                    </span>
                  </button>
                </div>
                <div v-else-if="watchLoaded && !watchItems.length && !watchError" class="action-empty">
                  <strong>还没有自选股</strong>
                  <span>从全局搜索添加第一只自选。</span>
                  <n-button size="small" type="primary" @click="openGlobalSearch">打开全局搜索</n-button>
                </div>
                <div v-else-if="watchLoaded && watchItems.length && !watchError" class="action-empty">
                  <strong>还没有重点自选</strong>
                  <span>已有 {{ watchItems.length }} 只普通自选，设为重点后会在这里优先展示。</span>
                  <n-button size="small" secondary @click="router.push('/watchlist')">管理重点自选</n-button>
                </div>
                <div class="section-footer">
                  <n-button size="small" text type="primary" @click="router.push('/watchlist')">查看全部自选</n-button>
                </div>
              </n-spin>
            </template>

            <template v-else>
              <n-spin :show="(tasksLoading && !tasksLoaded) || (reportLoading && !reportLoaded)">
                <n-alert v-if="tasksError" type="warning" :bordered="false" :show-icon="false" class="inline-state">
                  最近任务读取失败：{{ tasksError }}。来源 统一任务中心 · as_of unknown。
                </n-alert>
                <div v-if="recentTasks.length" class="work-list compact-list">
                  <button
                    v-for="task in recentTasks.slice(0, 2)"
                    :key="task.id"
                    type="button"
                    class="work-row"
                    :disabled="!taskResultRoute(task)"
                    @click="openTask(task)"
                  >
                    <span class="work-main">
                      <strong>{{ task.title || taskKindLabel(task.kind) }}</strong>
                      <small>来源 统一任务中心 · {{ formatTaskCompactTime(task.created_at) }}</small>
                    </span>
                    <n-tag size="tiny" :type="taskTagType(task.status)" :bordered="false">{{ taskStatusLabel(task.status) }}</n-tag>
                  </button>
                </div>

                <n-alert v-if="reportError" type="warning" :bordered="false" :show-icon="false" class="inline-state">
                  日报读取或生成失败：{{ reportError }}。<template v-if="latestReport">继续显示 {{ latestReport.trade_date }} 的最近已知日报。</template><template v-else>来源 收盘日报 · as_of unknown。</template>
                </n-alert>
                <div v-if="latestReport?.review" class="research-result">
                  <div class="result-head">
                    <strong>{{ latestReport.trade_date }} 收盘日报</strong>
                    <span>{{ llmLabel(latestReport) || '模型信息未记录' }}</span>
                  </div>
                  <p>{{ latestReport.review.summary }}</p>
                  <p v-if="latestReport.review.tomorrow_plan"><b>次日计划：</b>{{ latestReport.review.tomorrow_plan }}</p>
                  <div class="source-line">来源 收盘日报 · as_of {{ latestReport.trade_date }}</div>
                </div>
                <div v-else-if="latestReport" class="calm-inline">
                  日报任务 {{ latestReport.status }} · 来源 收盘日报 · as_of {{ latestReport.trade_date }}
                </div>
                <div v-else-if="reportLoaded && !reportError" class="action-empty compact-empty">
                  <strong>还没有收盘日报</strong>
                  <div class="empty-actions">
                    <n-button size="small" type="primary" :loading="reportGenerating" @click="generateReportOnce">生成一次</n-button>
                    <n-button size="small" secondary @click="router.push({ name: 'settings', query: { tab: 'pref' } })">进入日报设置</n-button>
                  </div>
                </div>

                <button v-if="mineRec" type="button" class="latest-rec" @click="router.push('/recommendations')">
                  <span><strong>最新推荐</strong> · {{ recTypeText(mineRec.type) }}策略 · {{ mineRec.strategy }}</span>
                  <small>来源 推荐追踪 · {{ relDay(mineRec.created_at) }}</small>
                </button>
                <n-alert v-else-if="mineRecError" type="warning" :bordered="false" :show-icon="false" class="inline-state">
                  推荐结果读取失败：{{ mineRecError }}。来源 推荐追踪 · as_of unknown。
                </n-alert>
                <div class="section-footer split-footer">
                  <n-button size="small" text type="primary" @click="router.push('/tasks')">全部任务</n-button>
                  <n-button size="small" text type="primary" @click="router.push('/daily-report')">收盘日报</n-button>
                </div>
              </n-spin>
            </template>
          </HomeWorkspaceSection>
        </div>
      </SectionCard>

      <div class="layer-heading">
        <div>
          <strong>市场全景</strong>
          <span>指数、榜单、资金与通用行情</span>
        </div>
        <span v-if="overview" class="qv-tnum">采集于 {{ fmtTime(overview.data_time) }}</span>
      </div>
      <n-alert
        v-if="ovError"
        :type="overview ? 'warning' : 'error'"
        :bordered="false"
        :show-icon="false"
      >
        {{ overview ? `刷新失败，以下继续显示采集于 ${fmtTime(overview.data_time)} 的最近已知市场概览：` : '市场概览读取失败：' }}{{ ovError }}。
        <n-button size="tiny" text type="primary" @click="loadOverview()">重新读取</n-button>
      </n-alert>

      <!-- 指数概览 -->
      <SectionCard title="指数概览">
        <n-spin :show="ovLoading && !overview">
          <template v-if="overview?.indices?.length">
            <n-grid
              cols="2 s:3 l:4"
              :x-gap="14"
              :y-gap="14"
              responsive="screen"
            >
              <n-gi v-for="ix in overview.indices" :key="ix.code">
                <StatCard :label="ix.name" :value="fmt(ix.price)" :change-pct="ix.change_pct" />
              </n-gi>
            </n-grid>
            <div class="source-line">
              来源 {{ sourceText(overview.indices) }} · as_of {{ newestAsOf(overview.indices.map((item) => item.data_time)) }}
            </div>
          </template>
          <n-empty v-else description="指数数据暂不可用" />
        </n-spin>
      </SectionCard>

      <!-- 涨幅榜 + 热门榜 -->
      <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
        <n-gi>
          <SectionCard title="涨幅榜">
            <template v-if="overview?.gainers?.length">
              <RankList :items="overview.gainers">
                <template #row="{ item }">
                  <div class="stock-row stock-row-link" @click="goDetail({ symbol: item.symbol, market: 'cn', name: item.name })">
                    <div class="sr-name">
                      <span class="sr-title">{{ item.name }}</span>
                      <span class="sr-symbol qv-mono">{{ item.symbol }}</span>
                    </div>
                    <div class="sr-figures">
                      <span class="sr-price qv-tnum">{{ fmt(item.price) }}</span>
                      <ChangeTag :value="item.change_pct" size="small" />
                      <span class="sr-amount qv-tnum">{{ fmtAmount(item.amount) }}</span>
                    </div>
                  </div>
                </template>
              </RankList>
              <div class="source-line">来源 {{ sourceText(overview.gainers) }} · 采集于 {{ fmtTime(overview.data_time) }}</div>
            </template>
            <n-empty v-else description="暂不可用" />
          </SectionCard>
        </n-gi>
        <n-gi>
          <SectionCard title="热门榜（成交额）">
            <template v-if="overview?.actives?.length">
              <RankList :items="overview.actives">
                <template #row="{ item }">
                  <div class="stock-row stock-row-link" @click="goDetail({ symbol: item.symbol, market: 'cn', name: item.name })">
                    <div class="sr-name">
                      <span class="sr-title">{{ item.name }}</span>
                      <span class="sr-symbol qv-mono">{{ item.symbol }}</span>
                    </div>
                    <div class="sr-figures">
                      <span class="sr-price qv-tnum">{{ fmt(item.price) }}</span>
                      <ChangeTag :value="item.change_pct" size="small" />
                      <span class="sr-amount qv-tnum">{{ fmtAmount(item.amount) }}</span>
                    </div>
                  </div>
                </template>
              </RankList>
              <div class="source-line">来源 {{ sourceText(overview.actives) }} · 采集于 {{ fmtTime(overview.data_time) }}</div>
            </template>
            <n-empty v-else description="暂不可用" />
          </SectionCard>
        </n-gi>
      </n-grid>

      <!-- 板块榜 + 市场情绪 -->
      <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
        <n-gi>
          <SectionCard title="板块涨跌榜">
            <template v-if="sectorsUnavailable" #extra>
              <n-tag size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
            </template>
            <template v-if="overview?.sectors?.length">
              <RankList :items="overview.sectors">
                <template #row="{ item }">
                  <div class="stock-row">
                    <span class="sr-title">{{ item.name }}</span>
                    <div class="sr-figures">
                      <ChangeTag :value="item.change_pct" size="small" />
                      <span class="sr-leader">领涨 {{ item.leader || '-' }}</span>
                    </div>
                  </div>
                </template>
              </RankList>
              <div class="source-line">来源 {{ sourceText(overview.sectors) }} · 采集于 {{ fmtTime(overview.data_time) }}</div>
            </template>
            <n-empty v-else description="板块榜依赖东财接口，当前限流暂不可用，稍后重试" />
          </SectionCard>
        </n-gi>
        <n-gi>
          <SectionCard title="市场情绪">
            <template #extra>
              <div class="card-actions">
                <n-tag v-if="breadthUnavailable" size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
                <n-button size="tiny" quaternary type="primary" @click="router.push('/mood')">查看盘面</n-button>
              </div>
            </template>
            <div v-if="overview?.breadth" class="breadth">
              <div class="breadth-summary">
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: upColor }">{{
                    overview.breadth.advances
                  }}</span>
                  <span class="bs-label">上涨</span>
                </div>
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: flatColor }">{{
                    overview.breadth.unchanged
                  }}</span>
                  <span class="bs-label">平盘</span>
                </div>
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: downColor }">{{
                    overview.breadth.declines
                  }}</span>
                  <span class="bs-label">下跌</span>
                </div>
              </div>
              <div class="breadth-bar">
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.advances) + '%',
                    background: upColor,
                  }"
                ></div>
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.unchanged) + '%',
                    background: flatColor,
                  }"
                ></div>
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.declines) + '%',
                    background: downColor,
                  }"
                ></div>
              </div>
              <div class="breadth-limits">
                <span class="bl" :style="{ background: withAlpha(upColor, 0.14), color: upColor }">
                  涨停 {{ overview.breadth.limit_up }}
                </span>
                <span
                  class="bl"
                  :style="{ background: withAlpha(downColor, 0.14), color: downColor }"
                >
                  跌停 {{ overview.breadth.limit_down }}
                </span>
                <span class="bl-date">{{ overview.breadth.trade_date }}</span>
              </div>
              <div class="source-line">
                来源 {{ overview.breadth.source || 'unknown' }} · as_of {{ overview.breadth.data_time || overview.breadth.trade_date || 'unknown' }}
              </div>
            </div>
            <n-empty v-else description="涨跌家数依赖东财接口，当前限流暂不可用，稍后重试" />
          </SectionCard>
        </n-gi>
      </n-grid>

      <!-- 个股速查 -->
      <SectionCard title="个股速查">
        <template #extra>
          <span class="hint">东财 → 腾讯 → 新浪 三源自动切换 · 仅 A 股已打通</span>
        </template>
        <div class="quote-search">
          <n-input
            v-model:value="symbol"
            placeholder="股票代码，如 600000"
            style="width: 200px"
            @keyup.enter="loadStock()"
          />
          <n-button type="primary" :loading="loading" @click="loadStock()">查询</n-button>
        </div>

        <n-alert
          v-if="quoteError"
          :type="quote ? 'warning' : 'error'"
          :bordered="false"
          :show-icon="false"
          class="inline-state"
        >
          {{ quote ? `查询失败，继续显示 ${quote.name || quote.symbol} 的最近已知结果：` : '行情读取失败：' }}{{ quoteError }}。
          来源 {{ quote?.source || '行情聚合' }} · as_of {{ quote?.freshness?.source_data_time || quote?.data_time || 'unknown' }}。
        </n-alert>
        <n-alert v-if="barsError" type="warning" :bordered="false" :show-icon="false" class="inline-state">
          {{ barsError }}。日线来源 unknown · as_of unknown。
        </n-alert>

        <div v-if="quote" class="quote-panel">
          <div class="quote-context">
            <strong>{{ quote.name || quote.symbol }} <span class="qv-mono">{{ quote.symbol }}</span></strong>
            <span>{{ quote.freshness?.freshness_status === 'fresh' ? '行情' : '最近已知行情' }}</span>
          </div>
          <div class="quote-hero">
            <span class="qh-price qv-figure" :style="{ color: pctColor(quote.change_pct) }">
              {{ fmt(quote.price) }}
            </span>
            <ChangeTag :value="quote.change_pct" />
            <FreshnessTag
              :status="quote.freshness?.freshness_status"
              :as-of="quote.freshness?.source_data_time"
              :reason="quote.freshness?.stale_reason"
            />
          </div>
          <div class="source-line quote-source">
            来源 {{ quote.freshness?.source || quote.source || 'unknown' }} · as_of
            {{ quote.freshness?.source_data_time || quote.data_time || 'unknown' }}<template v-if="quote.freshness?.freshness_status !== 'fresh'"> · 最近已知语义</template>
          </div>
          <div class="quote-grid">
            <div class="quote-cell">
              <span class="qc-label">今开</span>
              <span class="qc-value qv-tnum">{{ fmt(quote.open) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">昨收</span>
              <span class="qc-value qv-tnum">{{ fmt(quote.prev_close) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">最高</span>
              <span class="qc-value qv-tnum" :style="{ color: upColor }">{{ fmt(quote.high) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">最低</span>
              <span class="qc-value qv-tnum" :style="{ color: downColor }">{{ fmt(quote.low) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">成交量</span>
              <span class="qc-value qv-tnum">{{ fmtVol(quote.volume) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">成交额</span>
              <span class="qc-value qv-tnum">{{ fmtAmount(quote.amount) }}</span>
            </div>
          </div>

          <!-- 估值快照（腾讯免费源，best-effort） -->
          <div v-if="valuation && valuation.symbol === quote.symbol" class="quote-grid valuation-grid">
            <div class="quote-cell">
              <span class="qc-label">PE-TTM</span>
              <span class="qc-value qv-tnum">{{ valuation.pe_ttm < 0 ? '亏损' : fmt(valuation.pe_ttm) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">市净率</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.pb) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">总市值</span>
              <span class="qc-value qv-tnum">{{ valuation.total_cap > 0 ? (valuation.total_cap / 1e8).toFixed(0) + ' 亿' : '—' }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">换手率</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.turnover_rate) }}%</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">量比</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.volume_ratio) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">涨停 / 跌停</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.limit_up) }} / {{ fmt(valuation.limit_down) }}</span>
            </div>
            <div class="source-line valuation-source">来源 {{ valuation.source || 'unknown' }} · as_of {{ valuation.data_time || 'unknown' }}</div>
          </div>

          <!-- 快捷动作：查到即可直达，不用换页面重输代码 -->
          <div class="quote-actions">
            <n-button size="small" secondary type="primary" @click="goDetail(quote)">个股详情</n-button>
            <n-button size="small" secondary @click="goAnalysis(quote)">AI 分析</n-button>
            <n-button size="small" secondary @click="goQa(quote)">个股问答</n-button>
            <n-button size="small" secondary @click="goCompare(quote)">横向对比</n-button>
            <n-button size="small" secondary :loading="adding" @click="addToWatchlist(quote)">+ 自选</n-button>
            <n-button size="small" secondary @click="goAlert(quote)">设提醒</n-button>
          </div>
        </div>

        <div v-show="lastBars.length" ref="chartEl" class="quote-chart"></div>
      </SectionCard>

      <!-- AI 今日观点已上移到个人工作台；全市场资金仍完整保留在第二层。 -->
      <SectionCard title="资金流向">
        <template v-if="fundFlowUnavailable" #extra>
          <n-tag size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
        </template>
        <div v-if="overview?.fund_flow" class="fundflow">
          <div class="ff-hero">
            <span class="ff-label">主力净流入</span>
            <span
              class="ff-main qv-figure"
              :style="{ color: pctColor(overview.fund_flow.main_net) }"
            >
              {{ fmtYi(overview.fund_flow.main_net) }}
            </span>
            <span class="ff-date">{{ overview.fund_flow.trade_date }} · 沪深两市</span>
          </div>
          <div class="ff-grid">
            <div class="ff-cell">
              <span class="ff-k">超大单</span>
              <span
                class="ff-v qv-tnum"
                :style="{ color: pctColor(overview.fund_flow.super_net) }"
                >{{ fmtYi(overview.fund_flow.super_net) }}</span
              >
            </div>
            <div class="ff-cell">
              <span class="ff-k">大单</span>
              <span
                class="ff-v qv-tnum"
                :style="{ color: pctColor(overview.fund_flow.large_net) }"
                >{{ fmtYi(overview.fund_flow.large_net) }}</span
              >
            </div>
            <div class="ff-cell">
              <span class="ff-k">中单</span>
              <span
                class="ff-v qv-tnum"
                :style="{ color: pctColor(overview.fund_flow.medium_net) }"
                >{{ fmtYi(overview.fund_flow.medium_net) }}</span
              >
            </div>
            <div class="ff-cell">
              <span class="ff-k">小单</span>
              <span
                class="ff-v qv-tnum"
                :style="{ color: pctColor(overview.fund_flow.small_net) }"
                >{{ fmtYi(overview.fund_flow.small_net) }}</span
              >
            </div>
          </div>
          <div class="source-line">
            来源 {{ overview.fund_flow.source || 'unknown' }} · as_of {{ overview.fund_flow.data_time || overview.fund_flow.trade_date || 'unknown' }}
          </div>
        </div>
        <n-empty v-else description="两市资金流依赖东财接口，当前限流暂不可用，稍后重试" />
      </SectionCard>

      <n-alert type="warning" title="风险提示" :bordered="false">
        本内容仅供研究参考，不构成投资建议。AI 可能出错，数据可能延迟或不完整，投资决策需由用户自行承担风险。
      </n-alert>
    </div>
  </PageContainer>
</template>

<style scoped>
.dashboard {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.card-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}

/* 个人工作台 */
.mode-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 12px;
}
.personal-workspace,
.personal-workspace :deep(.n-card__content) {
  min-width: 0;
}
.mode-current {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.mode-current strong {
  font-size: 14px;
  line-height: 1.45;
}
.mode-current span {
  color: var(--home-muted);
  font-size: 12px;
  line-height: 1.45;
}
.mode-switch {
  flex: 0 0 auto;
  white-space: nowrap;
}
.mode-unknown {
  margin-bottom: 12px;
}
.workspace-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 12px;
  align-items: start;
}
.inline-state {
  margin-top: 10px;
  font-size: 12px;
}
.work-list {
  display: flex;
  flex-direction: column;
}
.work-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
  min-height: 52px;
  padding: 9px 2px;
  border: 0;
  border-bottom: 1px solid var(--home-divider);
  color: inherit;
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.work-row:hover {
  background: var(--home-soft-bg);
}
.work-row:focus-visible,
.latest-rec:focus-visible {
  outline: 2px solid var(--home-primary);
  outline-offset: 1px;
}
.work-row:disabled {
  cursor: default;
  opacity: 0.7;
}
.work-main {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.work-main strong {
  overflow: hidden;
  font-size: 13px;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.work-main small,
.work-side,
.source-line,
.gap-row small,
.latest-rec small {
  color: var(--home-muted);
  font-size: 11px;
  line-height: 1.45;
}
.work-main small {
  display: -webkit-box;
  overflow: hidden;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}
.work-side {
  display: flex;
  align-items: flex-end;
  gap: 5px;
  flex: 0 0 auto;
  flex-direction: column;
}
.source-line {
  margin-top: 8px;
}
.muted {
  color: var(--home-muted);
  font-size: 11px;
  font-weight: 400;
}
.calm-state,
.action-empty {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  justify-content: center;
  gap: 8px;
  min-height: 126px;
  padding: 14px 2px;
}
.calm-state {
  padding-left: 12px;
  border-left: 3px solid var(--home-primary);
  background: var(--home-success-bg);
}
.calm-state span,
.action-empty span {
  color: var(--home-muted);
  font-size: 12px;
  line-height: 1.5;
}
.compact-empty {
  min-height: 76px;
}
.empty-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.section-footer {
  display: flex;
  justify-content: flex-end;
  padding-top: 9px;
}
.split-footer {
  justify-content: space-between;
}
.position-strip {
  display: grid;
  grid-template-columns: 1.5fr 1fr 1fr;
  padding: 12px 0;
  border-bottom: 1px solid var(--home-divider);
}
.position-strip > div {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 4px;
  min-width: 0;
  padding: 0 10px;
  border-left: 1px solid var(--home-divider);
}
.position-strip > div:first-child {
  padding-left: 2px;
  border-left: 0;
}
.position-strip span {
  color: var(--home-muted);
  font-size: 11px;
}
.position-strip strong {
  overflow-wrap: anywhere;
  font-size: 18px;
  line-height: 1.2;
}
.compact-list {
  margin-top: 4px;
}
.calm-inline {
  margin-top: 9px;
  padding: 9px 10px;
  border-left: 3px solid var(--home-primary);
  background: var(--home-success-bg);
  font-size: 12px;
}
.gap-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 10px;
  padding: 8px 10px;
  background: var(--home-warning-bg);
}
.gap-row {
  display: flex;
  align-items: center;
  gap: 6px 10px;
  flex-wrap: wrap;
  font-size: 12px;
}
.gap-row small {
  flex-basis: 100%;
}
.research-result {
  margin-top: 10px;
  padding: 10px 12px;
  border-left: 3px solid var(--home-primary);
  background: var(--home-soft-bg);
}
.result-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}
.result-head span {
  color: var(--home-muted);
  font-size: 11px;
}
.research-result p {
  margin: 7px 0 0;
  font-size: 12px;
  line-height: 1.55;
}
.latest-rec {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  width: 100%;
  margin-top: 8px;
  padding: 9px 2px;
  border: 0;
  border-top: 1px solid var(--home-divider);
  border-bottom: 1px solid var(--home-divider);
  color: inherit;
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.layer-heading {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  padding: 8px 2px 0;
}
.layer-heading > div {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.layer-heading strong {
  font-size: 18px;
}
.layer-heading span {
  color: var(--home-muted);
  font-size: 12px;
}

/* 速查快捷动作条 */
.quote-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
}

/* 榜单行 */
.stock-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
}
.stock-row-link {
  cursor: pointer;
}
.sr-name {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.sr-title {
  font-size: 14px;
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sr-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.sr-figures {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-shrink: 0;
}
.sr-price {
  font-size: 14px;
  min-width: 56px;
  text-align: right;
}
.sr-amount {
  font-size: 12px;
  opacity: 0.6;
  min-width: 64px;
  text-align: right;
}
/* 超窄屏：右侧价格+涨跌+成交额三段不可收缩，名称列会被挤到 2~3 字，隐藏次要的成交额 */
@media (max-width: 480px) {
  .sr-figures {
    gap: 8px;
  }
  .sr-amount {
    display: none;
  }
}
.sr-leader {
  font-size: 12px;
  opacity: 0.6;
}

/* 市场情绪：涨跌家数 */
.breadth {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 4px 0;
}
.breadth-summary {
  display: flex;
  justify-content: space-around;
  text-align: center;
}
.bs-cell {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.bs-num {
  font-size: 26px;
  font-weight: 700;
  line-height: 1;
}
.bs-label {
  font-size: 12px;
  opacity: 0.6;
}
.breadth-bar {
  display: flex;
  height: 10px;
  border-radius: 6px;
  overflow: hidden;
}
.breadth-bar .seg {
  height: 100%;
  transition: width 0.4s ease;
}
.breadth-limits {
  display: flex;
  align-items: center;
  gap: 10px;
}
.bl {
  font-size: 12px;
  font-weight: 600;
  padding: 2px 10px;
  border-radius: 999px;
}
.bl-date {
  font-size: 12px;
  opacity: 0.45;
  margin-left: auto;
}

/* 资金流向 */
.fundflow {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.ff-hero {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}
.ff-label {
  font-size: 13px;
  opacity: 0.6;
}
.ff-main {
  font-size: 30px;
  font-weight: 700;
  line-height: 1;
}
.ff-date {
  font-size: 12px;
  opacity: 0.45;
}
.ff-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 10px 16px;
}
.ff-cell {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.ff-k {
  font-size: 12px;
  opacity: 0.55;
}
.ff-v {
  font-size: 16px;
  font-weight: 600;
}

/* 个股速查 */
.hint {
  font-size: 12px;
  opacity: 0.55;
}
.quote-search {
  display: flex;
  gap: 10px;
  margin-bottom: 16px;
}
.quote-panel {
  margin-bottom: 12px;
}
.quote-context {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 8px;
}
.quote-context strong {
  font-size: 14px;
}
.quote-context strong span,
.quote-context > span {
  color: var(--home-muted);
  font-size: 12px;
  font-weight: 400;
}
.quote-hero {
  display: flex;
  align-items: baseline;
  gap: 12px;
  margin-bottom: 14px;
}
.quote-source {
  margin: -6px 0 14px;
}
.qh-price {
  font-size: 34px;
  font-weight: 700;
  line-height: 1;
}
.quote-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
  gap: 10px 16px;
}
.valuation-grid {
  position: relative;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--home-divider);
}
.valuation-source {
  grid-column: 1 / -1;
  margin-top: 0;
}
.quote-cell {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.qc-label {
  font-size: 12px;
  opacity: 0.55;
}
.qc-value {
  font-size: 16px;
  font-weight: 500;
}
.quote-chart {
  width: 100%;
  height: 340px;
  margin-top: 8px;
}

@media (max-width: 768px) {
  .personal-workspace :deep(.n-card__content) {
    overflow-x: hidden;
  }
  .mode-toolbar {
    align-items: stretch;
    flex-direction: column;
    gap: 10px;
  }
  .mode-switch {
    display: flex;
    width: 100%;
  }
  .mode-switch :deep(.n-radio-button) {
    min-width: 0;
    flex: 1 1 0;
  }
  .mode-switch :deep(.n-radio-button__label) {
    display: block;
    width: 100%;
    padding: 0 6px;
    text-align: center;
  }
  .workspace-grid {
    grid-template-columns: minmax(0, 1fr);
  }
  .work-row {
    align-items: flex-start;
  }
  .work-main strong {
    white-space: normal;
  }
  .position-strip > div {
    padding: 0 7px;
  }
  .position-strip strong {
    font-size: 16px;
  }
  .latest-rec,
  .layer-heading,
  .layer-heading > div {
    align-items: flex-start;
    flex-direction: column;
    gap: 4px;
  }
  .quote-search {
    align-items: stretch;
  }
  .quote-search :deep(.n-input) {
    width: auto !important;
    min-width: 0;
    flex: 1;
  }
  .quote-hero {
    flex-wrap: wrap;
  }
  .quote-chart {
    height: 280px;
  }
}

@media (max-width: 400px) {
  .position-strip {
    grid-template-columns: 1.25fr 1fr 0.8fr;
  }
  .position-strip > div {
    padding: 0 5px;
  }
  .position-strip > div:first-child {
    padding-left: 0;
  }
  .work-side {
    max-width: 40%;
  }
  .source-line,
  .gap-row small {
    overflow-wrap: anywhere;
  }
}
</style>
