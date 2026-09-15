<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed, nextTick, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NModal,
  NForm,
  NFormItem,
  NSelect,
  NSwitch,
  NTag,
  NPopconfirm,
  NPopselect,
  NEmpty,
  NSpin,
  NText,
  NAlert,
  useMessage,
} from 'naive-ui'
import {
  listWatchlists,
  createGroup,
  updateGroup,
  deleteGroup,
  addItem,
  updateItem,
  deleteItem,
  setItemStage,
  listMissed,
  type WatchlistGroup,
  type WatchlistItem,
  type WatchlistItemBase,
  type ResearchStage,
  type MissedOpportunity,
} from '@/api/watchlist'
import { getDailyBars, type Bar } from '@/api/market'
import { getSessionEpoch } from '@/api/token'
import { isAbortError } from '@/api/client'
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import {
  idListQuery,
  queryRef,
  replaceRouteQuery,
  useListPageScroll,
  useRouteQueryState,
} from '@/composables/useListPageState'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import ChangeTag from '@/components/ChangeTag.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'
import DataImportWizard from '@/components/DataImportWizard.vue'
import { formatPrice } from '@/lib/formatPrice'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({
  '--qv-divider': vars.value.dividerColor,
  '--qv-action-target': withAlpha(vars.value.primaryColor, 0.12),
  '--qv-action-target-line': vars.value.primaryColor,
}))

const groups = ref<WatchlistGroup[]>([])
const loading = ref(false)
const importModal = ref(false)
const loadError = ref('')
const mutatingItems = ref(new Set<number>())
const removingGroups = ref(new Set<number>())
let loadSeq = 0
let disposed = false
const pageSession = getSessionEpoch()
const pageRouteName = route.name
const active = () => !disposed && route.name === pageRouteName && getSessionEpoch() === pageSession
let listAbort: AbortController | null = null
function invalidateListReads() { loadSeq++; listAbort?.abort(); loading.value = false }
function modalWritePending() { return groupSaving.value || itemSaving.value || passSaving.value }

function applyItemWrite(saved: WatchlistItemBase) {
  invalidateListReads()
  const previous = groups.value.flatMap(group => group.items).find(item => item.id === saved.id)
  for (const group of groups.value) group.items = group.items.filter(item => item.id !== saved.id)
  const group = groups.value.find(group => group.id === saved.watchlist_id)
  if (group) {
    group.items.push({ price: 0, change_pct: 0, quote_ok: false, data_time: '', freshness_status: 'unknown', ...previous, ...saved })
    group.items.sort((a, b) => Number(b.is_pinned) - Number(a.is_pinned) || a.id - b.id)
  }
}

function dropItem(id: number) {
  for (const group of groups.value) group.items = group.items.filter(item => item.id !== id)
  sparkControllers.get(id)?.abort()
  delete expanded.value[id]
  delete sparkBars.value[id]
  delete sparkFetchedAt[id]
}

