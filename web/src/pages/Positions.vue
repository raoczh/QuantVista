<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed, nextTick, watch, defineAsyncComponent } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NInputNumber,
  NModal,
  NForm,
  NFormItem,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NTabs,
  NTabPane,
  NTag,
  NPopconfirm,
  NEmpty,
  NSpin,
  NGrid,
  NGi,
  NAlert,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  listPositions,
  getPortfolioOverview,
  createPosition,
  updatePosition,
  closePosition,
  deletePosition,
  listPositionTrades,
  addPositionTrade,
  getTradeStats,
  getPositionCurve,
  type Position,
  type PositionInput,
  type PortfolioOverview,
  type PositionTrade,
  type TradeStats,
  type TradeStatBucket,
  type PortfolioCurve,
  type ExposureDim,
  type ExposureBucket,
  listCorpAdjusts,
  actCorpAdjust,
  type PositionCorpAdjust,
  requestPositionAdvice,
  getPositionExitAssessment,
  type PositionAdviceResult,
  type PositionExitAssessment,
  type PositionExitLevel,
  POSITION_VERDICT_LABEL,
} from '@/api/position'
import { getLLMTask, type LLMTask } from '@/api/llmTask'
import { pollUntil } from '@/lib/poll'
import { isAbortError } from '@/api/client'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import {
  enumQuery,
  integerEnumQuery,
  integerQuery,
  queryRef,
  useListPageScroll,
  useRouteQueryState,
} from '@/composables/useListPageState'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'
import DataImportWizard from '@/components/DataImportWizard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import PositionDecisionCenter from '@/components/positions/PositionDecisionCenter.vue'

const PortfolioRisk = defineAsyncComponent(() => import('@/pages/PortfolioRisk.vue'))

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, vars, isDark } = useUi()
const styleVars = computed(() => ({
  '--qv-divider': vars.value.dividerColor,
  '--qv-action-target': withAlpha(vars.value.primaryColor, 0.12),
  '--qv-action-target-line': vars.value.primaryColor,
}))
const warnColor = computed(() => vars.value.warningColor)

const positions = ref<Position[]>([])
const overview = ref<PortfolioOverview | null>(null)
const loading = ref(false)
const loadError = ref('')
const statusQuery = enumQuery<'holding' | 'closed' | 'all'>('holding', ['holding', 'closed', 'all'])
const typeQuery = enumQuery<'all' | 'short_term' | 'long_term'>('all', ['all', 'short_term', 'long_term'])
const statusFilter = ref(statusQuery.parse(route.query.status))
const typeFilter = ref(typeQuery.parse(route.query.type))
let positionLoadSeq = 0
let loadedStatus: typeof statusFilter.value | null = null
let suspendStatusReload = false

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load(silent = false) {
  // 自动刷新不抢占正在进行的手动刷新，否则它可能让手动轮次失效且把错误静默吞掉。
  if (silent && loading.value) return
  const requestedStatus = statusFilter.value
  const mySeq = ++positionLoadSeq
  // 切换状态筛选时先清掉上一范围的持仓，避免请求期间把“持仓中”数据显示在“已卖出”下。
  if (loadedStatus !== requestedStatus) positions.value = []
  if (!silent) loading.value = true
  loadError.value = ''
  try {
    const [list, ov] = await Promise.all([listPositions(requestedStatus), getPortfolioOverview()])
    if (mySeq !== positionLoadSeq || requestedStatus !== statusFilter.value) return
    positions.value = list
    overview.value = ov
    loadedStatus = requestedStatus
    await restoreExpandedTrade()
  } catch (e) {
    if (mySeq === positionLoadSeq) {
      positions.value = []
      overview.value = null
      loadError.value = (e as Error).message
      if (!silent) message.error(loadError.value)
    }
  } finally {
    // 静默自动刷新可能在手动刷新期间接管最新轮次；最新轮次无论是否 silent 都要收起旧 loading。
    if (mySeq === positionLoadSeq) loading.value = false
  }
}

// 盘中自动刷新盈亏（60s，仅交易时段+页面可见，静默）。
useAutoRefresh(() => load(true), 60_000)

watch(statusFilter, () => {
  if (!suspendStatusReload) void load()
})

const filtered = computed(() =>
  typeFilter.value === 'all'
    ? positions.value
    : positions.value.filter((p) => p.position_type === typeFilter.value),
)

// 汇总改为后端组合总览（GET /positions/overview：全组合口径，不随筛选变化）。
const mixLabel = computed(() => {
  const ov = overview.value
  if (!ov || ov.total_value <= 0) return '—'
  const short = (ov.short_value / ov.total_value) * 100
  return `${short.toFixed(0)}% / ${(100 - short).toFixed(0)}%`
})

// 部分估值透明化：行情失败/过期的仓位被排除出市值/盈亏汇总，不能默不作声地伪装成完整组合。
const pricedLabel = computed(() => {
  const ov = overview.value
  if (!ov) return ''
  const failed = ov.quote_failed_count ?? 0
  const stale = ov.quote_stale_count ?? 0
  if (failed + stale <= 0) return ''
  const total = ov.holding_count
  const parts: string[] = []
  if (stale > 0) parts.push(`${stale} 笔行情已过期`)
  if (failed > 0) parts.push(`${failed} 笔行情缺失`)
  return `已定价 ${total - failed - stale}/${total} 笔（${parts.join('、')}，未计入市值与盈亏）`
})

// 持仓行「N 天未分析」提示文案（从未分析则不带天数）。
function staleLabel(p: Position) {
  if (!p.last_analyzed_at) return '未分析'
  const days = Math.floor((Date.now() - new Date(p.last_analyzed_at).getTime()) / 86_400_000)
  return `${days} 天未分析`
}

function typeLabel(t: string) {
  return t === 'short_term' ? '短线' : '长线'
}
function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtMoney(n: number) {
  return (n >= 0 ? '' : '-') + Math.abs(n).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}
function todayStr() {
  return new Date().toLocaleDateString('en-CA') // YYYY-MM-DD 本地
}

// ---------- 建仓 / 编辑 ----------
const editModal = ref(false)
const editing = ref(false)
// 编辑已平仓持仓：后端仅接受 buy_reason/user_note，其余字段隐藏，避免“保存成功”误导。
const editingClosed = ref(false)
const form = ref<PositionInput & { id: number | null }>({
  id: null,
  symbol: '',
  market: 'cn',
  name: '',
  position_type: 'short_term',
  buy_price: undefined,
  buy_date: todayStr(),
  quantity: undefined,
  buy_fee: 0,
  buy_tax: 0,
  buy_reason: '',
  user_note: '',
  plan_stop_loss: undefined,
  plan_take_profit: undefined,
})

// 买入前检查清单（勾选状态随持仓落库，供卖出复盘对照）。
const CHECKLIST = [
  '买入理由已想清楚，能写下来（不是「感觉要涨」）',
  '已设定止损价/失效条件，并能接受对应亏损',
  '该仓位不会让单一标的占比过高',
  '已检查近期事件风险（财报/解禁/减持/停复牌）',
  '当前市场环境不明显逆风（趋势/情绪）',
]
const checklist = ref<boolean[]>(CHECKLIST.map(() => false))
function checklistToJSON(): string {
  if (!checklist.value.some(Boolean)) return ''
  return JSON.stringify({ items: CHECKLIST.map((text, i) => ({ text, checked: checklist.value[i] })) })
}
function checklistFromJSON(s: string) {
  checklist.value = CHECKLIST.map(() => false)
  if (!s) return
  try {
    const parsed = JSON.parse(s) as { items?: { text: string; checked: boolean }[] }
    parsed.items?.forEach((it, i) => {
      if (i < checklist.value.length) checklist.value[i] = !!it.checked
    })
  } catch {
    /* 兼容异常数据：忽略 */
  }
}
const checklistDone = computed(() => checklist.value.filter(Boolean).length)

// 仓位风险计算器：随表单实时计算（纯前端，无请求）。
const riskCalc = computed(() => {
  const price = form.value.buy_price || 0
  const qty = form.value.quantity || 0
  const stop = form.value.plan_stop_loss || 0
  const cost = price * qty + (form.value.buy_fee || 0) + (form.value.buy_tax || 0)
  if (price <= 0 || qty <= 0) return null
  const out: { cost: number; maxLoss: number | null; maxLossPct: number | null; gain: number | null } = {
    cost,
    maxLoss: null,
    maxLossPct: null,
    gain: null,
  }
  if (stop > 0 && stop < price) {
    out.maxLoss = (price - stop) * qty + (form.value.buy_fee || 0) + (form.value.buy_tax || 0)
    out.maxLossPct = (out.maxLoss / cost) * 100
  }
  const tp = form.value.plan_take_profit || 0
  if (tp > price) out.gain = (tp - price) * qty
  return out
})

function openCreate(prefill?: { symbol?: string; market?: string; name?: string; recId?: number; quantity?: number }) {
  editing.value = false
  editingClosed.value = false
  form.value = {
    id: null,
    symbol: prefill?.symbol || '',
    market: prefill?.market || 'cn',
    name: prefill?.name || '',
    position_type: 'short_term',
    buy_price: undefined,
    buy_date: todayStr(),
    quantity: prefill?.quantity,
    buy_fee: 0,
    buy_tax: 0,
    buy_reason: '',
    user_note: '',
    plan_stop_loss: undefined,
    plan_take_profit: undefined,
    recommendation_id: prefill?.recId || 0,
  }
  checklistFromJSON('')
  editModal.value = true
}
function openEdit(p: Position) {
  editing.value = true
  editingClosed.value = p.status === 'closed'
  form.value = {
    id: p.id,
    symbol: p.symbol,
    market: p.market,
    name: p.name,
    position_type: p.position_type,
    buy_price: p.buy_price,
    buy_date: p.buy_date,
    quantity: p.quantity,
    buy_fee: p.buy_fee,
    buy_tax: p.buy_tax,
    buy_reason: p.buy_reason,
    user_note: p.user_note,
    plan_stop_loss: p.plan_stop_loss || undefined,
    plan_take_profit: p.plan_take_profit || undefined,
  }
  checklistFromJSON(p.checklist_json)
  editModal.value = true
}
const submitting = ref(false)
async function submit() {
  if (submitting.value) return
  const f = form.value
  if (!editing.value && !f.symbol?.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (!editingClosed.value) {
    if (!f.buy_price || f.buy_price <= 0) {
      message.warning('请输入买入价格')
      return
    }
    if (!f.quantity || f.quantity <= 0) {
      message.warning('请输入买入数量')
      return
    }
    if (f.plan_stop_loss && f.plan_stop_loss >= f.buy_price) {
      message.warning('计划止损价应低于买入价')
      return
    }
    if (f.plan_take_profit && f.plan_take_profit <= f.buy_price) {
      message.warning('计划止盈价应高于买入价')
      return
    }
  }
  submitting.value = true
  try {
    // 已平仓持仓：后端仅接受 buy_reason/user_note，只提交这两项，避免误导用户以为改了成交数据。
    const payload: PositionInput =
      editing.value && editingClosed.value
        ? { buy_reason: f.buy_reason, user_note: f.user_note }
        : {
            symbol: f.symbol?.trim(),
            market: f.market,
            position_type: f.position_type,
            buy_price: f.buy_price,
            buy_date: f.buy_date,
            quantity: f.quantity,
            buy_fee: f.buy_fee,
            buy_tax: f.buy_tax,
            buy_reason: f.buy_reason,
            user_note: f.user_note,
            plan_stop_loss: f.plan_stop_loss || 0,
            plan_take_profit: f.plan_take_profit || 0,
            checklist_json: checklistToJSON(),
            recommendation_id: f.recommendation_id || 0,
          }
    if (editing.value && f.id) await updatePosition(f.id, payload)
    else await createPosition(payload)
    editModal.value = false
    invalidateAdvice()
    await load()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    submitting.value = false
  }
}

// ---------- 平仓 ----------
const closeModal = ref(false)
const closing = ref<Position | null>(null)
const closeForm = ref({
  sell_price: undefined as number | undefined,
  sell_date: todayStr(),
  sell_fee: 0,
  sell_tax: 0,
  sell_reason: '',
  review_note: '',
  sell_planned: '',
  ai_verdict: '',
  lesson_learned: '',
})
const sellPlannedOptions = [
  { label: '按计划卖出', value: 'yes' },
  { label: '未按计划（冲动/被动）', value: 'no' },
  { label: '部分按计划', value: 'partial' },
]
const aiVerdictOptions = [
  { label: 'AI 判断正确', value: 'right' },
  { label: 'AI 判断错误', value: 'wrong' },
  { label: '对错参半', value: 'mixed' },
  { label: '未参考 AI', value: 'unused' },
]
function openClose(p: Position) {
  closing.value = p
  closeForm.value = {
    sell_price: p.current_price || undefined,
    sell_date: todayStr(),
    sell_fee: 0,
    sell_tax: 0,
    sell_reason: '',
    review_note: '',
    sell_planned: '',
    ai_verdict: '',
    lesson_learned: '',
  }
  closeModal.value = true
}
const closingSubmit = ref(false)
async function submitClose() {
  if (!closing.value || closingSubmit.value) return
  if (!closeForm.value.sell_price || closeForm.value.sell_price <= 0) {
    message.warning('请输入卖出价格')
    return
  }
  closingSubmit.value = true
  try {
    await closePosition(closing.value.id, {
      sell_price: closeForm.value.sell_price,
      sell_date: closeForm.value.sell_date,
      sell_fee: closeForm.value.sell_fee,
      sell_tax: closeForm.value.sell_tax,
      sell_reason: closeForm.value.sell_reason,
      review_note: closeForm.value.review_note,
      sell_planned: closeForm.value.sell_planned,
      ai_verdict: closeForm.value.ai_verdict,
      lesson_learned: closeForm.value.lesson_learned,
    })
    closeModal.value = false
    invalidateAdvice()
    await load()
    message.success('已标记卖出')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    closingSubmit.value = false
  }
}

async function remove(p: Position) {
  try {
    await deletePosition(p.id)
    invalidateAdvice()
    await load()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// 快捷入口：分析/提醒页均已支持 query 预填（PRD 3.3/3.16 的跳转交互）。
function goAnalysis(p: Position) {
  router.push({ name: 'analysis', query: { module: 'stock', symbol: p.symbol, market: p.market } })
}
function goAlert(p: Position) {
  router.push({ name: 'alerts', query: { add: '1', symbol: p.symbol, market: p.market, name: p.name } })
}
function goThesis(p: Position) {
  router.push({ name: 'thesis', query: { add: '1', symbol: p.symbol, market: p.market, name: p.name } })
}

// ---------- U10/P09 统一导入向导 ----------
const importModal = ref(false)
function openImport() {
  importModal.value = true
}
async function onImportChanged(kind?: string) {
  invalidateAdvice()
  await load()
  if (route.query.onboarding_return === '1' && kind === 'position') {
    await router.push({ name: 'home', query: { onboarding: '1' } })
  }
}

// ---------- B5 加仓 / 减仓 + 流水明细 ----------
const tradeModal = ref(false)
const tradeTarget = ref<Position | null>(null)
const tradeSubmitting = ref(false)
const tradeForm = ref({
  side: 'buy' as 'buy' | 'sell',
  price: undefined as number | undefined,
  quantity: undefined as number | undefined,
  fee: 0,
  tax: 0,
  trade_date: todayStr(),
  note: '',
  sell_reason: '',
  review_note: '',
  sell_planned: '',
  ai_verdict: '',
  lesson_learned: '',
})
// 减仓到 0 会自动平仓：此时表单展开复盘字段（与「卖出」弹窗同一套维度）。
const tradeWillClose = computed(() => {
  const p = tradeTarget.value
  if (!p || tradeForm.value.side !== 'sell') return false
  return (tradeForm.value.quantity || 0) >= p.quantity
})

function openTrade(p: Position, side: 'buy' | 'sell') {
  tradeTarget.value = p
  tradeForm.value = {
    side,
    price: p.quote_ok ? p.current_price : undefined,
    quantity: undefined,
    fee: 0,
    tax: 0,
    trade_date: todayStr(),
    note: '',
    sell_reason: '',
    review_note: '',
    sell_planned: '',
    ai_verdict: '',
    lesson_learned: '',
  }
  tradeModal.value = true
}

async function submitTrade() {
  const p = tradeTarget.value
  if (!p || tradeSubmitting.value) return
  const f = tradeForm.value
  if (!f.price || f.price <= 0) {
    message.warning('请输入成交价格')
    return
  }
  if (!f.quantity || f.quantity <= 0) {
    message.warning('请输入成交数量')
    return
  }
  if (f.side === 'sell' && f.quantity > p.quantity) {
    message.warning(`卖出数量超过当前持仓（持有 ${p.quantity}）`)
    return
  }
  tradeSubmitting.value = true
  try {
    await addPositionTrade(p.id, {
      side: f.side,
      price: f.price,
      quantity: f.quantity,
      fee: f.fee,
      tax: f.tax,
      trade_date: f.trade_date,
      note: f.note,
      sell_reason: f.sell_reason,
      review_note: f.review_note,
      sell_planned: f.sell_planned,
      ai_verdict: f.ai_verdict,
      lesson_learned: f.lesson_learned,
    })
    tradeModal.value = false
    invalidateAdvice()
    await load()
    if (expandedTrades.value === p.id) await loadTrades(p.id)
    message.success(f.side === 'buy' ? '已记录加仓' : '已记录减仓')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    tradeSubmitting.value = false
  }
}

// 展开的流水明细（一次只展开一行，避免长列表拉出几十个请求）。
const expandedTrades = ref<number | null>(null)
const tradesById = ref<Record<number, PositionTrade[]>>({})
const tradesErrorById = ref<Record<number, string>>({})
const tradesLoading = ref(false)
let tradesSeq = 0

async function toggleTrades(p: Position) {
  if (expandedTrades.value === p.id) {
    tradesSeq++
    tradesLoading.value = false
    expandedTrades.value = null
    return
  }
  // 只允许当前展开行的请求控制 loading/回填；上一行迟到的响应直接丢弃。
  tradesSeq++
  tradesLoading.value = false
  expandedTrades.value = p.id
  if (!tradesById.value[p.id]) await loadTrades(p.id)
}
async function loadTrades(id: number) {
  const mySeq = ++tradesSeq
  const nextTrades = { ...tradesById.value }
  delete nextTrades[id]
  tradesById.value = nextTrades
  tradesErrorById.value = { ...tradesErrorById.value, [id]: '' }
  tradesLoading.value = true
  try {
    const rows = await listPositionTrades(id)
    if (mySeq !== tradesSeq || expandedTrades.value !== id) return
    tradesById.value = { ...tradesById.value, [id]: rows }
  } catch (e) {
    if (mySeq === tradesSeq) {
      const error = (e as Error).message
      tradesErrorById.value = { ...tradesErrorById.value, [id]: error }
      message.error(error)
    }
  } finally {
    if (mySeq === tradesSeq) tradesLoading.value = false
  }
}
function sideLabel(side: string) {
  if (side === 'buy') return '买入'
  if (side === 'adjust') return '除权折算'
  return '卖出'
}
// 流水方向配色：adjust 不是买卖，用中性色（拿涨跌色会让「折算」看着像一次盈亏）。
function sideColor(side: string) {
  if (side === 'adjust') return undefined
  return pctColor(side === 'buy' ? 1 : -1)
}
// 撤销一笔已确认的折算（入口放在流水里——用户看到那笔 adjust 才会想撤）。
// 后端只在「账面仍等于折算结果且其后无新交易」时接受，否则明确拒绝、不做部分回滚。
const revertingTrade = ref<number | null>(null)
async function revertAdjustTrade(t: PositionTrade, positionId: number) {
  if (!t.adjust_id || revertingTrade.value) return
  revertingTrade.value = t.id
  try {
    await actCorpAdjust(t.adjust_id, 'revert')
    message.success('已撤销该次折算，账本回滚')
    invalidateAdvice()
    await Promise.all([load(), loadTrades(positionId), loadCorpAdjusts()])
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    revertingTrade.value = null
  }
}

// ---------- B6 复盘统计 ----------
const mainTabQuery = enumQuery<'needs_action' | 'all' | 'review' | 'risk'>('needs_action', [
  'needs_action',
  'all',
  'review',
  'risk',
])
const statsRangeQuery = enumQuery<'all' | '30d' | '90d' | '180d' | '1y'>('all', [
  'all',
  '30d',
  '90d',
  '180d',
  '1y',
])
const mainTab = ref(mainTabQuery.parse(route.query.tab))
const stats = ref<TradeStats | null>(null)
const statsLoading = ref(false)
const statsError = ref('')
const statsRange = ref(statsRangeQuery.parse(route.query.stats_range))
let statsSeq = 0
const rangeOptions = [
  { label: '全部历史', value: 'all' },
  { label: '近 30 天', value: '30d' },
  { label: '近 90 天', value: '90d' },
  { label: '近 180 天', value: '180d' },
  { label: '近 1 年', value: '1y' },
]
async function loadStats() {
  const requestedRange = statsRange.value
  const mySeq = ++statsSeq
  if (stats.value?.range !== requestedRange) stats.value = null
  statsLoading.value = true
  statsError.value = ''
  try {
    const result = await getTradeStats(requestedRange)
    if (mySeq !== statsSeq || requestedRange !== statsRange.value) return
    stats.value = result
  } catch (e) {
    if (mySeq === statsSeq) {
      statsError.value = (e as Error).message
      message.error(statsError.value)
      stats.value = null
    }
  } finally {
    if (mySeq === statsSeq) statsLoading.value = false
  }
}
watch(mainTab, (t) => {
  if (t === 'review' && !stats.value) loadStats()
  if (t === 'all')
    nextTick(() => {
      renderCurve()
      renderExposure()
    })
})
watch(statsRange, () => {
  if (mainTab.value === 'review') loadStats()
})
// 盈亏比无定义（窗口内没有亏损交易）时如实显示「—」，不写 0 也不写 ∞。
const profitFactorText = computed(() => {
  const pf = stats.value?.profit_factor
  return pf == null ? '—' : pf.toFixed(2)
})
function bucketBarWidth(list: TradeStatBucket[], b: TradeStatBucket) {
  const max = Math.max(...list.map((x) => Math.abs(x.realized_pnl)), 1)
  return `${(Math.abs(b.realized_pnl) / max) * 100}%`
}

// ---------- C13 行业 / 风格暴露 ----------
// 数据随组合总览一起下发（GET /positions/overview），不额外请求。
// **纪律**：某一维 available=false 就整块不渲染——那是「不知道」，不是「分布均匀」；
// unknown 桶用中性色且恒排最后（后端已保证顺序，前端不再重排）。
const exposureEl = ref<HTMLDivElement | null>(null)
let exposureChart: echarts.ECharts | null = null
const exposureQuery = enumQuery<'industry' | 'cap_style' | 'value_style'>('industry', [
  'industry',
  'cap_style',
  'value_style',
])
const exposureDimKey = ref(exposureQuery.parse(route.query.exposure))
const exposureDimOptions = [
  { label: '行业', value: 'industry' },
  { label: '市值风格', value: 'cap_style' },
  { label: '估值风格', value: 'value_style' },
]
const exposure = computed(() => overview.value?.exposure || null)
const exposureDim = computed<ExposureDim | null>(() => {
  const ex = exposure.value
  if (!ex) return null
  const d = ex[exposureDimKey.value]
  return d?.available ? d : null
})
// 有任意一维可用才渲染整张卡（三维全无数据时不摆一张空卡）。
const exposureAnyAvailable = computed(() => {
  const ex = exposure.value
  return !!ex && (ex.industry.available || ex.cap_style.available || ex.value_style.available)
})
// 不可用维度的空态文案：分维度说清「为什么没有」，不写笼统的「暂无数据」。
const exposureEmptyText = computed(() => {
  switch (exposureDimKey.value) {
    case 'industry':
      return '行业归属尚未积累（全市场宇宙快照按交易日盘后落库），本维暂不可用——这是「暂时不知道」，不是「没有行业集中」。'
    case 'cap_style':
      return '估值数据源本次未取到总市值，本维暂不可用。'
    default:
      return '估值数据源本次未取到 PE，本维暂不可用。'
  }
})

function renderExposure() {
  const dim = exposureDim.value
  if (!exposureEl.value || !dim || !dim.buckets.length) {
    exposureChart?.dispose()
    exposureChart = null
    return
  }
  exposureChart?.dispose()
  exposureChart = echarts.init(exposureEl.value, isDark.value ? 'dark' : undefined)
  const primary = vars.value.primaryColor
  // 未知桶用文字三级色（中性），真实桶用主色按占比depth 递减——
  // 全部走主题键，6 套主题下都成立，无硬编码色。
  const known = dim.buckets.filter((b) => !b.unknown)
  const colorOf = (b: ExposureBucket, i: number) => {
    if (b.unknown) return withAlpha(vars.value.textColor3, 0.45)
    const step = known.length > 1 ? i / (known.length - 1) : 0
    return withAlpha(primary, 1 - step * 0.55)
  }
  exposureChart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      trigger: 'item',
      confine: true,
      formatter: (p: { name: string; value: number; dataIndex: number }) => {
        const b = dim.buckets[p.dataIndex]
        if (!b) return ''
        const tail = b.unknown ? '<br/>（数据缺失，不代表分布均匀）' : ''
        return `${b.label}<br/>占比 ${b.weight_pct.toFixed(1)}% · ${b.count} 只<br/>市值 ${fmtMoney(b.value)}${tail}`
      },
    },
    grid: { left: 8, right: 46, top: 6, bottom: 6, containLabel: true },
    xAxis: { type: 'value', max: 100, axisLabel: { formatter: '{value}%', fontSize: 10 }, splitLine: { lineStyle: { color: vars.value.dividerColor } } },
    // 横向条形图：类目多时自上而下阅读比饼图更清楚（行业可能十几个）。
    yAxis: {
      type: 'category',
      inverse: true,
      data: dim.buckets.map((b) => b.label),
      axisLabel: { fontSize: 11, color: vars.value.textColor2 },
      axisTick: { show: false },
    },
    series: [
      {
        type: 'bar',
        data: dim.buckets.map((b, i) => ({ value: b.weight_pct, itemStyle: { color: colorOf(b, i), borderRadius: [0, 4, 4, 0] } })),
        barMaxWidth: 18,
        label: { show: true, position: 'right', formatter: '{c}%', fontSize: 10, color: vars.value.textColor3 },
      },
    ],
  })
}
watch([exposureDimKey, exposure], () => nextTick(() => renderExposure()))