async function onImportChanged(kind?: string) {
  if (!active()) return
  const returnToOnboarding = route.query.onboarding_return === '1' && kind === 'watchlist'
  invalidateListReads()
  groups.value = []
  await load()
  if (!active()) return
  if (returnToOnboarding && route.query.onboarding_return === '1') {
    await router.push({ name: 'home', query: { onboarding: '1' } })
  }
}

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load(silent = false): Promise<boolean> {
  if (!active() || (silent && loading.value)) return false
  const seq = ++loadSeq
  listAbort?.abort()
  const controller = new AbortController()
  listAbort = controller
  if (!silent) loading.value = true
  try {
    const result = await listWatchlists(controller.signal)
    if (!active() || seq !== loadSeq) return false
    groups.value = result
    loadError.value = ''
    void restoreExpandedSparks()
    return true
  } catch (e) {
    if (active() && seq === loadSeq && !isAbortError(e)) loadError.value = (e as Error).message
    return false
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// 盘中自动刷新行情（60s，仅交易时段+页面可见，静默）。
useAutoRefresh(() => load(true), 60_000)

// ---------- 机会池漏斗 ----------
// 文案静态；颜色取主题语义色（§4.1 禁硬编码色，随 6 套明暗主题自适应）。
// 8 阶段映射 6 个语义变量，保证漏斗相邻阶段不同色即可。
const stageLabels: Record<string, string> = {
  discovered: '发现',
  screening: '初筛',
  watching: '观察',
  waiting_price: '等价格',
  planned: '有计划',
  bought: '已买入',
  passed: '已放弃',
  reviewed: '已复盘',
}
const stageMeta = computed<Record<string, { label: string; color: string }>>(() => ({
  discovered: { label: stageLabels.discovered, color: vars.value.textColor3 },
  screening: { label: stageLabels.screening, color: vars.value.infoColor },
  watching: { label: stageLabels.watching, color: vars.value.primaryColor },
  waiting_price: { label: stageLabels.waiting_price, color: vars.value.warningColor },
  planned: { label: stageLabels.planned, color: vars.value.infoColor },
  bought: { label: stageLabels.bought, color: vars.value.errorColor },
  passed: { label: stageLabels.passed, color: vars.value.textColor3 },
  reviewed: { label: stageLabels.reviewed, color: vars.value.successColor },
}))
const stageOptions = [
  { label: '清除标注', value: '' },
  ...Object.entries(stageLabels).map(([value, label]) => ({ label, value })),
]
// 转「已放弃」时先收集原因。
const passModal = ref(false)
const passReason = ref('')
const passTarget = ref<WatchlistItem | null>(null)
const passSaving = ref(false)

async function onStageSelect(it: WatchlistItem, stage: ResearchStage) {
  if (!active() || modalWritePending() || mutatingItems.value.has(it.id)) return
  if (stage === 'passed') {
    passTarget.value = it
    passReason.value = ''
    passModal.value = true
    return
  }
  mutatingItems.value.add(it.id)
  invalidateListReads()
  try {
    const saved = await setItemStage(it.id, stage)
    if (!active()) return
    applyItemWrite(saved)
    await load(true)
    if (!active()) return
    message.success('研究阶段已更新')
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    mutatingItems.value.delete(it.id)
  }
}
async function confirmPass() {
  if (!active() || !passModal.value || !passTarget.value || modalWritePending() || mutatingItems.value.has(passTarget.value.id)) return
  const target = passTarget.value
  const reason = passReason.value
  passSaving.value = true
  mutatingItems.value.add(target.id)
  invalidateListReads()
  try {
    const saved = await setItemStage(target.id, 'passed', reason)
    if (!active()) return
    applyItemWrite(saved)
    passModal.value = false
    passTarget.value = null
    await load(true)
    if (!active()) return
    message.success('已标记放弃，进入「错过机会」复盘池')
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    passSaving.value = false
    mutatingItems.value.delete(target.id)
  }
}

// ---------- 错过机会复盘 ----------
const missedModal = ref(false)
const missedRows = ref<MissedOpportunity[]>([])
const missedLoading = ref(false)
const missedError = ref('')
let missedSeq = 0
const verdictMeta: Record<string, { label: string; type: 'success' | 'error' | 'default' | 'warning' }> = {
  avoided_loss: { label: '回避正确', type: 'success' },
  missed_gain: { label: '错过上涨', type: 'error' },
  neutral: { label: '基本持平', type: 'default' },
  no_base: { label: '无基准价', type: 'warning' },
  no_quote: { label: '当前行情不可用', type: 'warning' },
  comparison_unknown: { label: '价格口径待核验', type: 'warning' },
  stale_quote: { label: '行情过期，暂无法判定', type: 'warning' },
}
async function openMissed() {
  if (!active()) return
  const seq = ++missedSeq
  missedModal.value = true
  missedRows.value = []
  missedError.value = ''
  missedLoading.value = true
  try {
    const result = await listMissed()
    if (active() && seq === missedSeq && missedModal.value) missedRows.value = result
  } catch (e) {
    if (active() && seq === missedSeq && missedModal.value) missedError.value = (e as Error).message
  } finally {
    if (seq === missedSeq) missedLoading.value = false
  }
}

// ---------- 分组增删改 ----------
const groupModal = ref(false)
const groupForm = ref<{ id: number | null; name: string }>({ id: null, name: '' })

function openCreateGroup() {
  if (!active() || modalWritePending()) return
  groupForm.value = { id: null, name: '' }
  groupModal.value = true
}
function openRenameGroup(g: WatchlistGroup) {
  if (!active() || modalWritePending() || removingGroups.value.has(g.id)) return
  groupForm.value = { id: g.id, name: g.name }
  groupModal.value = true
}
const groupSaving = ref(false)
async function submitGroup() {
  if (!active() || !groupModal.value || groupSaving.value) return
  const { id, name } = groupForm.value
  if (!name.trim()) {
    message.warning('请输入分组名称')
    return
  }
  groupSaving.value = true
  invalidateListReads()
  try {
    const saved = id ? await updateGroup(id, name.trim()) : await createGroup(name.trim())
    if (!active()) return
    invalidateListReads()
    const existing = groups.value.find(group => group.id === saved.id)
    if (existing) Object.assign(existing, { name: saved.name, sort_order: saved.sort_order })
    else groups.value.push({ ...saved, items: [] })
    groupModal.value = false
    await load()
    if (!active()) return
    message.success('已保存')
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    groupSaving.value = false
  }
}
async function removeGroup(g: WatchlistGroup) {
  if (!active() || modalWritePending() || removingGroups.value.has(g.id)) return
  removingGroups.value.add(g.id)
  invalidateListReads()
  try {
    await deleteGroup(g.id)
    if (!active()) return
    invalidateListReads()
    for (const id of g.items.map(item => item.id)) dropItem(id)
    groups.value = groups.value.filter(group => group.id !== g.id)
    await load()
    if (!active()) return
    message.success('分组已删除')
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    removingGroups.value.delete(g.id)
  }
}

// ---------- 条目增删改 ----------
const itemModal = ref(false)
const itemEditing = ref(false)
const itemForm = ref<{
  id: number | null
  watchlist_id: number
  symbol: string
  market: string
  name: string
  focus_reason: string
  note: string
  is_pinned: boolean
}>({ id: null, watchlist_id: 0, symbol: '', market: 'cn', name: '', focus_reason: '', note: '', is_pinned: false })

const groupSelectOptions = computed(() =>
  groups.value.map((g) => ({ label: g.name, value: g.id })),
)

function openAddItem(
  groupId?: number,
  prefill?: { symbol?: string; market?: string; name?: string },
) {
  if (!active() || modalWritePending()) return
  itemEditing.value = false
  itemForm.value = {
    id: null,
    watchlist_id: groupId || groups.value[0]?.id || 0,
    symbol: prefill?.symbol || '',
    market: prefill?.market || 'cn',
    name: prefill?.name || '',
    focus_reason: '',
    note: '',
    is_pinned: false,
  }
  itemModal.value = true
}
function openEditItem(item: WatchlistItem) {
  if (!active() || modalWritePending() || mutatingItems.value.has(item.id)) return
  itemEditing.value = true
  itemForm.value = {
    id: item.id,
    watchlist_id: item.watchlist_id,
    symbol: item.symbol,
    market: item.market,
    name: item.name,
    focus_reason: item.focus_reason,
    note: item.note,
    is_pinned: item.is_pinned,
  }
  itemModal.value = true
}
const itemSaving = ref(false)
async function submitItem() {
  if (!active() || !itemModal.value || itemSaving.value) return
  const f = { ...itemForm.value }
  const editing = itemEditing.value
  const returnToOnboarding = route.query.onboarding_return === '1'
  if (!f.watchlist_id) { message.warning('请选择自选分组'); return }
  if (!editing && !f.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  itemSaving.value = true
  invalidateListReads()
  try {
    let saved: WatchlistItemBase
    if (editing && f.id) {
      saved = await updateItem(f.id, {
        watchlist_id: f.watchlist_id,
        focus_reason: f.focus_reason,
        note: f.note,
        is_pinned: f.is_pinned,
      })
    } else {
      saved = await addItem(f.watchlist_id, {
        symbol: f.symbol.trim(),
        market: f.market,
        name: f.name.trim() || undefined,
        focus_reason: f.focus_reason,
        note: f.note,
        is_pinned: f.is_pinned,
      })
    }
    if (!active()) return
    applyItemWrite(saved)
    itemModal.value = false
    await load()
    if (!active()) return
    message.success('已保存')
    if (!editing && returnToOnboarding && route.query.onboarding_return === '1') {
      await router.push({ name: 'home', query: { onboarding: '1' } })
    }
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    itemSaving.value = false
  }
}
async function togglePin(item: WatchlistItem) {
  if (!active() || modalWritePending() || mutatingItems.value.has(item.id)) return
  mutatingItems.value.add(item.id)
  invalidateListReads()
  try {
    const saved = await updateItem(item.id, {
      is_pinned: !item.is_pinned,
    })
    if (!active()) return
    applyItemWrite(saved)
    await load()
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    mutatingItems.value.delete(item.id)
  }
}
async function removeItem(item: WatchlistItem) {
  if (!active() || modalWritePending() || mutatingItems.value.has(item.id)) return
  mutatingItems.value.add(item.id)
  invalidateListReads()
  try {
    await deleteItem(item.id)
    if (!active()) return
    invalidateListReads()
    dropItem(item.id)
    await load()
    if (!active()) return
    message.success('已移除')
  } catch (e) {
    if (active()) message.error((e as Error).message)
  } finally {
    mutatingItems.value.delete(item.id)
  }
}

// 从自选一键建仓：跳转持仓页并预填。
function buildPosition(item: WatchlistItem) {
  router.push({
    name: 'positions',
    query: { add: '1', symbol: item.symbol, market: item.market, name: item.name },
  })
}
// 快捷入口：分析/提醒/问答页均已支持 query 预填（PRD 3.3/3.16/3.17 的跳转交互）。
function goAnalysis(item: WatchlistItem) {
  router.push({ name: 'analysis', query: { module: 'stock', symbol: item.symbol, market: item.market } })
}
function goAlert(item: WatchlistItem) {
  router.push({ name: 'alerts', query: { add: '1', symbol: item.symbol, market: item.market, name: item.name } })
}
function goQa(item: WatchlistItem) {
  router.push({ name: 'qa', query: { symbol: item.symbol, market: item.market } })
}
function goThesis(item: WatchlistItem) {
  router.push({ name: 'thesis', query: { add: '1', symbol: item.symbol, market: item.market, name: item.name } })
}

function fmt(n: number | undefined) {
  return formatPrice(n)
}

// ---------- 行展开迷你走势：按需加载单只近 60 日日线，避开数据源限流 ----------
const expanded = ref<Record<number, boolean>>({})
const sparkBars = ref<Record<number, Bar[]>>({})
const sparkLoading = ref<Record<number, boolean>>({})
const sparkFetchedAt: Record<number, number> = {}
const sparkRequests = new Map<number, Promise<void>>()
const sparkControllers = new Map<number, AbortController>()
const sparkCacheMs = 10 * 60_000
const expandedIDs = computed<number[]>({
  get: () =>
    Object.entries(expanded.value)
      .filter(([, open]) => open)
      .map(([id]) => Number(id))
      .filter((id) => Number.isInteger(id) && id > 0)
      .sort((a, b) => a - b),
  set: (ids) => {
    expanded.value = Object.fromEntries(ids.map((id) => [id, true]))
  },
})

useRouteQueryState(route, router, [queryRef('expanded', expandedIDs, idListQuery(8))])
const { restoreScroll } = useListPageScroll(route, 'watchlist')

async function loadSpark(it: WatchlistItem): Promise<void> {
  if (!active()) return
  const pending = sparkRequests.get(it.id)
  if (pending) {
    if (!sparkControllers.get(it.id)?.signal.aborted) return pending
    await pending
    if (sparkRequests.get(it.id) === pending) sparkRequests.delete(it.id)
    if (active() && expanded.value[it.id]) return loadSpark(it)
    return
  }
  const cached = sparkBars.value[it.id]
  const lastDate = cached?.[cached.length - 1]?.trade_date || ''
  const quoteDate = it.data_time?.slice(0, 10) || ''
  if (cached?.length && Date.now() - (sparkFetchedAt[it.id] || 0) < sparkCacheMs && (!quoteDate || quoteDate <= lastDate)) return
  const request = (async () => {
    const controller = new AbortController()
    sparkControllers.set(it.id, controller)
    sparkLoading.value[it.id] = true
    try {
      const result = await getDailyBars(it.market, it.symbol, 60, controller.signal)
      if (!active() || !groups.value.some(group => group.items.some(item => item.id === it.id))) return
      sparkBars.value[it.id] = result
      sparkFetchedAt[it.id] = Date.now()
    } catch (e) {
      if (active() && expanded.value[it.id] && !isAbortError(e)) {
        expanded.value[it.id] = false
        message.error((e as Error).message)
      }
    } finally {
      sparkLoading.value[it.id] = false
      if (sparkControllers.get(it.id) === controller) sparkControllers.delete(it.id)
    }
  })()
  sparkRequests.set(it.id, request)
  try { await request } finally { if (sparkRequests.get(it.id) === request) sparkRequests.delete(it.id) }
}

async function toggleExpand(it: WatchlistItem) {
  if (!active()) return
  if (expanded.value[it.id]) {
    expanded.value[it.id] = false
    sparkControllers.get(it.id)?.abort()
    return
  }
  expanded.value[it.id] = true
  await loadSpark(it)
}

async function restoreExpandedSparks() {
  if (!active()) return
  const items = new Map(groups.value.flatMap((group) => group.items).map((item) => [item.id, item]))
  const valid = expandedIDs.value.filter((id) => items.has(id))
  if (valid.length !== expandedIDs.value.length) expandedIDs.value = valid
  // 顺序恢复，避免一次返回页面并发打出多只日线请求。
  for (const id of valid) {
    if (!active() || !expanded.value[id]) continue
    const item = items.get(id)
    if (item) await loadSpark(item)
  }
}

watch(expandedIDs, () => {
  if (groups.value.length) void restoreExpandedSparks()
})

// 近 60 日收盘价 → SVG 路径（viewBox 560x72，preserveAspectRatio=none 拉伸自适应）
const SPARK_W = 560
const SPARK_H = 72
function sparkPaths(bars: Bar[]) {
  const closes = bars.map((b) => b.close)
  if (closes.length < 2) return null
  const min = Math.min(...closes)
  const max = Math.max(...closes)
  const span = max - min || 1
  const stepX = SPARK_W / (closes.length - 1)
  const pts = closes.map(
    (c, i) => `${(i * stepX).toFixed(1)},${(SPARK_H - 5 - ((c - min) / span) * (SPARK_H - 10)).toFixed(1)}`,
  )
  const line = 'M' + pts.join(' L')
  return { line, area: `${line} L${SPARK_W},${SPARK_H} L0,${SPARK_H} Z` }
}
function sparkStats(bars: Bar[]) {
  const first = bars[0]
  const last = bars[bars.length - 1]
  const chg = first?.close ? ((last.close - first.close) / first.close) * 100 : 0
  return {
    chg,
    high: Math.max(...bars.map((b) => b.high)),
    low: Math.min(...bars.map((b) => b.low)),
  }
}

const totalCount = computed(() => groups.value.reduce((s, g) => s + g.items.length, 0))

const highlightedItemID = ref<number | null>(null)
let lastConsumedStockAction = ''
let activeStockAction = ''
let stockActionLoad: Promise<boolean> | null = null

function stockActionKey() {
  const symbol = String(route.query.symbol || '').trim()
  if (!symbol && route.query.add !== '1') return ''
  return String(route.query._stock_action || '') || [symbol, route.query.market || 'cn', route.query.add || ''].join(':')
}

function ensureStockActionLoad() {
  if (!stockActionLoad) {
    stockActionLoad = load().finally(() => {
      stockActionLoad = null
    })
  }
  return stockActionLoad
}

function watchlistItemElementID(id: number) {
  return `watchlist-item-${id}`
}

async function applyStockActionQuery(): Promise<boolean> {
  if (!active() || modalWritePending()) return false
  const actionKey = stockActionKey()
  if (!actionKey || actionKey === lastConsumedStockAction || actionKey === activeStockAction) return false

  activeStockAction = actionKey
  try {
    const loaded = await ensureStockActionLoad()
    if (!active() || !loaded || stockActionKey() !== actionKey || modalWritePending()) return true

    const symbol = String(route.query.symbol || '').trim().toLowerCase()
    const market = String(route.query.market || 'cn').trim().toLowerCase()
    const target = groups.value
      .flatMap((group) => group.items)
      .find(
        (item) =>
          item.symbol.trim().toLowerCase() === symbol &&
          (item.market || 'cn').trim().toLowerCase() === market,
      )

    lastConsumedStockAction = actionKey
    if (target) {
      highlightedItemID.value = target.id
      await nextTick()
      if (!active() || stockActionKey() !== actionKey) return true
      document.getElementById(watchlistItemElementID(target.id))?.scrollIntoView({
        behavior: 'smooth',
        block: 'center',
      })
    } else {
      highlightedItemID.value = null
      openAddItem(undefined, {
        symbol: String(route.query.symbol || ''),
        market: String(route.query.market || 'cn'),
        name: String(route.query.name || ''),
      })
    }
    await replaceRouteQuery(route, router, {
      symbol: undefined,
      market: undefined,
      name: undefined,
      add: undefined,
      _stock_action: undefined,
    })
    return true
  } finally {
    if (activeStockAction === actionKey) activeStockAction = ''
  }
}

watch(() => [route.query._stock_action, route.query.symbol, route.query.market, route.query.add, groupSaving.value, itemSaving.value, passSaving.value], () => void applyStockActionQuery())
watch(missedModal, show => { if (!show) { missedSeq++; missedLoading.value = false } })

onMounted(async () => {
  const handledAction = await applyStockActionQuery()
  if (!handledAction) await ensureStockActionLoad()
  if (!active()) return
  await restoreExpandedSparks()
  if (!active()) return
  if (!handledAction) await restoreScroll()
})
onBeforeUnmount(() => {
  disposed = true
  invalidateListReads()
  missedSeq++
  for (const controller of sparkControllers.values()) controller.abort()
})
</script>

<template>
  <PageContainer title="自选股" subtitle="整理关注名单，比较行情变化，继续研究值得跟踪的机会。">
    <template #actions>
      <n-tag size="small" round :bordered="false">{{ totalCount }} 只</n-tag>
      <n-button size="small" secondary @click="openCreateGroup">新建分组</n-button>
      <n-button size="small" type="primary" @click="openAddItem()">+ 添加自选</n-button>
      <n-button size="small" quaternary @click="importModal = true">导入</n-button>
      <n-button size="small" secondary @click="openMissed">错过机会</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <n-alert v-if="loadError" type="warning" style="margin-bottom: 12px">{{ loadError }}；当前列表可能尚未更新。</n-alert>

    <n-spin :show="loading && !groups.length">
      <div class="wl" :style="styleVars">
        <SectionCard v-for="g in groups" :key="g.id" :title="g.name">
          <template #extra>
            <div class="group-actions">
              <n-tag size="tiny" round :bordered="false">{{ g.items.length }}</n-tag>
              <n-button size="tiny" quaternary @click="openAddItem(g.id)">添加</n-button>
              <n-button size="tiny" quaternary :disabled="removingGroups.has(g.id)" @click="openRenameGroup(g)">重命名</n-button>
              <n-popconfirm @positive-click="removeGroup(g)">
                <template #trigger>
                  <n-button size="tiny" quaternary type="error" :disabled="removingGroups.has(g.id)">删除</n-button>
                </template>
                删除分组「{{ g.name }}」及其下 {{ g.items.length }} 只自选？
              </n-popconfirm>
            </div>
          </template>

          <n-empty v-if="!g.items.length" description="该分组暂无自选，点击「添加」加入股票" />
          <div v-else class="items">
            <div
              v-for="it in g.items"
              :id="watchlistItemElementID(it.id)"
              :key="it.id"
              class="item-wrap"
              :class="{ 'is-stock-action-target': highlightedItemID === it.id }"
            >
              <div class="item" @click="toggleExpand(it)">
                <button type="button" class="it-chevron" :class="{ 'is-open': expanded[it.id] }" :aria-expanded="!!expanded[it.id]" :aria-label="`${expanded[it.id] ? '收起' : '展开'}${it.name || it.symbol}的走势图`" @click.stop="toggleExpand(it)">▸</button>
                <div class="it-main">
                  <div class="it-name">
                    <n-tag v-if="it.is_pinned" size="tiny" type="warning" round :bordered="false"
                      >重点</n-tag
                    >
                    <StockIdentity :symbol="it.symbol" :market="it.market" :name="it.name" density="table" />
                    <n-popselect
                      :value="it.research_stage"
                      :options="stageOptions"
                      trigger="click"
                      :disabled="mutatingItems.has(it.id)"
                      @update:value="(v: ResearchStage) => onStageSelect(it, v)"
                    >
                      <n-tag
                        size="tiny"
                        round
                        :bordered="false"
                        style="cursor: pointer"
                        :color="it.research_stage && stageMeta[it.research_stage]
                          ? { color: withAlpha(stageMeta[it.research_stage].color, 0.15), textColor: stageMeta[it.research_stage].color }
                          : undefined"
                        @click.stop
                      >
                        {{ it.research_stage && stageMeta[it.research_stage] ? stageMeta[it.research_stage].label : '标阶段' }}
                      </n-tag>
                    </n-popselect>
                  </div>
                  <div v-if="it.focus_reason || it.note" class="it-note">
                    <span v-if="it.focus_reason">关注：{{ it.focus_reason }}</span>
                    <span v-if="it.note" class="it-memo">· {{ it.note }}</span>
                  </div>
                </div>
                <div class="it-quote">
                  <span v-if="it.quote_ok" class="it-price qv-tnum" :style="{ color: pctColor(it.change_pct) }">
                    {{ fmt(it.price) }}
                  </span>
                  <span v-else class="it-price muted">—</span>
                  <ChangeTag v-if="it.quote_ok" :value="it.change_pct" size="small" />
                  <FreshnessTag
                    :status="it.freshness_status"
                    :as-of="it.data_time ? String(it.data_time).slice(0, 16).replace('T', ' ') : ''"
                  />
                </div>
                <div class="it-actions" @click.stop>
                  <n-button size="tiny" quaternary :disabled="mutatingItems.has(it.id)" @click="togglePin(it)">{{
                    it.is_pinned ? '取消重点' : '重点'
                  }}</n-button>
                  <n-button size="tiny" quaternary @click="goAnalysis(it)">分析</n-button>
                  <n-button size="tiny" quaternary @click="goAlert(it)">提醒</n-button>
                  <n-button size="tiny" quaternary @click="goQa(it)">问答</n-button>
                  <n-button size="tiny" quaternary @click="goThesis(it)">逻辑卡</n-button>
                  <n-button size="tiny" quaternary @click="buildPosition(it)">建仓</n-button>
                  <n-button size="tiny" quaternary :disabled="mutatingItems.has(it.id)" @click="openEditItem(it)">编辑</n-button>
                  <n-popconfirm @positive-click="removeItem(it)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error" :disabled="mutatingItems.has(it.id)">移除</n-button>
                    </template>
                    从自选中移除「{{ it.name || '名称待补全' }}（{{ it.symbol }}）」？
                  </n-popconfirm>
                </div>
              </div>

              <!-- 展开：近 60 日迷你走势（按需加载） -->
              <div v-if="expanded[it.id]" class="item-spark">
                <n-spin :show="!!sparkLoading[it.id]" size="small">
                  <template v-if="(sparkBars[it.id]?.length || 0) >= 2">
                    <svg
                      class="spark-svg"
                      :viewBox="`0 0 ${SPARK_W} ${SPARK_H}`"
                      preserveAspectRatio="none"
                      aria-hidden="true"
                    >
                      <path
                        :d="sparkPaths(sparkBars[it.id])!.area"
                        :fill="withAlpha(pctColor(sparkStats(sparkBars[it.id]).chg), 0.12)"
                      />
                      <path
                        :d="sparkPaths(sparkBars[it.id])!.line"
                        fill="none"
                        :stroke="pctColor(sparkStats(sparkBars[it.id]).chg)"
                        stroke-width="1.5"
                        vector-effect="non-scaling-stroke"
                      />
                    </svg>
                    <div class="spark-meta qv-tnum">
                      <span>近 {{ sparkBars[it.id].length }} 个交易日 · 截至 {{ sparkBars[it.id][sparkBars[it.id].length - 1].trade_date }}</span>
                      <span :style="{ color: pctColor(sparkStats(sparkBars[it.id]).chg) }">
                        区间 {{ sparkStats(sparkBars[it.id]).chg >= 0 ? '+' : '' }}{{ sparkStats(sparkBars[it.id]).chg.toFixed(2) }}%
                      </span>
                      <span>最高 {{ fmt(sparkStats(sparkBars[it.id]).high) }}</span>
                      <span>最低 {{ fmt(sparkStats(sparkBars[it.id]).low) }}</span>
                    </div>
                  </template>
                  <div v-else class="spark-placeholder">
                    <span v-if="!sparkLoading[it.id]">日线不足两条，暂无法绘制走势</span>
                  </div>
                </n-spin>
              </div>
            </div>
          </div>
        </SectionCard>
      </div>
    </n-spin>

    <!-- 分组新建/重命名 -->
    <n-modal
      v-model:show="groupModal"
      class="watchlist-dialog"
      :style="styleVars"
      preset="card"
      :title="groupForm.id ? '重命名分组' : '新建分组'"
      style="max-width: 420px"
      :mask-closable="!groupSaving"
      :close-on-esc="!groupSaving"
      :closable="!groupSaving"
    >
      <n-form :disabled="groupSaving" @submit.prevent="submitGroup">
        <n-form-item label="分组名称">
          <n-input v-model:value="groupForm.name" placeholder="如：核心持仓 / 观察池" maxlength="32" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button :disabled="groupSaving" @click="groupModal = false">取消</n-button>
          <n-button type="primary" :loading="groupSaving" @click="submitGroup">保存</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 标记放弃：记录原因与当时价格 -->
    <n-modal v-model:show="passModal" class="watchlist-dialog" :style="styleVars" preset="card" title="标记放弃" style="max-width: 460px" :mask-closable="!passSaving" :close-on-esc="!passSaving" :closable="!passSaving">
      <p class="pass-hint">
        <template v-if="passTarget?.research_stage === 'passed'">该条目已标记放弃；本次补充原因，保留原参照价格和时间。</template>
        <template v-else>将「{{ passTarget?.name || '名称待补全' }}（{{ passTarget?.symbol || '代码未知' }}）」标记为已放弃。系统会记录当前有效价格，之后可在「错过机会」中复盘；缺少有效价格或复权依据时不做确定判断。</template>
      </p>
      <n-input
        v-model:value="passReason"
        :disabled="passSaving"
        :maxlength="250"
        type="textarea"
        :rows="2"
        placeholder="放弃原因（建议填写）：如估值过高 / 逻辑存疑 / 仓位已满"
      />
      <template #footer>
        <div class="modal-footer">
          <n-button :disabled="passSaving" @click="passModal = false">取消</n-button>
          <n-button type="warning" :loading="passSaving" @click="confirmPass">确认放弃</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 错过机会复盘 -->
    <n-modal v-model:show="missedModal" class="watchlist-dialog" :style="styleVars" preset="card" title="错过机会复盘" style="max-width: 720px">
      <n-spin :show="missedLoading">
        <n-alert v-if="missedError" type="warning">{{ missedError }}</n-alert>
        <n-empty v-else-if="!missedLoading && !missedRows.length" description="还没有已放弃的标的——在自选条目上把阶段标为「已放弃」后，这里会记录并跟踪" />
        <div v-else class="missed-list">
          <div v-for="m in missedRows" :key="m.id" class="missed-row">
            <div class="missed-main">
              <div class="missed-name">
                <StockIdentity :symbol="m.symbol" :market="m.market" :name="m.name" density="table" clickable actions />
                <n-tag size="tiny" :type="verdictMeta[m.verdict]?.type" round :bordered="false">
                  {{ verdictMeta[m.verdict]?.label }}
                </n-tag>
              </div>
              <div v-if="m.passed_reason" class="missed-reason">放弃原因：{{ m.passed_reason }}</div>
              <div v-if="m.comparison_note" class="missed-reason">{{ m.comparison_note }}</div>
            </div>
            <div class="missed-nums qv-tnum">
              <span>放弃价 {{ m.passed_price > 0 ? formatPrice(m.passed_price) : '—' }}</span>
              <span>现价 {{ m.quote_ok ? formatPrice(m.current_price) : '—' }}</span>
              <span
                v-if="!m.quote_ok && m.verdict === 'stale_quote' && m.last_price"
                :title="`最近已知价（截至 ${m.quote_as_of || '未知'}，已过期，不参与结论）`"
                >旧 {{ formatPrice(m.last_price) }}</span
              >
              <span v-if="m.passed_price > 0 && m.quote_ok && ['missed_gain', 'avoided_loss', 'neutral'].includes(m.verdict)" :style="{ color: pctColor(m.change_since_pct) }">
                {{ m.change_since_pct >= 0 ? '+' : '' }}{{ m.change_since_pct.toFixed(2) }}%
              </span>
            </div>
          </div>
        </div>
      </n-spin>
    </n-modal>

    <!-- 条目添加/编辑 -->
    <n-modal
      v-model:show="itemModal"
      class="watchlist-dialog"
      :style="styleVars"
      preset="card"
      :title="itemEditing ? '编辑自选' : '添加自选'"
      style="max-width: 460px"
      :mask-closable="!itemSaving"
      :close-on-esc="!itemSaving"
      :closable="!itemSaving"
    >
      <n-form label-placement="top" :disabled="itemSaving">
        <n-form-item label="所属分组">
          <n-select v-model:value="itemForm.watchlist_id" :options="groupSelectOptions" />
        </n-form-item>
        <n-form-item v-if="!itemEditing" label="股票代码">
          <n-input v-model:value="itemForm.symbol" placeholder="如 600000" />
        </n-form-item>
        <n-form-item v-if="!itemEditing" label="市场">
          <n-select v-model:value="itemForm.market" :options="marketOptions" />
        </n-form-item>
        <n-form-item label="关注原因">
          <n-input
            v-model:value="itemForm.focus_reason"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="为什么关注它（可选）"
            maxlength="500"
          />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="itemForm.note" placeholder="补充备注（可选）" maxlength="500" />
        </n-form-item>
        <n-form-item label="重点关注">
          <n-switch v-model:value="itemForm.is_pinned" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button :disabled="itemSaving" @click="itemModal = false">取消</n-button>
          <n-button type="primary" :loading="itemSaving" @click="submitItem">保存</n-button>
        </div>
      </template>
    </n-modal>

    <DataImportWizard
      v-model:show="importModal"
      initial-kind="watchlist"
      @confirmed="onImportChanged"
      @rolled-back="onImportChanged"
    />

    <n-text v-if="!loading && !loadError && !groups.length" depth="3">暂无数据</n-text>
  </PageContainer>
</template>

<style scoped>
:global(.watchlist-dialog.n-card) { max-height: calc(100dvh - 24px); }
:global(.watchlist-dialog.n-card > .n-card-content) { min-height: 0; overflow-y: auto; }
:global(.watchlist-dialog.n-card > .n-card-header),
:global(.watchlist-dialog.n-card > .n-card__footer) { flex-shrink: 0; }
.wl {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.group-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  justify-content: flex-end;
}
.items {
  display: flex;
  flex-direction: column;
}
.item-wrap {
  border-bottom: 1px solid var(--qv-divider);
  border-radius: 8px;
  transition: background-color 0.18s ease, box-shadow 0.18s ease;
}
.item-wrap:last-child {
  border-bottom: none;
}
.item-wrap.is-stock-action-target {
  background: var(--qv-action-target);
  box-shadow: inset 3px 0 0 var(--qv-action-target-line);
}
.item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  cursor: pointer;
  border-radius: 8px;
  transition: background-color 0.15s ease;
}
.item:hover {
  background: rgba(128, 128, 128, 0.06);
}
.it-chevron {
  border: 0;
  padding: 0;
  background: transparent;
  color: inherit;
  cursor: pointer;
  height: 30px;
  flex-shrink: 0;
  width: 14px;
  font-size: 11px;
  opacity: 0.4;
  transition: transform 0.18s ease;
}
.it-chevron.is-open {
  transform: rotate(90deg);
}
.item-spark {
  padding: 2px 4px 12px 30px;
}
.spark-svg {
  display: block;
  width: 100%;
  height: 72px;
}
.spark-placeholder {
  height: 72px;
  display: flex;
  align-items: center;
  justify-content: center;
  opacity: 0.65;
}
.spark-meta {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 12px;
  opacity: 0.72;
}
.it-main {
  flex: 1;
  min-width: 0;
}
.it-name {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  flex-wrap: wrap;
}
.it-note {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.it-memo {
  opacity: 0.7;
}
.it-quote {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 120px;
  justify-content: flex-end;
}
.it-price {
  font-size: 15px;
  font-weight: 600;
}
.it-price.muted {
  opacity: 0.4;
}
.it-actions {
  display: flex;
  align-items: center;
  gap: 2px;
  flex-shrink: 0;
}

@media (max-width: 768px) {
  .item {
    flex-wrap: wrap;
    row-gap: 2px;
  }
  .it-name {
    flex-wrap: wrap;
  }
  .it-quote {
    min-width: 0;
  }
  /* 操作按钮整行换到下一行，与内容左对齐；加大触摸目标（tiny 按钮 22px 太小） */
  .it-actions {
    flex-basis: 100%;
    flex-wrap: wrap;
    padding-left: 18px;
    gap: 6px;
    row-gap: 4px;
  }
  .it-actions :deep(.n-button),
  .group-actions :deep(.n-button) {
    height: 30px;
    padding: 0 10px;
  }
  /* 错过机会弹窗：名称与数字并排 nowrap 会溢出 360px 弹窗，改上下两行 */
  .missed-row {
    flex-wrap: wrap;
    row-gap: 4px;
  }
  .missed-nums {
    flex-basis: 100%;
    flex-wrap: wrap;
    white-space: normal;
    justify-content: flex-start;
  }
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
.pass-hint {
  margin: 0 0 10px;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.8;
}
.missed-list {
  display: flex;
  flex-direction: column;
}
.missed-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.missed-row:last-child {
  border-bottom: none;
}
.missed-main {
  flex: 1;
  min-width: 0;
}
.missed-name {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-width: 0;
}
.missed-reason {
  font-size: 12px;
  opacity: 0.65;
  margin-top: 2px;
  overflow-wrap: anywhere;
}
.missed-nums {
  display: flex;
  gap: 12px;
  flex-shrink: 0;
  font-size: 12.5px;
  white-space: nowrap;
}
</style>