// ---------- B7 资产曲线 ----------
const curveEl = ref<HTMLDivElement | null>(null)
let curveChart: echarts.ECharts | null = null
const curve = ref<PortfolioCurve | null>(null)
const curveDaysQuery = integerEnumQuery(90, [30, 90, 180, 365])
const curveDays = ref(curveDaysQuery.parse(route.query.curve_days))
const curveLoading = ref(false)
const curveError = ref('')
let curveAbort: AbortController | null = null
let curveSeq = 0
const curveDayOptions = [
  { label: '近 30 天', value: 30 },
  { label: '近 90 天', value: 90 },
  { label: '近 180 天', value: 180 },
  { label: '近 1 年', value: 365 },
]

// ---------- B8 除权除息待确认折算 ----------
// **纪律：程序绝不静默改写用户账本**——这里只呈现建议，用户点确认才写。
// 撤销仅在「账面仍等于折算结果且其后无新交易」时被后端接受，否则明确拒绝。
const corpAdjusts = ref<PositionCorpAdjust[]>([])
const corpAdjustLoading = ref(false)
const corpAdjustError = ref('')
const corpAdjustActing = ref<number | null>(null)
let corpAdjustAbort: AbortController | null = null

async function loadCorpAdjusts() {
  corpAdjustAbort?.abort()
  const ctrl = new AbortController()
  corpAdjustAbort = ctrl
  corpAdjustLoading.value = true
  corpAdjustError.value = ''
  try {
    corpAdjusts.value = await listCorpAdjusts('pending', ctrl.signal)
  } catch (e) {
    if (isAbortError(e)) return
    corpAdjustError.value = (e as Error).message
  } finally {
    if (corpAdjustAbort === ctrl) corpAdjustLoading.value = false
  }
}

async function doCorpAdjust(row: PositionCorpAdjust, action: 'confirm' | 'dismiss') {
  if (corpAdjustActing.value) return
  corpAdjustActing.value = row.id
  try {
    await actCorpAdjust(row.id, action)
    message.success(action === 'confirm' ? '已按方案折算持仓' : '已忽略该调整')
    invalidateAdvice()
    await Promise.all([loadCorpAdjusts(), load()])
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    corpAdjustActing.value = null
  }
}

// 折算说明（每 10 股口径原文优先）。
function adjustPlanText(a: PositionCorpAdjust) {
  if (a.plan_profile) return a.plan_profile
  const parts: string[] = []
  if (a.bonus_ratio > 0) parts.push(`送 ${a.bonus_ratio}`)
  if (a.transfer_ratio > 0) parts.push(`转 ${a.transfer_ratio}`)
  if (a.dividend_pretax > 0) parts.push(`派 ${a.dividend_pretax} 元`)
  return parts.length ? `每 10 股 ${parts.join(' ')}` : '—'
}

// ---------- D17 AI 持有 / 减仓 / 清仓建议 ----------
// 走 llm_tasks 后台任务：HTTP 秒回任务 id，前端轮询；浏览器断开不取消模型调用。
const advice = ref<PositionAdviceResult | null>(null)
const adviceLoading = ref(false)
const adviceError = ref('')
const adviceTargetPositionID = ref<number | null>(null)
let adviceAbort: AbortController | null = null

// 建议是一次性账本快照。任何会改变持仓数量/成本/公司行动或风险信号的成功操作，
// 都必须立刻废弃旧结果；同时取消在途轮询，避免旧请求稍后回填成“当前建议”。
function invalidateAdvice() {
  adviceAbort?.abort()
  adviceAbort = null
  adviceLoading.value = false
  advice.value = null
  adviceError.value = ''
}

async function runAdvice(target?: Position) {
  if (adviceLoading.value) return
  adviceAbort?.abort()
  const ctrl = new AbortController()
  adviceAbort = ctrl
  adviceLoading.value = true
  adviceTargetPositionID.value = target?.id ?? null
  adviceError.value = ''
  try {
    const task = await requestPositionAdvice(
      target ? { position_id: target.id, symbol: target.symbol } : {},
    )
    await resolveAdviceTask(task, ctrl)
    await nextTick()
    document.getElementById('position-advice-panel')?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  } catch (e) {
    if (isAbortError(e) || (e as Error).name === 'PollCancelled') return
    adviceError.value = (e as Error).message
  } finally {
    if (adviceAbort === ctrl) {
      adviceLoading.value = false
      adviceTargetPositionID.value = null
    }
  }
}

const exitLevelLabel: Record<PositionExitLevel, string> = {
  normal: '正常',
  watch: '观察',
  review: '待复核',
  urgent: '紧急',
  unknown: '数据不足',
}
const exitLevelType = (level: PositionExitLevel) =>
  level === 'urgent' ? 'error' : level === 'review' || level === 'watch' ? 'warning' : level === 'normal' ? 'success' : 'default'

async function resolveAdviceTask(
  initial: LLMTask<PositionAdviceResult>,
  ctrl: AbortController,
  expectedRouteTaskID: number | null = null,
) {
  if (initial.kind !== 'position_advice') throw new Error('该任务不是持仓建议任务，无法在此页面打开')
  const final =
    initial.status === 'processing'
      ? await pollUntil(
          () => getLLMTask<PositionAdviceResult>(initial.id),
          (task) => task.status !== 'processing',
          { signal: ctrl.signal },
        )
      : initial
  if (expectedRouteTaskID !== null && routeTaskID() !== expectedRouteTaskID) return
  if (final.kind !== 'position_advice') throw new Error('该任务不是持仓建议任务，无法在此页面打开')
  if (final.status !== 'success' || !final.result) {
    const detail = final.error || 'AI 建议生成失败'
    throw new Error(final.error_code ? `${detail}（${final.error_code}）` : detail)
  }
  advice.value = final.result
}

function routeTaskID(): number | null {
  const raw = Array.isArray(route.query.task_id) ? route.query.task_id[0] : route.query.task_id
  const id = Number(raw)
  return Number.isInteger(id) && id > 0 ? id : null
}

let restoredAdviceTaskID: number | null = null
let restoringAdviceTaskID: number | null = null
async function restoreRouteAdvice(): Promise<boolean> {
  const id = routeTaskID()
  if (!id) {
    restoredAdviceTaskID = null
    if (restoringAdviceTaskID !== null) adviceAbort?.abort()
    return false
  }
  if (restoredAdviceTaskID === id || restoringAdviceTaskID === id) return true

  restoringAdviceTaskID = id
  adviceAbort?.abort()
  const ctrl = new AbortController()
  adviceAbort = ctrl
  adviceLoading.value = true
  advice.value = null
  adviceError.value = ''
  try {
    const task = await getLLMTask<PositionAdviceResult>(id)
    if (routeTaskID() !== id) return true
    restoredAdviceTaskID = id
    await resolveAdviceTask(task, ctrl, id)
  } catch (e) {
    if (isAbortError(e) || (e as Error).name === 'PollCancelled') return true
    if (routeTaskID() === id) adviceError.value = (e as Error).message || '持仓建议任务状态读取失败'
  } finally {
    if (restoringAdviceTaskID === id) restoringAdviceTaskID = null
    if (adviceAbort === ctrl) {
      adviceAbort = null
      adviceLoading.value = false
    }
  }
  return true
}

watch(() => route.query.task_id, () => void restoreRouteAdvice())

const verdictLabel = (v: string) => POSITION_VERDICT_LABEL[v as keyof typeof POSITION_VERDICT_LABEL] || v
const verdictType = (v: string) => (v === 'exit' ? 'error' : v === 'trim' ? 'warning' : 'success')
const advicePositionType = (v: string) => (v === 'short_term' ? '短线' : v === 'long_term' ? '长线' : v)
const adviceGeneratedAt = computed(() => {
  if (!advice.value?.generated_at) return ''
  return new Date(advice.value.generated_at).toLocaleString('zh-CN', { hour12: false })
})

async function loadCurve() {
  curveAbort?.abort()
  const myAbort = new AbortController()
  curveAbort = myAbort
  const mySeq = ++curveSeq
  const requestedDays = curveDays.value
  if (curve.value?.days !== requestedDays) curve.value = null
  curveLoading.value = true
  curveError.value = ''
  try {
    const data = await getPositionCurve(requestedDays, myAbort.signal)
    if (mySeq !== curveSeq) return
    curve.value = data
    await nextTick()
    renderCurve()
  } catch (e) {
    if (mySeq !== curveSeq || isAbortError(e)) return
    curve.value = null
    curveError.value = (e as Error).message
    curveChart?.dispose()
    curveChart = null
  } finally {
    if (mySeq === curveSeq) curveLoading.value = false
  }
}
watch(curveDays, () => loadCurve())

function renderCurve() {
  const c = curve.value
  if (!curveEl.value || !c?.points.length) {
    curveChart?.dispose()
    curveChart = null
    return
  }
  curveChart?.dispose()
  curveChart = echarts.init(curveEl.value, isDark.value ? 'dark' : undefined)
  const dates = c.points.map((p) => p.trade_date)
  const primary = vars.value.primaryColor
  const warn = vars.value.warningColor
  curveChart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      trigger: 'axis',
      confine: true,
      formatter: (ps: { axisValue: string; seriesName: string; value: number; dataIndex: number }[]) => {
        if (!ps.length) return ''
        const p = c.points[ps[0].dataIndex]
        const lines = ps.map((s) => `${s.seriesName} ${fmtMoney(s.value)}`)
        if (p?.partial) lines.push(`⚠ ${p.note || '当日部分标的无有效行情，非完整净值'}`)
        return `${ps[0].axisValue}<br/>${lines.join('<br/>')}`
      },
    },
    legend: {
      top: 0,
      data: ['持仓市值', '累计已实现'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 62, right: 18, top: 30, bottom: 28 },
    xAxis: { type: 'category', data: dates, boundaryGap: false, axisLabel: { hideOverlap: true, fontSize: 10 } },
    yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { color: vars.value.dividerColor } }, axisLabel: { fontSize: 10 } },
    series: [
      {
        name: '持仓市值',
        type: 'line',
        data: c.points.map((p) => p.market_value),
        symbol: 'circle',
        // partial 点用空心大点标出：那天有标的没有有效行情，不是完整净值。
        symbolSize: (_v: number, params: { dataIndex: number }) => (c.points[params.dataIndex]?.partial ? 8 : 3),
        itemStyle: {
          color: (params: { dataIndex: number }) => (c.points[params.dataIndex]?.partial ? warn : primary),
        },
        lineStyle: { width: 2, color: primary },
        areaStyle: { color: withAlpha(primary, 0.1) },
      },
      {
        name: '累计已实现',
        type: 'line',
        data: c.points.map((p) => p.realized_cum),
        symbol: 'none',
        lineStyle: { width: 1.5, type: 'dashed', color: warn },
        itemStyle: { color: warn },
      },
    ],
  })
}
function onResize() {
  curveChart?.resize()
  exposureChart?.resize()
}
// 主题变化整套重绘：6 套主题中明暗只是其一，同为浅色的两套换主题时 isDark 不变
// 而 primaryColor/dividerColor 全变，只监听 isDark 会留在旧色板上。
watch([isDark, vars], () => {
  renderCurve()
  renderExposure()
})

const highlightedPositionID = ref<number | null>(null)
const focusedAssessment = ref<PositionExitAssessment | null>(null)
const focusedAssessmentError = ref('')
let lastConsumedStockAction = ''
let activeStockAction = ''

const expandedTradeID = computed<number>({
  get: () => expandedTrades.value || 0,
  set: (id) => {
    expandedTrades.value = id > 0 ? id : null
  },
})
useRouteQueryState(route, router, [
  queryRef('status', statusFilter, statusQuery),
  queryRef('type', typeFilter, typeQuery),
  queryRef('stats_range', statsRange, statsRangeQuery),
  queryRef('tab', mainTab, mainTabQuery),
  queryRef('exposure', exposureDimKey, exposureQuery),
  queryRef('curve_days', curveDays, curveDaysQuery),
  queryRef('trade', expandedTradeID, integerQuery(0, 1, 2_147_483_647)),
])
const { restoreScroll } = useListPageScroll(route, 'positions')

async function restoreExpandedTrade() {
  const id = expandedTrades.value
  if (!id || mainTab.value !== 'all') return
  // query 同步会先更新筛选再发请求；旧范围列表不能据此把新范围的展开项判成无效。
  if (loadedStatus !== statusFilter.value) return
  const target = positions.value.find((position) => position.id === id)
  if (!target || (typeFilter.value !== 'all' && target.position_type !== typeFilter.value)) {
    expandedTrades.value = null
    return
  }
  if (!tradesById.value[id]) await loadTrades(id)
}

watch(typeFilter, () => void restoreExpandedTrade())
watch(expandedTrades, () => void restoreExpandedTrade())

function stockActionKey() {
  if (route.query.import === '1') return 'import'
  const positionID = Number(route.query.position_id)
  if (Number.isInteger(positionID) && positionID > 0) {
    const assessmentID = Number(route.query.assessment_id)
    return Number.isInteger(assessmentID) && assessmentID > 0
      ? `position:${positionID}:assessment:${assessmentID}`
      : `position:${positionID}`
  }
  const symbol = String(route.query.symbol || '').trim()
  if (!symbol && route.query.add !== '1') return ''
  return String(route.query._stock_action || '') || [symbol, route.query.market || 'cn', route.query.add || '', route.query.quantity || ''].join(':')
}

function routeSuggestedQuantity(): number | undefined {
  const value = Number(route.query.quantity)
  return Number.isInteger(value) && value >= 100 && value % 100 === 0 ? value : undefined
}

function stockRouteRemainder() {
  const query = { ...route.query }
  for (const key of ['symbol', 'market', 'name', 'add', 'import', 'rec_id', 'quantity', '_stock_action']) delete query[key]
  return query
}

function positionElementID(id: number) {
  return mainTab.value === 'needs_action' ? `position-item-${id}` : `position-ledger-item-${id}`
}

function assessmentRouteID() {
  const value = Number(route.query.assessment_id)
  return Number.isInteger(value) && value > 0 ? value : null
}

async function loadFocusedAssessment(positionID: number) {
  const assessmentID = assessmentRouteID()
  focusedAssessment.value = null
  focusedAssessmentError.value = ''
  if (!assessmentID) return
  try {
    focusedAssessment.value = await getPositionExitAssessment(positionID, assessmentID)
  } catch (error) {
    focusedAssessmentError.value = (error as Error).message || '通知对应的卖出风险评估读取失败'
  }
}

async function applyStockActionQuery(): Promise<boolean> {
  const actionKey = stockActionKey()
  if (!actionKey || actionKey === lastConsumedStockAction || actionKey === activeStockAction) return false

  activeStockAction = actionKey
  try {
    if (route.query.import === '1') {
      lastConsumedStockAction = actionKey
      openImport()
      await router.replace({ name: 'positions', query: stockRouteRemainder() })
      return false
    }
    if (route.query.add === '1') {
      lastConsumedStockAction = actionKey
      highlightedPositionID.value = null
      openCreate({
        symbol: String(route.query.symbol || ''),
        market: String(route.query.market || 'cn'),
        name: String(route.query.name || ''),
        recId: Number(route.query.rec_id) || 0,
        quantity: routeSuggestedQuantity(),
      })
      await router.replace({ name: 'positions', query: stockRouteRemainder() })
      return true
    }

    suspendStatusReload = true
    statusFilter.value = 'holding'
    typeFilter.value = 'all'
    suspendStatusReload = false
    await load()
    if (loadError.value || stockActionKey() !== actionKey) return true

    const requestedPositionID = Number(route.query.position_id)
    const symbol = String(route.query.symbol || '').trim().toLowerCase()
    const market = String(route.query.market || 'cn').trim().toLowerCase()
    const target = Number.isInteger(requestedPositionID) && requestedPositionID > 0
      ? positions.value.find((position) => position.id === requestedPositionID)
      : positions.value.find(
          (position) =>
            position.symbol.trim().toLowerCase() === symbol &&
            (position.market || 'cn').trim().toLowerCase() === market,
        )

    lastConsumedStockAction = actionKey
    if (target) {
      highlightedPositionID.value = target.id
      if (assessmentRouteID()) {
        mainTab.value = 'needs_action'
        await loadFocusedAssessment(target.id)
      } else {
        mainTab.value = 'all'
        focusedAssessment.value = null
      }
      await nextTick()
      document.getElementById(positionElementID(target.id))?.scrollIntoView({
        behavior: 'smooth',
        block: 'center',
      })
    } else if (!(Number.isInteger(requestedPositionID) && requestedPositionID > 0)) {
      highlightedPositionID.value = null
      openCreate({
        symbol: String(route.query.symbol || ''),
        market: String(route.query.market || 'cn'),
        name: String(route.query.name || ''),
        recId: Number(route.query.rec_id) || 0,
        quantity: routeSuggestedQuantity(),
      })
    }
    await router.replace({ name: 'positions', query: stockRouteRemainder() })
    return true
  } finally {
    if (activeStockAction === actionKey) activeStockAction = ''
  }
}

watch(
  () => [route.query._stock_action, route.query.position_id, route.query.assessment_id],
  () => void applyStockActionQuery(),
)

onMounted(async () => {
  const hasExplicitTask = routeTaskID() !== null
  const hadStockAction = !!stockActionKey()
  // 股票动作先核对当前持仓：已有记录定位高亮，否则预填建仓；rec_id 保留推荐血缘。
  if (!(await applyStockActionQuery())) await load()
  await Promise.all([loadCurve(), loadCorpAdjusts()])
  if (mainTab.value === 'review' && !stats.value) await loadStats()
  if (hasExplicitTask) void restoreRouteAdvice()
  window.addEventListener('resize', onResize)
  if (!hadStockAction) await restoreScroll()
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  positionLoadSeq++
  statsSeq++
  tradesSeq++
  corpAdjustAbort?.abort()
  corpAdjustAbort = null
  adviceAbort?.abort()
  adviceAbort = null
  curveSeq++
  curveAbort?.abort()
  curveAbort = null
  curveChart?.dispose()
  curveChart = null
  exposureChart?.dispose()
  exposureChart = null
})
</script>

<template>
  <PageContainer title="持仓卖出决策中心" subtitle="先处理风险，再管理账本与复盘">
    <template v-if="mainTab !== 'risk'" #actions>
      <n-button size="small" type="primary" @click="openCreate()">+ 新建持仓</n-button>
      <n-button size="small" quaternary @click="openImport">导入</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <div class="pos" :style="styleVars">
      <!-- 汇总（组合总览：全组合口径） -->
      <n-grid v-if="mainTab === 'all'" cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="持仓成本" :value="overview ? fmtMoney(overview.total_cost) : '—'" />
        </n-gi>
        <n-gi>
          <StatCard
            label="当前市值"
            :value="overview ? fmtMoney(overview.total_value) : '—'"
            :sub="pricedLabel"
          />
        </n-gi>
        <n-gi>
          <StatCard
            label="浮动盈亏"
            :value="overview ? fmtMoney(overview.total_profit) : '—'"
            :change-pct="overview ? overview.profit_pct : undefined"
          />
        </n-gi>
        <n-gi>
          <StatCard
            label="已实现盈亏"
            :value="overview ? fmtMoney(overview.realized_profit) : '—'"
            :sub="overview ? '已平仓累计' : ''"
          />
        </n-gi>
        <n-gi>
          <StatCard
            label="持仓笔数"
            :value="overview ? String(overview.holding_count) : '—'"
            :sub="overview ? `盈 ${overview.win_count} · 亏 ${overview.lose_count}` : ''"
          />
        </n-gi>
        <n-gi>
          <StatCard label="短线 / 长线" :value="mixLabel" sub="按市值占比" />
        </n-gi>
        <n-gi>
          <StatCard
            label="最大持仓占比"
            :value="overview?.top_weight_pct ? overview.top_weight_pct.toFixed(1) + '%' : '—'"
            :sub="overview?.top_name || overview?.top_symbol || ''"
          />
        </n-gi>
      </n-grid>

      <n-alert v-if="mainTab === 'all' && loadError" type="error" :bordered="false" title="持仓读取失败">
        {{ loadError }}
      </n-alert>

      <!-- 组合风控信号（集中度/止损/未分析） -->
      <n-alert v-if="mainTab === 'all' && overview?.signals?.length" type="warning" title="组合风控信号">
        <div v-for="(s, i) in overview.signals" :key="i" class="signal-line">{{ s }}</div>
      </n-alert>

      <n-tabs v-model:value="mainTab" type="line" animated>
        <n-tab-pane name="needs_action" tab="需要处理">
          <PositionDecisionCenter
            :positions="positions"
            :overview="overview"
            :loading="loading"
            :error="loadError || focusedAssessmentError"
            :focused-position-id="highlightedPositionID"
            :focused-assessment="focusedAssessment"
            :advice="advice"
            :advice-loading="adviceLoading"
            :advice-error="adviceError"
            :advice-target-position-id="adviceTargetPositionID"
            @refresh="load()"
            @review="runAdvice"
          />
        </n-tab-pane>
        <n-tab-pane name="all" tab="全部持仓">
          <!-- B8 除权除息待确认折算：仅在有 pending 建议时出现。
               **不确认的话本页盈亏就是错的数字**（10 转 10 后显示 -50%），
               所以放在最顶部；但程序绝不代替用户改账本。 -->
          <SectionCard v-if="corpAdjusts.length || corpAdjustError" title="除权除息待确认">
            <template #extra>
              <n-button size="tiny" quaternary :loading="corpAdjustLoading" @click="loadCorpAdjusts">刷新</n-button>
            </template>
            <n-alert v-if="corpAdjustError" type="error" :bordered="false" title="调整建议读取失败">
              {{ corpAdjustError }}
            </n-alert>
            <template v-else>
              <n-alert type="warning" :bordered="false" style="margin-bottom: 10px" :show-icon="false">
                以下持仓已到除权除息日。普通建议核对后可确认折算；标记“需人工核对”的历史错序记录不会自动改账，
                只有明确确认已自行核对后才能忽略并解除交易拦截。
              </n-alert>
              <div class="adjust-list">
                <div v-for="a in corpAdjusts" :key="a.id" class="adjust-row">
                  <div class="adjust-main">
                    <div class="adjust-head">
                      <StockIdentity :symbol="a.symbol" :market="a.market || 'cn'" :name="a.name" density="table" clickable />
                      <n-tag v-if="a.manual_review" size="tiny" round :bordered="false" type="error">需人工核对</n-tag>
                      <n-tag v-if="a.record_date" size="tiny" round :bordered="false">登记日 {{ a.record_date }}</n-tag>
                      <n-tag size="tiny" round :bordered="false" type="warning">除权日 {{ a.ex_date }}</n-tag>
                    </div>
                    <div class="adjust-plan">{{ adjustPlanText(a) }}</div>
                    <div v-if="a.manual_review" class="adjust-review">
                      {{ a.review_reason }}。忽略只解除拦截，不会自动修正数量、成本或已实现盈亏。
                    </div>
                    <div v-else class="adjust-calc qv-tnum">
                      <template v-if="a.entitled_qty > 0">登记日有权 {{ a.entitled_qty }} 股 · </template>
                      当前数量 {{ a.qty_before }} → <b>{{ a.qty_after }}</b> 股 · 成本
                      {{ a.cost_before.toFixed(4) }} → <b>{{ a.cost_after.toFixed(4) }}</b> 元
                      <span v-if="a.cash_dividend > 0"> · 现金分红 {{ a.cash_dividend.toFixed(2) }} 元（税前）</span>
                    </div>
                  </div>
                  <div class="adjust-actions">
                    <n-button
                      v-if="!a.manual_review"
                      size="small"
                      type="primary"
                      :loading="corpAdjustActing === a.id"
                      @click="doCorpAdjust(a, 'confirm')"
                      >确认折算</n-button
                    >
                    <n-popconfirm
                      v-if="a.manual_review"
                      positive-text="确认已核对"
                      negative-text="取消"
                      @positive-click="doCorpAdjust(a, 'dismiss')"
                    >
                      <template #trigger>
                        <n-button size="small" type="warning" :loading="corpAdjustActing === a.id">已核对并忽略</n-button>
                      </template>
                      确认已自行核对历史流水。系统不会自动修正账本，忽略后将允许继续交易。
                    </n-popconfirm>
                    <n-button v-else size="small" quaternary @click="doCorpAdjust(a, 'dismiss')">忽略</n-button>
                  </div>
                </div>
              </div>
            </template>
          </SectionCard>

          <!-- D17 AI 卖出建议：逐笔持仓的 hold/trim/exit 封闭结论。
               无当前有效行情的仓位不参与（不基于旧价出割/守/补结论）。 -->
          <SectionCard id="position-advice-panel" title="AI 卖出建议">
            <template #extra>
              <n-button size="tiny" type="primary" ghost :loading="adviceLoading" @click="runAdvice()">
                分析全部持仓
              </n-button>
            </template>
            <n-alert v-if="adviceError" type="error" :bordered="false" title="AI 建议生成失败">
              {{ adviceError }}
            </n-alert>
            <div v-else-if="!advice" class="advice-empty">
              针对<b>每一笔持仓</b>给出「继续持有 / 减仓 / 清仓」的结论、理由与失效条件。
              成本、浮动盈亏、持有交易日、自最高点回撤等数值由服务端算好后喂给模型，模型只做判断不做算术；
              无当前有效行情的持仓不参与（不基于旧价给出割/守/补结论）。
            </div>
            <div v-else class="advice-box">
              <div v-for="(n, i) in advice.notes || []" :key="i" class="advice-note">{{ n }}</div>
              <div class="advice-list">
                <div v-for="a in advice.advices" :key="a.position_id" class="advice-row">
                  <div class="advice-head">
                    <n-tag size="small" round :bordered="false" :type="verdictType(a.verdict)">{{
                      verdictLabel(a.verdict)
                    }}</n-tag>
                    <StockIdentity :symbol="a.symbol" market="cn" :name="a.name" density="table" clickable />
                    <span class="advice-position qv-tnum">
                      {{ advicePositionType(a.position_type) }} · 成本 {{ a.cost.toFixed(2) }} · {{ a.quantity }} 股
                    </span>
                  </div>
                  <div class="advice-reason">{{ a.reason }}</div>
                  <div v-if="a.invalidation" class="advice-invalid">失效条件：{{ a.invalidation }}</div>
                </div>
              </div>
              <div class="advice-foot">
                本次分析 {{ advice.analyzed }} 笔<span v-if="advice.skipped > 0">，跳过 {{ advice.skipped }} 笔</span>
                <span v-if="advice.evidence_check">
                  · 证据核验 {{ advice.evidence_check.matched }}/{{ advice.evidence_check.total }} 项与数据一致</span
                >
                <span v-if="advice.model"> · {{ advice.model }}</span>
                <span v-if="adviceGeneratedAt"> · 生成于 {{ adviceGeneratedAt }}</span>
                。研究参考，不构成投资建议。
              </div>
            </div>
          </SectionCard>

          <!-- C13 行业 / 风格暴露：回答「我是不是把钱全压在一个赛道上」。
               缺数据的维度整块缺席（那是「不知道」不是「均匀」），
               缺失部分显式成「未知」桶且恒排最后。 -->
          <SectionCard v-if="exposureAnyAvailable" title="行业 / 风格暴露">
            <template #extra>
              <n-radio-group v-model:value="exposureDimKey" size="small">
                <n-radio-button v-for="o in exposureDimOptions" :key="o.value" :value="o.value">
                  {{ o.label }}
                </n-radio-button>
              </n-radio-group>
            </template>
            <template v-if="exposureDim">
              <div ref="exposureEl" class="exposure-chart"></div>
              <div class="exposure-meta qv-tnum">
                已定价市值 {{ fmtMoney(exposure?.base ?? 0) }} · 本维覆盖
                {{ exposureDim.known_pct.toFixed(1) }}%
              </div>
              <div v-if="exposureDim.note" class="exposure-note">{{ exposureDim.note }}</div>
              <div class="exposure-note">{{ exposure?.base_note }}</div>
            </template>
            <n-empty v-else :description="exposureEmptyText" />
          </SectionCard>

          <!-- B7 资产曲线：读每交易日 16:20 落库的快照，不做插值补造 -->
          <SectionCard title="资产曲线">
            <template #extra>
              <div class="filters">
                <n-select
                  v-model:value="curveDays"
                  :options="curveDayOptions"
                  size="small"
                  style="width: 120px"
                />
                <n-button size="tiny" quaternary :loading="curveLoading" @click="loadCurve">刷新</n-button>
              </div>
            </template>
            <n-spin :show="curveLoading && !curve">
              <n-alert v-if="curveError" type="error" :bordered="false" title="资产曲线读取失败">
                {{ curveError }}
              </n-alert>
              <div v-show="!!curve?.points.length" ref="curveEl" class="curve-chart"></div>
              <n-empty
                v-if="!curveLoading && !curveError && curve && !curve.points.length"
                description="暂无资产快照——曲线自启用之日起按交易日盘后积累，不回溯历史"
              />
              <div v-if="curve?.notes?.length" class="curve-notes">
                <div v-for="(n, i) in curve.notes" :key="i">{{ n }}</div>
              </div>
            </n-spin>
          </SectionCard>

          <SectionCard title="持仓明细">
            <template #extra>
              <div class="filters">
                <n-radio-group v-model:value="typeFilter" size="small">
                  <n-radio-button value="all">全部</n-radio-button>
                  <n-radio-button value="short_term">短线</n-radio-button>
                  <n-radio-button value="long_term">长线</n-radio-button>
                </n-radio-group>
                <n-radio-group v-model:value="statusFilter" size="small">
                  <n-radio-button value="holding">持仓中</n-radio-button>
                  <n-radio-button value="closed">已卖出</n-radio-button>
                  <n-radio-button value="all">全部</n-radio-button>
                </n-radio-group>
              </div>
            </template>

            <n-spin :show="loading && !positions.length">
              <n-empty
                v-if="!loading && !loadError && !filtered.length"
                description="暂无持仓，点击「新建持仓」记录一笔买入"
              />
              <div v-if="filtered.length" class="rows">
                <div
                  v-for="p in filtered"
                  :id="`position-ledger-item-${p.id}`"
                  :key="p.id"
                  class="row-wrap"
                  :class="{ 'is-stock-action-target': highlightedPositionID === p.id }"
                >
                  <div class="row">
                    <div class="r-name">
                      <div class="r-title-line">
                        <n-tag
                          size="tiny"
                          round
                          :bordered="false"
                          :type="p.position_type === 'short_term' ? 'warning' : 'info'"
                          >{{ typeLabel(p.position_type) }}</n-tag
                        >
                        <StockIdentity :symbol="p.symbol" :market="p.market" :name="p.name" density="table" clickable actions />
                        <n-tag v-if="p.status === 'closed'" size="tiny" :bordered="false">已卖出</n-tag>
                        <n-tag
                          v-if="p.status === 'holding' && p.exit_assessment"
                          size="tiny"
                          :bordered="false"
                          :type="exitLevelType(p.exit_assessment.level)"
                        >{{ exitLevelLabel[p.exit_assessment.level] }}</n-tag>
                        <n-tag v-if="p.below_stop_loss" size="tiny" type="error" :bordered="false">破止损</n-tag>
                        <n-tag v-else-if="p.near_stop_loss" size="tiny" type="warning" :bordered="false">近止损</n-tag>
                        <FreshnessTag
                          v-if="p.status === 'holding'"
                          :status="p.freshness_status"
                          :as-of="p.quote_as_of"
                          :reason="p.stale_reason"
                        />
                        <n-tag
                          v-if="p.status === 'holding' && p.analysis_stale"
                          size="tiny"
                          :bordered="false"
                          class="tag-click"
                          title="点击发起个股分析"
                          @click="goAnalysis(p)"
                          >{{ staleLabel(p) }}</n-tag
                        >
                      </div>
                      <div class="r-sub">
                        <template v-if="p.status === 'closed'">
                          累计买入 {{ p.total_buy_qty || p.quantity }} 股 · 均价 {{ fmt(p.buy_price) }}
                        </template>
                        <template v-else> 持有 {{ p.quantity }} 股 · 均价 {{ fmt(p.buy_price) }} </template>
                        <span v-if="p.buy_date">· {{ p.buy_date }}</span>
                        <span v-if="p.status === 'holding' && p.held_trade_days > 0">· 持有 {{ p.held_trade_days }} 交易日</span>
                        <span v-if="p.status === 'closed'"> · 末笔卖出 {{ fmt(p.sell_price) }}</span>
                        <span v-if="p.status === 'holding' && p.realized_pnl" :style="{ color: pctColor(p.realized_pnl) }">
                          · 已兑现 {{ fmtMoney(p.realized_pnl) }}
                        </span>
                      </div>
                      <div v-if="p.short_term_review" class="r-hint" :style="{ color: warnColor }">
                        ⚠ 短线已持有 {{ p.held_trade_days }} 交易日，建议复盘是否止盈/止损或转长线
                      </div>
                      <!-- D15 持仓期最高价与回撤：回答「我赚过多少、现在回吐了多少」。
                           峰值自建仓起算（买入日之前的高点不算），加仓后按新成本重新起算。 -->
                      <div v-if="p.status === 'holding' && p.peak" class="r-peak">
                        持仓期最高 <span class="qv-tnum">{{ fmt(p.peak.price) }}</span>
                        <span v-if="p.peak.date">（{{ p.peak.date }}）</span>
                        <template v-if="p.quote_ok && p.peak.drawdown_pct > 0">
                          · 已回撤
                          <span class="qv-tnum" :style="{ color: pctColor(-p.peak.drawdown_pct) }"
                            >{{ p.peak.drawdown_pct.toFixed(2) }}%</span
                          >
                        </template>
                        <span v-else-if="!p.quote_ok"> · 回撤未知（无当前有效行情）</span>
                        <span v-if="p.peak.from"> · 自 {{ p.peak.from }} 起算</span>
                        <span v-if="p.peak.backfilled" class="r-peak-note" :title="p.peak.note">（含日线回填）</span>
                      </div>
                      <div
                        v-if="p.status === 'holding' && p.exit_assessment"
                        class="exit-assessment"
                        :class="`is-${p.exit_assessment.level}`"
                      >
                        <div class="exit-assessment-head">
                          <span class="exit-reason">{{ p.exit_assessment.primary_reason }}</span>
                          <span class="exit-asof qv-tnum">
                            行情 {{ p.exit_assessment.quote_as_of || '未知' }} · 日线
                            {{ p.exit_assessment.bars_as_of || '未知' }}
                          </span>
                        </div>
                        <div v-if="p.exit_assessment.evidence.length" class="exit-evidence">
                          <span v-for="(item, index) in p.exit_assessment.evidence" :key="index">{{ item }}</span>
                        </div>
                        <div v-if="p.exit_assessment.data_gaps.length" class="exit-gaps">
                          {{ p.exit_assessment.data_gaps.join('；') }}
                        </div>
                        <div class="exit-action">
                          <span>{{ p.exit_assessment.next_action }}</span>
                          <n-button
                            v-if="p.exit_assessment.level === 'review' || p.exit_assessment.level === 'urgent'"
                            size="tiny"
                            type="primary"
                            ghost
                            :loading="adviceLoading && adviceTargetPositionID === p.id"
                            :disabled="adviceLoading && adviceTargetPositionID !== p.id"
                            @click="runAdvice(p)"
                          >AI 复核</n-button>
                        </div>
                      </div>
                      <div v-else-if="p.status === 'holding'" class="exit-assessment-empty">
                        卖出风险评估尚未生成
                      </div>
                      <div v-if="p.status === 'closed' && p.review_note" class="r-review">
                        复盘：{{ p.review_note }}
                      </div>
                    </div>

                    <div class="r-figures">
                      <div class="r-fig">
                        <span class="r-fig-label">{{ p.status === 'closed' ? '卖出价' : '现价' }}</span>
                        <span class="r-fig-val qv-tnum">{{ p.quote_ok ? fmt(p.current_price) : '—' }}</span>
                        <span
                          v-if="!p.quote_ok && p.status === 'holding' && p.last_price"
                          class="r-fig-stale qv-tnum"
                          :title="`最近已知价（截至 ${p.quote_as_of || '未知'}，已过期，不代表当前价格）`"
                          >旧 {{ fmt(p.last_price) }}</span
                        >
                      </div>
                      <div class="r-fig">
                        <span class="r-fig-label">{{ p.status === 'closed' ? '已实现盈亏' : '盈亏' }}</span>
                        <span class="r-fig-val qv-tnum" :style="{ color: pctColor(p.profit_amount) }">
                          {{ p.quote_ok ? fmtMoney(p.profit_amount) : '—' }}
                        </span>
                      </div>
                      <div class="r-fig">
                        <span class="r-fig-label">收益率</span>
                        <span class="r-fig-val qv-tnum" :style="{ color: pctColor(p.profit_pct) }">
                          {{ p.quote_ok ? p.profit_pct.toFixed(2) + '%' : '—' }}
                        </span>
                      </div>
                    </div>

                    <div class="r-actions">
                      <n-button v-if="p.status === 'holding'" size="tiny" type="primary" ghost @click="openTrade(p, 'buy')"
                        >加仓</n-button
                      >
                      <n-button v-if="p.status === 'holding'" size="tiny" type="warning" ghost @click="openTrade(p, 'sell')"
                        >减仓</n-button
                      >
                      <n-button v-if="p.status === 'holding'" size="tiny" type="primary" ghost @click="openClose(p)"
                        >清仓</n-button
                      >
                      <n-button size="tiny" quaternary @click="toggleTrades(p)">
                        {{ expandedTrades === p.id ? '收起流水' : '流水' }}
                      </n-button>
                      <n-button size="tiny" quaternary @click="goAnalysis(p)">分析</n-button>
                      <n-button size="tiny" quaternary @click="goAlert(p)">提醒</n-button>
                      <n-button size="tiny" quaternary @click="goThesis(p)">逻辑卡</n-button>
                      <n-button size="tiny" quaternary @click="openEdit(p)">编辑</n-button>
                      <n-popconfirm @positive-click="remove(p)">
                        <template #trigger>
                          <n-button size="tiny" quaternary type="error">删除</n-button>
                        </template>
                        删除持仓「{{ p.name || '名称待补全' }}（{{ p.symbol }}）」？流水明细一并删除。
                      </n-popconfirm>
                    </div>
                  </div>

                  <!-- B5 流水明细（展开一行） -->
                  <div v-if="expandedTrades === p.id" class="trade-panel">
                    <n-spin :show="tradesLoading && !tradesById[p.id]">
                      <n-alert
                        v-if="tradesErrorById[p.id]"
                        type="error"
                        :bordered="false"
                        title="流水读取失败"
                      >
                        {{ tradesErrorById[p.id] }}
                      </n-alert>
                      <n-empty
                        v-else-if="!tradesLoading && tradesById[p.id] && !tradesById[p.id].length"
                        size="small"
                        description="暂无流水"
                      />
                      <table v-else class="trade-table qv-tnum">
                        <thead>
                          <tr>
                            <th>日期</th>
                            <th>方向</th>
                            <th class="ta-r">价格</th>
                            <th class="ta-r">数量</th>
                            <th class="ta-r">费用</th>
                            <th class="ta-r">税费</th>
                            <th class="ta-r">已实现</th>
                            <th class="ta-r">持仓后</th>
                            <th class="ta-r">均价后</th>
                            <th>备注</th>
                          </tr>
                        </thead>
                        <tbody>
                          <tr v-for="t in tradesById[p.id]" :key="t.id">
                            <td>{{ t.trade_date || '—' }}</td>
                            <td>
                              <span :style="{ color: sideColor(t.side) }">{{ sideLabel(t.side) }}</span>
                            </td>
                            <td class="ta-r">{{ t.side === 'adjust' ? '—' : fmt(t.price) }}</td>
                            <td
                              class="ta-r"
                              :title="t.side === 'adjust' ? '除权折算导致的持仓数量变化，不是一次买卖' : ''"
                            >
                              {{
                                t.side === 'adjust'
                                  ? t.quantity
                                    ? `${t.quantity > 0 ? '+' : ''}${t.quantity}`
                                    : '—'
                                  : t.quantity
                              }}
                            </td>
                            <td class="ta-r">{{ t.side === 'adjust' ? '—' : fmt(t.fee) }}</td>
                            <td class="ta-r">{{ t.side === 'adjust' ? '—' : fmt(t.tax) }}</td>
                            <td
                              class="ta-r"
                              :style="{ color: t.side === 'sell' ? pctColor(t.realized_pnl) : undefined }"
                              :title="t.side === 'adjust' ? '除权折算笔记的是到手税前现金分红' : ''"
                            >
                              {{ t.side === 'buy' || (t.side === 'adjust' && !t.realized_pnl) ? '—' : fmtMoney(t.realized_pnl) }}
                            </td>
                            <td class="ta-r">{{ t.quantity_after }}</td>
                            <td class="ta-r">{{ fmt(t.avg_cost_after) }}</td>
                            <td class="t-note">
                              {{ t.note || '—' }}
                              <n-tag v-if="t.backfilled" size="tiny" :bordered="false" title="旧持仓惰性补建的等价记录，非用户录入">补建</n-tag>
                              <n-popconfirm v-if="t.side === 'adjust' && t.adjust_id" @positive-click="revertAdjustTrade(t, p.id)">
                                <template #trigger>
                                  <n-button size="tiny" quaternary :loading="revertingTrade === t.id">撤销折算</n-button>
                                </template>
                                撤销后数量与成本回滚到折算前（{{ t.quantity_before }} 股 / {{ fmt(t.avg_cost_before || 0) }}
                                元）。若此后已有新交易，后端会拒绝撤销。
                              </n-popconfirm>
                            </td>
                          </tr>
                        </tbody>
                      </table>
                    </n-spin>
                  </div>
                </div>
              </div>
            </n-spin>
          </SectionCard>
        </n-tab-pane>

        <!-- B6 复盘统计：用户执行口径，与推荐追踪的模型口径不混算 -->
        <n-tab-pane name="review" tab="交易与复盘">
          <SectionCard title="交易复盘统计">
            <template #extra>
              <div class="filters">
                <n-select v-model:value="statsRange" :options="rangeOptions" size="small" style="width: 130px" />
                <n-button size="tiny" quaternary :loading="statsLoading" @click="loadStats">刷新</n-button>
              </div>
            </template>
            <n-spin :show="statsLoading && !stats">
              <n-alert v-if="statsError" type="error" :bordered="false" title="复盘统计读取失败">
                {{ statsError }}
              </n-alert>
              <template v-else-if="stats">
                <n-grid cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen">
                  <n-gi>
                    <StatCard
                      label="总已实现盈亏"
                      :value="fmtMoney(stats.total_realized_pnl)"
                      :sub="`${stats.closed} 笔已平仓`"
                    />
                  </n-gi>
                  <n-gi>
                    <StatCard
                      label="胜率"
                      :value="stats.closed ? stats.win_rate.toFixed(1) + '%' : '—'"
                      :sub="`盈 ${stats.win_count} · 亏 ${stats.loss_count} · 平 ${stats.flat_count}`"
                    />
                  </n-gi>
                  <n-gi>
                    <StatCard
                      label="盈亏比"
                      :value="profitFactorText"
                      :sub="stats.profit_factor == null ? '窗口内无亏损交易，无定义' : `均盈 ${fmtMoney(stats.avg_win)} / 均亏 ${fmtMoney(stats.avg_loss)}`"
                    />
                  </n-gi>
                  <n-gi>
                    <StatCard
                      label="平均持有"
                      :value="stats.hold_sample ? stats.avg_hold_trade_days.toFixed(1) + ' 交易日' : '—'"
                      :sub="`${stats.hold_sample}/${stats.closed} 笔可计算`"
                    />
                  </n-gi>
                </n-grid>

                <n-empty v-if="!stats.closed" description="当前窗口内没有已平仓记录（不是 0%，是没有样本）" />

                <template v-else>
                  <div class="dist-grid">
                    <div v-for="dist in [
                      { title: '按行业', rows: stats.by_industry },
                      { title: '按持有时长', rows: stats.by_hold_bucket },
                      { title: '按买入理由', rows: stats.by_buy_reason },
                      { title: '按 AI 判断', rows: stats.by_ai_verdict },
                      { title: '按是否按计划', rows: stats.by_sell_planned },
                    ]" :key="dist.title" class="dist-block">
                      <div class="dist-title">{{ dist.title }}</div>
                      <div v-for="b in dist.rows" :key="b.key" class="dist-row">
                        <span class="dist-label" :class="{ 'is-unknown': b.unknown }">{{ b.label }}</span>
                        <div class="dist-bar-wrap">
                          <div
                            class="dist-bar"
                            :style="{
                              width: bucketBarWidth(dist.rows, b),
                              background: b.realized_pnl >= 0 ? withAlpha(vars.errorColor, 0.7) : withAlpha(vars.successColor, 0.7),
                            }"
                          ></div>
                        </div>
                        <span class="dist-val qv-tnum" :style="{ color: pctColor(b.realized_pnl) }">{{ fmtMoney(b.realized_pnl) }}</span>
                        <span class="dist-meta qv-tnum">{{ b.trades }} 笔 · 胜 {{ b.win_rate.toFixed(0) }}%</span>
                      </div>
                    </div>
                  </div>

                  <div class="top-grid">
                    <div class="top-block">
                      <div class="dist-title">最赚 Top{{ stats.top_winners.length }}</div>
                      <n-empty v-if="!stats.top_winners.length" size="small" description="窗口内无盈利交易" />
                      <div v-for="t in stats.top_winners" :key="'w' + t.position_id" class="top-row">
                        <StockIdentity :symbol="t.symbol" market="cn" :name="t.name" density="table" clickable />
                        <span class="qv-tnum" :style="{ color: pctColor(t.realized_pnl) }">{{ fmtMoney(t.realized_pnl) }}</span>
                        <span class="qv-tnum" :style="{ color: pctColor(t.return_pct) }">{{ t.return_pct.toFixed(2) }}%</span>
                        <span class="top-meta qv-tnum">{{ t.hold_trade_days ? t.hold_trade_days + ' 交易日' : '—' }}</span>
                      </div>
                    </div>
                    <div class="top-block">
                      <div class="dist-title">最亏 Top{{ stats.top_losers.length }}</div>
                      <n-empty v-if="!stats.top_losers.length" size="small" description="窗口内无亏损交易" />
                      <div v-for="t in stats.top_losers" :key="'l' + t.position_id" class="top-row">
                        <StockIdentity :symbol="t.symbol" market="cn" :name="t.name" density="table" clickable />
                        <span class="qv-tnum" :style="{ color: pctColor(t.realized_pnl) }">{{ fmtMoney(t.realized_pnl) }}</span>
                        <span class="qv-tnum" :style="{ color: pctColor(t.return_pct) }">{{ t.return_pct.toFixed(2) }}%</span>
                        <span class="top-meta qv-tnum">{{ t.hold_trade_days ? t.hold_trade_days + ' 交易日' : '—' }}</span>
                      </div>
                    </div>
                  </div>

                  <div class="lesson-block">
                    <div class="dist-title">复盘教训清单</div>
                    <n-empty v-if="!stats.lessons.length" size="small" description="还没有填写过「下次策略调整点」" />
                    <div v-for="l in stats.lessons" :key="l.position_id" class="lesson-row">
                      <span class="lesson-date qv-tnum">{{ l.sell_date || '—' }}</span>
                      <StockIdentity :symbol="l.symbol" market="cn" :name="l.name" density="table" clickable />
                      <span class="qv-tnum" :style="{ color: pctColor(l.realized_pnl) }">{{ fmtMoney(l.realized_pnl) }}</span>
                      <span class="lesson-text">{{ l.lesson }}</span>
                    </div>
                  </div>
                </template>

                <div v-if="stats.notes.length" class="curve-notes">
                  <div v-for="(n, i) in stats.notes" :key="i">{{ n }}</div>
                </div>
              </template>
            </n-spin>
          </SectionCard>
        </n-tab-pane>
        <n-tab-pane name="risk" tab="组合风险" display-directive="show:lazy">
          <PortfolioRisk embedded />
        </n-tab-pane>
      </n-tabs>
    </div>

    <!-- 建仓 / 编辑 -->
    <n-modal
      v-model:show="editModal"
      preset="card"
      :title="editing ? '编辑持仓' : '新建持仓'"
      style="max-width: 520px"
    >
      <n-form label-placement="top">
        <n-alert v-if="editingClosed" type="info" :bordered="false" style="margin-bottom: 14px">
          已平仓持仓仅可修改「买入理由」与「备注」，其余成交数据不可再更改。
        </n-alert>
        <template v-if="!editingClosed">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="股票代码">
                <n-input v-model:value="form.symbol" placeholder="如 600000" :disabled="editing" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="市场">
                <n-select v-model:value="form.market" :options="marketOptions" :disabled="editing" />
              </n-form-item>
            </n-gi>
          </n-grid>
          <n-form-item label="类型">
            <n-radio-group v-model:value="form.position_type">
              <n-radio-button value="short_term">短线</n-radio-button>
              <n-radio-button value="long_term">长线</n-radio-button>
            </n-radio-group>
          </n-form-item>
          <n-grid cols="1 s:3" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="买入价">
                <n-input-number v-model:value="form.buy_price" :min="0" :precision="4" style="width: 100%" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="数量">
                <n-input-number v-model:value="form.quantity" :min="0" style="width: 100%" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="买入日期">
                <n-input v-model:value="form.buy_date" placeholder="YYYY-MM-DD" />
              </n-form-item>
            </n-gi>
          </n-grid>
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="买入手续费">
                <n-input-number v-model:value="form.buy_fee" :min="0" :precision="2" style="width: 100%" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="买入税费">
                <n-input-number v-model:value="form.buy_tax" :min="0" :precision="2" style="width: 100%" />
              </n-form-item>
            </n-gi>
          </n-grid>
        </template>
        <n-form-item label="买入理由">
          <n-input
            v-model:value="form.buy_reason"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="为什么买入（可选）"
            maxlength="512"
          />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="form.user_note" placeholder="补充备注（可选）" maxlength="512" />
        </n-form-item>

        <!-- 风险计划 + 仓位风险计算器（实时纯前端计算） -->
        <template v-if="!editingClosed">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="计划止损价（可选）">
                <n-input-number v-model:value="form.plan_stop_loss" :min="0" :precision="4" style="width: 100%" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="计划止盈价（可选）">
                <n-input-number v-model:value="form.plan_take_profit" :min="0" :precision="4" style="width: 100%" />
              </n-form-item>
            </n-gi>
          </n-grid>
          <div v-if="riskCalc" class="risk-calc qv-tnum">
            <span>投入 {{ riskCalc.cost.toFixed(0) }} 元</span>
            <template v-if="riskCalc.maxLoss != null">
              <span :style="{ color: vars.errorColor }">
                触发止损亏 {{ riskCalc.maxLoss.toFixed(0) }} 元（-{{ riskCalc.maxLossPct!.toFixed(1) }}%）
              </span>
            </template>
            <span v-else class="risk-hint">填写止损价即可预估最大亏损</span>
            <span v-if="riskCalc.gain != null && riskCalc.maxLoss" >
              盈亏比 {{ (riskCalc.gain / riskCalc.maxLoss).toFixed(1) }}
            </span>
          </div>

          <!-- 买入前检查清单 -->
          <div class="checklist">
            <div class="checklist-head">
              <span>买入前检查（{{ checklistDone }}/{{ CHECKLIST.length }}）</span>
              <span class="risk-hint">勾选状态会随持仓保存，卖出复盘时对照</span>
            </div>
            <label v-for="(text, i) in CHECKLIST" :key="i" class="check-item">
              <input v-model="checklist[i]" type="checkbox" />
              <span>{{ text }}</span>
            </label>
          </div>
        </template>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="editModal = false">取消</n-button>
          <n-button type="primary" :loading="submitting" @click="submit">保存</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 平仓 -->
    <n-modal
      v-model:show="closeModal"
      preset="card"
      :title="`卖出 · ${closing?.name || '名称待补全'}${closing?.symbol ? `（${closing.symbol}）` : ''}`"
      style="max-width: 480px"
    >
      <n-form label-placement="top">
        <n-grid cols="1 s:3" responsive="screen" :x-gap="12">
          <n-gi>
            <n-form-item label="卖出价">
              <n-input-number
                v-model:value="closeForm.sell_price"
                :min="0"
                :precision="4"
                style="width: 100%"
              />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="手续费">
              <n-input-number v-model:value="closeForm.sell_fee" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="税费(印花税)">
              <n-input-number v-model:value="closeForm.sell_tax" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="卖出日期">
          <n-input v-model:value="closeForm.sell_date" placeholder="YYYY-MM-DD" />
        </n-form-item>
        <n-form-item label="卖出原因">
          <n-input v-model:value="closeForm.sell_reason" placeholder="止盈 / 止损 / 逻辑变化…（可选）" maxlength="512" />
        </n-form-item>

        <!-- 结构化复盘：固定维度，供跨笔统计与自我校准 -->
        <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
          <n-gi>
            <n-form-item label="是否按计划卖出">
              <n-select v-model:value="closeForm.sell_planned" :options="sellPlannedOptions" placeholder="（可选）" clearable />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="当时 AI 判断">
              <n-select v-model:value="closeForm.ai_verdict" :options="aiVerdictOptions" placeholder="（可选）" clearable />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="下次策略调整点">
          <n-input
            v-model:value="closeForm.lesson_learned"
            placeholder="这笔交易教会了什么？下次怎么改？（可选）"
            maxlength="512"
          />
        </n-form-item>
        <n-form-item label="复盘">
          <n-input
            v-model:value="closeForm.review_note"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="这笔交易的复盘总结（可选）"
            maxlength="512"
          />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="closeModal = false">取消</n-button>
          <n-button type="primary" :loading="closingSubmit" @click="submitClose">确认卖出</n-button>
        </div>
      </template>
    </n-modal>

    <!-- B5 加仓 / 减仓 -->
    <n-modal
      v-model:show="tradeModal"
      preset="card"
      :title="`${tradeForm.side === 'buy' ? '加仓' : '减仓'} · ${tradeTarget?.name || '名称待补全'}${tradeTarget?.symbol ? `（${tradeTarget.symbol}）` : ''}`"
      style="max-width: 500px"
    >
      <n-form label-placement="top">
        <div v-if="tradeTarget" class="trade-tip">
          当前持有 <b class="qv-tnum">{{ tradeTarget.quantity }}</b> 股 · 加权成本
          <b class="qv-tnum">{{ fmt(tradeTarget.buy_price) }}</b>
          <span v-if="tradeForm.side === 'buy'">；加仓后成本按 (原成本×原数量 + 本次价×本次数量) / 新数量 重算</span>
          <span v-else>；减仓按当前加权成本结转已实现盈亏，卖出数量不能超过持仓</span>
        </div>
        <n-form-item label="方向">
          <n-radio-group v-model:value="tradeForm.side">
            <n-radio-button value="buy">加仓</n-radio-button>
            <n-radio-button value="sell">减仓</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-grid cols="1 s:3" responsive="screen" :x-gap="12">
          <n-gi>
            <n-form-item label="成交价">
              <n-input-number v-model:value="tradeForm.price" :min="0" :precision="4" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="数量">
              <n-input-number v-model:value="tradeForm.quantity" :min="0" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="日期">
              <n-input v-model:value="tradeForm.trade_date" placeholder="YYYY-MM-DD" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
          <n-gi>
            <n-form-item label="手续费">
              <n-input-number v-model:value="tradeForm.fee" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item :label="tradeForm.side === 'buy' ? '税费' : '税费(印花税)'">
              <n-input-number v-model:value="tradeForm.tax" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="备注">
          <n-input v-model:value="tradeForm.note" placeholder="这笔操作的原因（可选）" maxlength="200" />
        </n-form-item>

        <!-- 减到 0 会自动平仓：就地收集复盘，避免用户以为「减完就没了」而漏掉复盘 -->
        <template v-if="tradeWillClose">
          <n-alert type="info" :bordered="false" style="margin-bottom: 12px">
            本次卖出将清空持仓并自动标记为已平仓，请顺手填写复盘（可选但强烈建议）。
          </n-alert>
          <n-form-item label="卖出原因">
            <n-input v-model:value="tradeForm.sell_reason" placeholder="止盈 / 止损 / 逻辑变化…（可选）" maxlength="512" />
          </n-form-item>
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="是否按计划卖出">
                <n-select v-model:value="tradeForm.sell_planned" :options="sellPlannedOptions" placeholder="（可选）" clearable />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="当时 AI 判断">
                <n-select v-model:value="tradeForm.ai_verdict" :options="aiVerdictOptions" placeholder="（可选）" clearable />
              </n-form-item>
            </n-gi>
          </n-grid>
          <n-form-item label="下次策略调整点">
            <n-input v-model:value="tradeForm.lesson_learned" placeholder="这笔交易教会了什么？（可选）" maxlength="512" />
          </n-form-item>
          <n-form-item label="复盘">
            <n-input
              v-model:value="tradeForm.review_note"
              type="textarea"
              :autosize="{ minRows: 2, maxRows: 4 }"
              placeholder="这笔交易的复盘总结（可选）"
              maxlength="512"
            />
          </n-form-item>
        </template>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="tradeModal = false">取消</n-button>
          <n-button type="primary" :loading="tradeSubmitting" @click="submitTrade">确认</n-button>
        </div>
      </template>
    </n-modal>

    <DataImportWizard
      v-model:show="importModal"
      initial-kind="position"
      @confirmed="onImportChanged"
      @rolled-back="onImportChanged"
    />
  </PageContainer>
</template>

<style scoped>
.pos {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.filters {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.rows {
  display: flex;
  flex-direction: column;
}
.row {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 12px 4px;
  border-bottom: 1px solid var(--qv-divider);
  flex-wrap: wrap;
}
.row:last-child {
  border-bottom: none;
}
.r-name {
  flex: 1;
  min-width: 180px;
}
.r-title-line {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.r-title {
  font-size: 14px;
  font-weight: 500;
}
.r-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.r-sub {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 3px;
}
.r-review {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 2px;
}
.r-hint {
  font-size: 12px;
  font-weight: 500;
  margin-top: 4px;
}
.tag-click {
  cursor: pointer;
}
.signal-line {
  line-height: 1.7;
}
.r-figures {
  display: flex;
  gap: 22px;
}
.r-fig {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 70px;
  text-align: right;
}
.r-fig-label {
  font-size: 11px;
  opacity: 0.5;
}
.r-fig-stale {
  font-size: 11px;
  opacity: 0.55;
  text-decoration: line-through dotted;
}
.r-fig-val {
  font-size: 14px;
  font-weight: 600;
}
.r-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}

@media (max-width: 768px) {
  .rows,
  .row-wrap,
  .row,
  .r-title-line,
  .exit-assessment {
    width: 100%;
    min-width: 0;
    max-width: 100%;
    box-sizing: border-box;
  }
  .row {
    align-items: stretch;
    gap: 10px;
  }
  .r-name,
  .r-figures,
  .r-actions {
    flex: 1 1 100%;
    width: 100%;
    min-width: 0;
    max-width: 100%;
  }
  .r-title,
  .r-sub,
  .r-review,
  .r-hint {
    min-width: 0;
    max-width: 100%;
    overflow-wrap: anywhere;
  }
  /* 行操作区（卖出/分析/复盘/删除等 6 个 tiny 按钮）加大触摸目标 */
  .r-actions {
    flex-wrap: wrap;
    gap: 6px;
    row-gap: 4px;
  }
  .r-actions :deep(.n-button) {
    flex: 0 1 auto;
    max-width: 100%;
    height: 30px;
    padding: 0 10px;
  }
  .r-figures {
    gap: 14px;
    flex-wrap: wrap;
    justify-content: space-between;
  }
  .r-fig {
    flex: 1 1 64px;
    min-width: 0;
    text-align: center;
  }
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
.risk-calc {
  display: flex;
  gap: 14px;
  flex-wrap: wrap;
  font-size: 12.5px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--qv-hover, rgba(128, 128, 128, 0.08));
  margin-bottom: 12px;
}
.risk-hint {
  opacity: 0.6;
}
.checklist {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12.5px;
  border-top: 1px dashed var(--qv-divider);
  padding-top: 10px;
}
.checklist-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.check-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  cursor: pointer;
  line-height: 1.5;
}
.check-item input {
  margin-top: 3px;
  accent-color: v-bind('vars.primaryColor');
}

/* ---------- B7 资产曲线 ---------- */
.curve-chart {
  width: 100%;
  height: 300px;
}
.curve-notes {
  margin-top: 8px;
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.7;
}

/* ---------- C13 行业 / 风格暴露 ---------- */
.exposure-chart {
  width: 100%;
  height: 260px;
}
.exposure-meta {
  margin-top: 6px;
  font-size: 12.5px;
  opacity: 0.75;
}
.exposure-note {
  margin-top: 4px;
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.7;
}

/* ---------- B5 流水明细 ---------- */
.row-wrap {
  border-bottom: 1px solid var(--qv-divider);
  border-radius: 8px;
  transition: background-color 0.18s ease, box-shadow 0.18s ease;
}
.row-wrap:last-child {
  border-bottom: none;
}
.row-wrap.is-stock-action-target {
  background: var(--qv-action-target);
  box-shadow: inset 3px 0 0 var(--qv-action-target-line);
}
.row-wrap .row {
  border-bottom: none;
}
.trade-panel {
  padding: 4px 4px 14px;
  overflow-x: auto;
}
.trade-table {
  width: 100%;
  min-width: 720px;
  border-collapse: collapse;
  font-size: 12px;
}
.trade-table th,
.trade-table td {
  padding: 6px 8px;
  white-space: nowrap;
  border-bottom: 1px dashed var(--qv-divider);
}
.trade-table th {
  font-weight: 600;
  opacity: 0.6;
  text-align: left;
}
.trade-table .ta-r {
  text-align: right;
}
.trade-table .t-note {
  white-space: normal;
  min-width: 120px;
  opacity: 0.75;
}
.trade-tip {
  font-size: 12.5px;
  line-height: 1.7;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--qv-hover, rgba(128, 128, 128, 0.08));
  margin-bottom: 12px;
}

/* ---------- B6 复盘统计 ---------- */
.dist-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 18px;
  margin-top: 16px;
}
.dist-block {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}
.dist-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 2px;
}
.dist-row {
  display: grid;
  grid-template-columns: minmax(80px, 1.1fr) minmax(40px, 1fr) auto;
  grid-template-areas: 'label bar val' 'meta meta meta';
  align-items: center;
  gap: 4px 8px;
  font-size: 12px;
}
.dist-label {
  grid-area: label;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dist-label.is-unknown {
  opacity: 0.55;
  font-style: italic;
}
.dist-bar-wrap {
  grid-area: bar;
  height: 8px;
  border-radius: 4px;
  background: color-mix(in srgb, currentColor 10%, transparent);
  overflow: hidden;
}
.dist-bar {
  height: 100%;
  border-radius: 4px;
}
.dist-val {
  grid-area: val;
  font-weight: 600;
  text-align: right;
}
.dist-meta {
  grid-area: meta;
  font-size: 11px;
  opacity: 0.5;
}
.top-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 18px;
  margin-top: 18px;
}
.top-block,
.lesson-block {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}
.lesson-block {
  margin-top: 18px;
}
.top-row {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
  padding: 4px 0;
  border-bottom: 1px dashed var(--qv-divider);
}
.top-name {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: baseline;
  gap: 6px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.top-meta {
  font-size: 11px;
  opacity: 0.5;
}
.lesson-row {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  font-size: 12.5px;
  padding: 6px 0;
  border-bottom: 1px dashed var(--qv-divider);
}
.lesson-date {
  font-size: 11px;
  opacity: 0.55;
}
.lesson-text {
  flex: 1;
  min-width: 200px;
  line-height: 1.6;
  opacity: 0.85;
  overflow-wrap: anywhere;
}

@media (max-width: 768px) {
  .curve-chart {
    height: 240px;
  }
  /* 行业桶多时手机上要更高才放得下类目标签 */
  .exposure-chart {
    height: 280px;
  }
  .dist-grid,
  .top-grid {
    grid-template-columns: 1fr;
  }
  .lesson-text {
    flex-basis: 100%;
  }
}

/* B8 除权除息待确认 */
.adjust-list {
  display: flex;
  flex-direction: column;
}
.adjust-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 0;
}
.adjust-row + .adjust-row {
  border-top: 1px dashed rgba(128, 128, 128, 0.22);
}
.adjust-main {
  flex: 1;
  min-width: 0;
}
.adjust-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.adjust-name {
  font-size: 14px;
  font-weight: 600;
}
.adjust-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.adjust-plan {
  font-size: 13px;
  opacity: 0.8;
  margin-top: 3px;
}
.adjust-calc {
  font-size: 12px;
  opacity: 0.72;
  margin-top: 3px;
}
.adjust-review {
  margin-top: 5px;
  color: v-bind('vars.errorColor');
  font-size: 12px;
  line-height: 1.55;
}
.adjust-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
@media (max-width: 768px) {
  .adjust-row {
    flex-wrap: wrap;
    row-gap: 6px;
  }
  .adjust-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
}

/* D15 持仓期最高价与回撤 */
.r-peak {
  font-size: 12px;
  opacity: 0.72;
  margin-top: 3px;
}
.r-peak-note {
  opacity: 0.7;
  cursor: help;
}

/* 统一持仓卖出风险事实 */
.exit-assessment {
  margin-top: 8px;
  padding: 8px 10px;
  border-left: 3px solid var(--qv-divider);
  background: rgba(128, 128, 128, 0.06);
  min-width: 0;
}
.exit-assessment.is-watch,
.exit-assessment.is-review {
  border-left-color: v-bind('vars.warningColor');
}
.exit-assessment.is-urgent {
  border-left-color: v-bind('vars.errorColor');
}
.exit-assessment.is-normal {
  border-left-color: v-bind('vars.successColor');
}
.exit-assessment-head,
.exit-action {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
}
.exit-reason {
  min-width: 0;
  font-size: 13px;
  line-height: 1.55;
  font-weight: 600;
  overflow-wrap: anywhere;
}
.exit-asof,
.exit-assessment-empty {
  font-size: 11px;
  opacity: 0.58;
}
.exit-asof {
  flex-shrink: 0;
  text-align: right;
}
.exit-evidence {
  display: flex;
  flex-direction: column;
  gap: 2px;
  margin-top: 5px;
  font-size: 12px;
  line-height: 1.5;
  opacity: 0.76;
  overflow-wrap: anywhere;
}
.exit-gaps {
  margin-top: 5px;
  color: v-bind('vars.warningColor');
  font-size: 12px;
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.exit-action {
  margin-top: 6px;
  font-size: 12px;
  line-height: 1.5;
  opacity: 0.78;
}
.exit-action > span {
  min-width: 0;
  overflow-wrap: anywhere;
}
.exit-action :deep(.n-button) {
  flex-shrink: 0;
}
.exit-assessment-empty {
  margin-top: 5px;
}
@media (max-width: 768px) {
  .exit-assessment-head,
  .exit-action {
    flex-direction: column;
    align-items: stretch;
  }
  .exit-asof {
    text-align: left;
  }
  .exit-action :deep(.n-button) {
    align-self: flex-start;
  }
}

/* D17 AI 卖出建议 */
.advice-empty {
  font-size: 13px;
  opacity: 0.7;
  line-height: 1.7;
}
.advice-box {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.advice-note {
  font-size: 12px;
  opacity: 0.65;
}
.advice-list {
  display: flex;
  flex-direction: column;
}
.advice-row {
  padding: 10px 0;
}
.advice-row + .advice-row {
  border-top: 1px solid var(--qv-divider);
}
.advice-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.advice-name {
  font-size: 14px;
  font-weight: 600;
}
.advice-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.advice-position {
  font-size: 12px;
  opacity: 0.65;
}
.advice-reason {
  font-size: 13px;
  opacity: 0.85;
  margin-top: 5px;
  line-height: 1.7;
}
.advice-invalid {
  font-size: 12px;
  opacity: 0.65;
  margin-top: 4px;
}
.advice-foot {
  font-size: 11px;
  opacity: 0.55;
  border-top: 1px solid var(--qv-divider);
  padding-top: 8px;
}
@media (max-width: 768px) {
  .review-row {
    flex-wrap: wrap;
    row-gap: 6px;
  }
  .review-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
}
</style>
