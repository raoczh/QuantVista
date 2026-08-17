<script setup lang="ts">
import { ref, computed, onBeforeUnmount, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NTag,
  NTable,
  NGrid,
  NGi,
  NModal,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NSwitch,
  NSpin,
  NEmpty,
  NPopover,
  NPopconfirm,
  NAlert,
  NCheckbox,
  useMessage,
} from 'naive-ui'
import {
  getScreenerStrategies,
  getScreenerStrategyHistory,
  screenerScan,
  getScreenerResult,
  listScreenerResults,
  saveScreenerStrategy,
  deleteScreenerStrategy,
  getScreenerStatus,
  parseScreenerStrategy,
  createWatchlistBatch,
  undoWatchlistBatch,
  PERIOD_LABEL,
  RISK_LABEL,
  RISK_TAG_TYPE,
  type StrategiesView,
  type BuiltinStrategy,
  type RetailTemplate,
  type CustomStrategy,
  type ScanResult,
  type ScanRequest,
  type FactorTableStatus,
  type FactorDef,
  type CondNode,
  type ParseStrategyResult,
  type ScreenerStrategyHistory,
  type ScreenerStrategyRevision,
  type StrategyRun,
  type WatchlistBatch,
} from '@/api/screener'
import { listWatchlists, type WatchlistGroup } from '@/api/watchlist'
import { taskStatusLabel } from '@/api/taskCenter'
import { ApiRequestError } from '@/api/client'
import { getLLMTask, listLLMTasks, type LLMTask } from '@/api/llmTask'
import { isPollCancelled, pollUntil } from '@/lib/poll'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { useIsMobile } from '@/composables/useIsMobile'
import { useStockActions } from '@/composables/useStockActions'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'
import StockIdentity from '@/components/StockIdentity.vue'

const message = useMessage()
const router = useRouter()
const route = useRoute()
const { vars } = useUi()
const { llmLabel } = useLlmLabel()
const { isMobile } = useIsMobile()
const actions = useStockActions()
const styleVars = computed(() => ({
  '--qv-divider': vars.value.dividerColor,
  '--qv-warning': vars.value.warningColor,
  '--qv-code-bg': vars.value.actionColor,
}))

// ---------- 策略广场与宽表状态 ----------

const data = ref<StrategiesView | null>(null)
const status = ref<FactorTableStatus | null>(null)
const scanHistory = ref<StrategyRun<ScanResult>[]>([])
const loading = ref(false)

const periodFilter = ref<'all' | 'short' | 'swing' | 'mid'>('all')
const builtinFiltered = computed<BuiltinStrategy[]>(() => {
  const list = data.value?.builtin ?? []
  if (periodFilter.value === 'all') return list
  return list.filter((b) => b.period === periodFilter.value)
})
const customList = computed<CustomStrategy[]>(() => data.value?.custom ?? [])
const retailTemplates = computed<RetailTemplate[]>(() => data.value?.retail_templates ?? [])
const factors = computed<FactorDef[]>(() => data.value?.factors ?? [])
const retailParamValues = ref<Record<string, Record<string, number>>>({})

async function load() {
  loading.value = true
  try {
    ;[data.value, status.value, scanHistory.value] = await Promise.all([
      getScreenerStrategies(),
      getScreenerStatus(),
      listScreenerResults(20).catch(() => scanHistory.value),
    ])
    for (const template of data.value?.retail_templates ?? []) {
      if (retailParamValues.value[template.key]) continue
      retailParamValues.value[template.key] = Object.fromEntries(template.params.map((param) => [param.key, param.default]))
    }
    const resultId = Number(Array.isArray(route.query.result_id) ? route.query.result_id[0] : route.query.result_id)
    if (Number.isSafeInteger(resultId) && resultId > 0) await openScanResult(resultId, true)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

const statusText = computed(() => {
  const s = status.value
  if (!s) return ''
  if (s.building) return '因子宽表构建中…'
  if (!s.ready) return '因子宽表未构建：首次扫描会自动构建（需全市场日线数据，约 5~10 秒）'
  return `数据日期 ${s.trade_date} · 覆盖 ${s.universe} 只 · ${s.factors} 个因子 · 构建耗时 ${((s.build_ms ?? 0) / 1000).toFixed(1)}s`
})

// ---------- 扫描 ----------

const scanning = ref('') // 正在扫描的 strategy_key / `custom-{id}` / 'temp'
const result = ref<ScanResult | null>(null)
const includeST = ref(false)
const includeStale = ref(false)
interface ScanTarget {
  template_key?: string
  template_version?: number
  template_params?: Record<string, number>
  strategy_key?: string
  strategy_id?: number
  strategy_revision_id?: number
  tree?: CondNode
}
// 最近一次扫描目标（开关切换后重扫用）。
let lastScanTarget: ScanTarget | null = null
let scanPollAbort: AbortController | null = null
onBeforeUnmount(() => scanPollAbort?.abort())

function scanTargetFromRequest(request?: ScanRequest | Record<string, unknown>): ScanTarget | null {
  if (!request) return null
  const req = request as ScanRequest
  if (req.template_key) {
    return {
      template_key: req.template_key,
      template_version: req.template_version,
      template_params: req.template_params,
    }
  }
  if (req.strategy_key) return { strategy_key: req.strategy_key }
  if (req.strategy_id) return { strategy_id: req.strategy_id, strategy_revision_id: req.strategy_revision_id }
  if (req.tree) return { tree: req.tree }
  return null
}

async function openScanResult(id: number, trackRunning = false) {
  scanPollAbort?.abort()
  const controller = new AbortController()
  scanPollAbort = controller
  try {
    let run = await getScreenerResult(id)
    if (trackRunning && (run.status === 'queued' || run.status === 'running')) {
      run = await pollUntil(() => getScreenerResult(id), (value) => value.status !== 'queued' && value.status !== 'running', {
        intervalMs: 1500,
        timeoutMs: 15 * 60 * 1000,
        signal: controller.signal,
      })
    }
    if (run.status !== 'success' || !run.result) throw new Error(run.error || '扫描未生成可用结果')
    result.value = run.result
    currentResultId.value = run.id
    selectedSymbols.value = []
    watchlistBatch.value = null
    includeST.value = Boolean((run.request as ScanRequest | undefined)?.include_st)
    includeStale.value = Boolean((run.request as ScanRequest | undefined)?.include_stale)
    lastScanTarget = scanTargetFromRequest(run.request)
    void router.replace({ query: { ...route.query, result_id: String(run.id) } })
  } finally {
    if (scanPollAbort === controller) scanPollAbort = null
  }
}

function runRetailTemplate(template: RetailTemplate) {
  const params = { ...(retailParamValues.value[template.key] ?? {}) }
  return runScan(
    { template_key: template.key, template_version: template.version, template_params: params },
    `retail-${template.key}`,
  )
}

function openScanHistory(item: StrategyRun<ScanResult>) {
  if (item.status === 'success') {
    void openScanResult(item.id, false).catch((error) => message.error((error as Error).message))
    return
  }
  void router.push({ name: 'tasks', query: { job_id: String(item.job_run_id) } })
}

async function runScan(target: ScanTarget, tag: string) {
  scanning.value = tag
  try {
    const run = await screenerScan({
      ...target,
      include_st: includeST.value,
      include_stale: includeStale.value,
    })
    void router.replace({ query: { ...route.query, result_id: String(run.id) } })
    message.info('扫描任务已创建，可在任务中心查看或取消')
    await openScanResult(run.id, true)
    lastScanTarget = target
    status.value = await getScreenerStatus().catch(() => status.value)
    scanHistory.value = await listScreenerResults(20).catch(() => scanHistory.value)
    if (!result.value?.matched) {
      message.info('本次扫描无命中（条件较严或市况不配合，属正常情况）')
    }
  } catch (e) {
    if (!isPollCancelled(e)) message.error(`${(e as Error).message}；最近一次成功结果仍保留在页面和扫描历史中`)
  } finally {
    scanning.value = ''
  }
}
function rescan() {
  if (lastScanTarget) runScan(lastScanTarget, 'rescan')
}

const resultStats = computed(() => {
  const r = result.value
  if (!r) return ''
  const parts = [`命中 ${r.matched}`, `参与判定 ${r.scanned}/${r.universe} 只`]
  if (r.st_skipped) parts.push(`ST 跳过 ${r.st_skipped}`)
  if (r.stale_skipped) parts.push(`停牌/滞后跳过 ${r.stale_skipped}`)
  if (r.truncated) parts.push(`仅展示前 ${r.items?.length ?? 0} 只（按成交额降序）`)
  return parts.join(' · ')
})

function shortHash(hash?: string): string {
  return hash ? hash.slice(0, 8) : '--------'
}

// ---------- 扫描结果批量加入自选 ----------

const currentResultId = ref(0)
const selectedSymbols = ref<string[]>([])
const batchShow = ref(false)
const batchLoading = ref(false)
const watchlistGroups = ref<WatchlistGroup[]>([])
const batchGroupId = ref<number | null>(null)
const watchlistBatch = ref<WatchlistBatch | null>(null)
const watchlistBatchIssues = computed(() =>
  (watchlistBatch.value?.items ?? []).filter((item) => item.status === 'failed' || item.status === 'conflict'),
)
const displayedSymbols = computed(() => (result.value?.items ?? []).map((item) => item.symbol))
const allDisplayedSelected = computed(
  () => displayedSymbols.value.length > 0 && displayedSymbols.value.every((symbol) => selectedSymbols.value.includes(symbol)),
)
const someDisplayedSelected = computed(
  () => !allDisplayedSelected.value && displayedSymbols.value.some((symbol) => selectedSymbols.value.includes(symbol)),
)

function toggleResultSymbol(symbol: string, checked: boolean) {
  if (checked) {
    if (selectedSymbols.value.includes(symbol)) return
    if (selectedSymbols.value.length >= 100) {
      message.warning('单次最多选择 100 只股票')
      return
    }
    selectedSymbols.value = [...selectedSymbols.value, symbol]
    return
  }
  selectedSymbols.value = selectedSymbols.value.filter((item) => item !== symbol)
}

function toggleAllResults(checked: boolean) {
  if (!checked) {
    selectedSymbols.value = []
    return
  }
  if (displayedSymbols.value.length > 100) {
    message.warning('当前结果超过 100 只，请先缩小范围后批量加入')
    return
  }
  selectedSymbols.value = [...displayedSymbols.value]
}

async function openWatchlistBatch() {
  if (!currentResultId.value || !selectedSymbols.value.length) {
    message.warning('请先选择要加入自选的股票')
    return
  }
  batchLoading.value = true
  try {
    watchlistGroups.value = await listWatchlists()
    batchGroupId.value = watchlistGroups.value[0]?.id ?? null
    watchlistBatch.value = null
    batchShow.value = true
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    batchLoading.value = false
  }
}

async function applyWatchlistBatch() {
  if (!batchGroupId.value) {
    message.warning('请选择自选分组')
    return
  }
  batchLoading.value = true
  try {
    watchlistBatch.value = await createWatchlistBatch(currentResultId.value, batchGroupId.value, selectedSymbols.value)
    const batch = watchlistBatch.value
    message.success(`批量处理完成：新增 ${batch.created}，已存在 ${batch.existed}，失败 ${batch.failed}`)
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    batchLoading.value = false
  }
}

async function undoCurrentWatchlistBatch() {
  if (!watchlistBatch.value) return
  batchLoading.value = true
  try {
    watchlistBatch.value = await undoWatchlistBatch(watchlistBatch.value.id)
    if (watchlistBatch.value.conflicts) {
      message.warning(`已撤销 ${watchlistBatch.value.removed} 项，${watchlistBatch.value.conflicts} 项因后续变更而保留`)
    } else {
      message.success(`已撤销本批新建的 ${watchlistBatch.value.removed} 项`)
    }
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    batchLoading.value = false
  }
}

// ---------- 自定义策略编辑器 ----------

// 行式条件（行间 AND）：数值因子支持 值比较/区间/与因子比，布尔因子支持 是/否。
interface CondRow {
  factor: string
  op: string // '>' '>=' '<' '<=' 'between' 'is_true' 'is_false' '>ref' '<ref'
  value?: number
  value2?: number
  ref?: string
}
const editorShow = ref(false)
const editorSaving = ref(false)
const editorForm = ref<{ id: number; name: string; desc: string; period: string; risk: string }>({
  id: 0,
  name: '',
  desc: '',
  period: 'swing',
  risk: 'mid',
})
const editorRows = ref<CondRow[]>([])
const editorBaseRevisionId = ref<number>()
const editorLoadedRevision = ref<number>()
const editorCurrentRevision = ref<number>()
const editorRestored = ref(false)
interface RevisionConflict {
  staleRevisionId: number
  currentRevisionId: number
  currentRevision: number
}
const editorConflict = ref<RevisionConflict | null>(null)
const conflictCompared = ref(false)

const factorOptions = computed(() => {
  const groups = new Map<string, { label: string; value: string }[]>()
  for (const f of factors.value) {
    if (!groups.has(f.group)) groups.set(f.group, [])
    groups.get(f.group)!.push({ label: `${f.name}（${f.key}）`, value: f.key })
  }
  return Array.from(groups.entries()).map(([g, children]) => ({
    type: 'group' as const,
    label: g,
    key: g,
    children,
  }))
})
// 数值因子（ref 右侧可选：排除布尔）。
const refFactorOptions = computed(() =>
  factors.value
    .filter((f) => f.kind !== 'bool')
    .map((f) => ({ label: f.name, value: f.key })),
)
function factorDef(key: string): FactorDef | undefined {
  return factors.value.find((f) => f.key === key)
}
function opOptions(factorKey: string) {
  const def = factorDef(factorKey)
  if (def?.kind === 'bool') {
    return [
      { label: '为是', value: 'is_true' },
      { label: '为否', value: 'is_false' },
    ]
  }
  return [
    { label: '大于', value: '>' },
    { label: '大于等于', value: '>=' },
    { label: '小于', value: '<' },
    { label: '小于等于', value: '<=' },
    { label: '介于', value: 'between' },
    { label: '大于某因子', value: '>ref' },
    { label: '小于某因子', value: '<ref' },
  ]
}
function onRowFactorChange(row: CondRow) {
  const def = factorDef(row.factor)
  if (def?.kind === 'bool') {
    row.op = 'is_true'
    row.value = undefined
    row.value2 = undefined
    row.ref = undefined
  } else if (row.op === 'is_true' || row.op === 'is_false') {
    row.op = '>'
  }
}
function addRow() {
  editorRows.value.push({ factor: 'chg_pct', op: '>', value: 0 })
}
function removeRow(i: number) {
  editorRows.value.splice(i, 1)
}

function rowsToTree(rows: CondRow[]): CondNode | null {
  const leaves: CondNode[] = []
  for (const r of rows) {
    if (!r.factor) continue
    switch (r.op) {
      case 'is_true':
      case 'is_false':
        leaves.push({ factor: r.factor, op: r.op })
        break
      case 'between':
        if (r.value == null || r.value2 == null) return null
        leaves.push({ factor: r.factor, op: 'between', value: r.value, value2: r.value2 })
        break
      case '>ref':
      case '<ref':
        if (!r.ref) return null
        leaves.push({ factor: r.factor, op: r.op[0], ref: r.ref })
        break
      default:
        if (r.value == null) return null
        leaves.push({ factor: r.factor, op: r.op, value: r.value })
    }
  }
  if (!leaves.length) return null
  return { all: leaves }
}

// flattenTree 将「单层 all 叶子」树回填为编辑行；含嵌套 any/组的高级树返回 null（不可行式编辑）。
function flattenTree(tree: CondNode | null): CondRow[] | null {
  if (!tree || !tree.all?.length || tree.any?.length) return null
  const rows: CondRow[] = []
  for (const n of tree.all) {
    if (n.all?.length || n.any?.length || !n.factor || !n.op) return null
    if (n.ref) {
      if (n.op !== '>' && n.op !== '<') return null
      rows.push({ factor: n.factor, op: `${n.op}ref`, ref: n.ref })
    } else {
      rows.push({ factor: n.factor, op: n.op, value: n.value, value2: n.value2 })
    }
  }
  return rows
}

function openCreate() {
  editorForm.value = { id: 0, name: '', desc: '', period: 'swing', risk: 'mid' }
  editorRows.value = [{ factor: 'chg_pct', op: 'between', value: 1, value2: 6 }]
  editorBaseRevisionId.value = undefined
  editorLoadedRevision.value = undefined
  editorCurrentRevision.value = undefined
  editorRestored.value = false
  editorConflict.value = null
  conflictCompared.value = false
  resetAiGen()
  editorShow.value = true
}
function openEdit(cs: CustomStrategy) {
  const rows = flattenTree(cs.tree)
  resetAiGen()
  editorForm.value = { id: cs.id, name: cs.name, desc: cs.desc, period: cs.period || 'swing', risk: cs.risk || 'mid' }
  editorBaseRevisionId.value = cs.current_revision_id
  editorLoadedRevision.value = cs.revision
  editorCurrentRevision.value = cs.revision
  editorRestored.value = false
  editorConflict.value = null
  conflictCompared.value = false
  if (rows) {
    editorRows.value = rows
  } else {
    editorRows.value = []
    aiAdvancedTree.value = cs.tree
    aiAdvancedConditions.value = cs.conditions ?? []
    message.info('该版本含嵌套条件组，条件树将以只读方式保留，可修改基本信息并保存新版本')
  }
  editorShow.value = true
}

// ---------- 不可变版本历史与比较 ----------

const historyShow = ref(false)
const historyLoading = ref(false)
const historyError = ref('')
const historyName = ref('')
const strategyHistory = ref<ScreenerStrategyHistory | null>(null)
const historyLeftId = ref<number | null>(null)
const historyRightId = ref<number | null>(null)
let historyRequestSequence = 0

const historyOptions = computed(() =>
  (strategyHistory.value?.revisions ?? []).map((revision) => ({
    label: `v${revision.revision} · ${shortHash(revision.content_hash)} · ${formatRevisionTime(revision.created_at)}`,
    value: revision.id,
  })),
)
const historyLeft = computed(() =>
  strategyHistory.value?.revisions.find((revision) => revision.id === historyLeftId.value),
)
const historyRight = computed(() =>
  strategyHistory.value?.revisions.find((revision) => revision.id === historyRightId.value),
)

function formatRevisionTime(value: string): string {
  if (!value) return '时间未知'
  return value.replace('T', ' ').slice(0, 16)
}

async function openHistory(
  strategy: CustomStrategy | number,
  preferredLeftId?: number,
  preferredRightId?: number,
): Promise<boolean> {
  const requestSequence = ++historyRequestSequence
  const strategyId = typeof strategy === 'number' ? strategy : strategy.id
  historyName.value = typeof strategy === 'number' ? editorForm.value.name : strategy.name
  historyShow.value = true
  historyLoading.value = true
  historyError.value = ''
  strategyHistory.value = null
  try {
    const view = await getScreenerStrategyHistory(strategyId)
    if (requestSequence !== historyRequestSequence) return false
    strategyHistory.value = view
    const revisions = view.revisions
    const currentId = view.current_revision_id || revisions[0]?.id || null
    historyLeftId.value = revisions.some((revision) => revision.id === preferredLeftId) ? preferredLeftId! : currentId
    const fallbackRight = revisions.find((revision) => revision.id !== historyLeftId.value)?.id ?? currentId
    historyRightId.value = revisions.some((revision) => revision.id === preferredRightId) ? preferredRightId! : fallbackRight
    return true
  } catch (e) {
    if (requestSequence !== historyRequestSequence) return false
    historyError.value = (e as Error).message
    return false
  } finally {
    if (requestSequence === historyRequestSequence) historyLoading.value = false
  }
}

function restoreRevision(revision: ScreenerStrategyRevision) {
  const history = strategyHistory.value
  if (!history || !revision.tree) {
    message.error('该版本没有可恢复的条件树快照')
    return
  }
  const current = history.revisions.find((item) => item.id === history.current_revision_id) ?? history.revisions[0]
  const rows = flattenTree(revision.tree)
  resetAiGen()
  editorForm.value = {
    id: revision.strategy_id,
    name: revision.name,
    desc: revision.desc,
    period: revision.period || 'swing',
    risk: revision.risk || 'mid',
  }
  editorBaseRevisionId.value = history.current_revision_id
  editorLoadedRevision.value = revision.revision
  editorCurrentRevision.value = current?.revision ?? revision.revision
  editorRestored.value = revision.id !== history.current_revision_id
  editorConflict.value = null
  conflictCompared.value = false
  if (rows) {
    editorRows.value = rows
  } else {
    editorRows.value = []
    aiAdvancedTree.value = revision.tree
    aiAdvancedConditions.value = revision.conditions ?? []
  }
  historyShow.value = false
  editorShow.value = true
}

async function scanHistoryRevision(revision: ScreenerStrategyRevision) {
  historyShow.value = false
  await runScan(
    { strategy_id: revision.strategy_id, strategy_revision_id: revision.id },
    `custom-${revision.strategy_id}-revision-${revision.id}`,
  )
}

function backtestHistoryRevision(revision: ScreenerStrategyRevision) {
  router.push({
    path: '/backtest',
    query: { strategy_id: String(revision.strategy_id), strategy_revision_id: String(revision.id) },
  })
}

function stableTreeJSON(tree: CondNode | null): string {
  function normalize(value: unknown): unknown {
    if (Array.isArray(value)) return value.map(normalize)
    if (value && typeof value === 'object') {
      return Object.fromEntries(
        Object.entries(value as Record<string, unknown>)
          .filter(([, item]) => item !== undefined)
          .sort(([left], [right]) => left.localeCompare(right))
          .map(([key, item]) => [key, normalize(item)]),
      )
    }
    return value
  }
  return JSON.stringify(normalize(tree), null, 2)
}

interface TreeDiffLine {
  text: string
  changed: boolean
}

function lineDiff(leftText: string, rightText: string): { left: TreeDiffLine[]; right: TreeDiffLine[] } {
  const left = leftText.split('\n')
  const right = rightText.split('\n')
  const lengths = Array.from({ length: left.length + 1 }, () => Array<number>(right.length + 1).fill(0))
  for (let i = left.length - 1; i >= 0; i--) {
    for (let j = right.length - 1; j >= 0; j--) {
      lengths[i][j] = left[i] === right[j] ? lengths[i + 1][j + 1] + 1 : Math.max(lengths[i + 1][j], lengths[i][j + 1])
    }
  }
  const leftSame = new Set<number>()
  const rightSame = new Set<number>()
  let i = 0
  let j = 0
  while (i < left.length && j < right.length) {
    if (left[i] === right[j]) {
      leftSame.add(i)
      rightSame.add(j)
      i++
      j++
    } else if (lengths[i + 1][j] >= lengths[i][j + 1]) {
      i++
    } else {
      j++
    }
  }
  return {
    left: left.map((text, index) => ({ text, changed: !leftSame.has(index) })),
    right: right.map((text, index) => ({ text, changed: !rightSame.has(index) })),
  }
}

const historyTreeDiff = computed(() =>
  lineDiff(stableTreeJSON(historyLeft.value?.tree ?? null), stableTreeJSON(historyRight.value?.tree ?? null)),
)
const historyTreeChanged = computed(
  () => stableTreeJSON(historyLeft.value?.tree ?? null) !== stableTreeJSON(historyRight.value?.tree ?? null),
)

function historyFieldChanged(field: 'name' | 'desc' | 'period' | 'risk'): boolean {
  return historyLeft.value?.[field] !== historyRight.value?.[field]
}

async function compareConflict() {
  const conflict = editorConflict.value
  if (!conflict) return
  conflictCompared.value = await openHistory(editorForm.value.id, conflict.currentRevisionId, conflict.staleRevisionId)
  if (conflictCompared.value && strategyHistory.value) {
    const current = strategyHistory.value.revisions.find(
      (revision) => revision.id === strategyHistory.value?.current_revision_id,
    )
    conflict.currentRevisionId = strategyHistory.value.current_revision_id
    conflict.currentRevision = current?.revision ?? conflict.currentRevision
  }
}

function acceptLatestConflictBase() {
  const conflict = editorConflict.value
  if (!conflict) return
  if (!conflictCompared.value) {
    message.warning('请先打开版本历史，比较冲突版本后再继续')
    return
  }
  editorBaseRevisionId.value = conflict.currentRevisionId
  editorCurrentRevision.value = conflict.currentRevision
  editorConflict.value = null
  message.info(`已改用 v${conflict.currentRevision} 作为保存基线；再次保存会创建一个新版本`)
}

// ---------- AI 白话生成（P3c）----------
// AI 只负责生成：预览（人话条件 + unmatched 警示）→ 用户点「套用」才落编辑器，
// 不直接执行扫描。嵌套 any 树无法行式编辑，以只读条件清单形态套用（可保存/试扫）。

const aiText = ref('')
const aiParsing = ref(false)
const aiResult = ref<ParseStrategyResult | null>(null)
const aiTask = ref<LLMTask<ParseStrategyResult> | null>(null)
const aiTaskError = ref('')
let aiPollAbort: AbortController | null = null
onBeforeUnmount(() => aiPollAbort?.abort())
// 套用的嵌套树（行式编辑器只支持一层 all 的既有约束）：非空时行编辑区切只读展示。
const aiAdvancedTree = ref<CondNode | null>(null)
const aiAdvancedConditions = ref<string[]>([])

function resetAiGen() {
  aiPollAbort?.abort()
  aiPollAbort = null
  aiText.value = ''
  aiParsing.value = false
  aiResult.value = null
  aiTask.value = null
  aiTaskError.value = ''
  aiAdvancedTree.value = null
  aiAdvancedConditions.value = []
}

async function runAiParse() {
  const text = aiText.value.trim()
  if (!text) {
    message.warning('请先用白话描述选股条件')
    return
  }
  aiParsing.value = true
  aiResult.value = null
  aiTaskError.value = ''
  try {
    const task = await parseScreenerStrategy(text)
    aiTask.value = task
    message.info('解析任务已创建，正在后台执行（刷新或关闭页面不影响任务）')
    await trackParseTask(task)
  } catch (e) {
    if (!isPollCancelled(e)) {
      aiTaskError.value = (e as Error).message
      message.error(aiTaskError.value)
    }
  } finally {
    aiParsing.value = false
  }
}

function applyParseResult(value: ParseStrategyResult) {
  aiResult.value = value
  aiTaskError.value = ''
  if (!value.tree) {
    message.warning('AI 未能映射出任何条件（因子库没有对应数据，详见提示）')
  }
}

async function trackParseTask(initial: LLMTask<ParseStrategyResult>, silentFailure = false) {
  aiPollAbort?.abort()
  const controller = new AbortController()
  aiPollAbort = controller
  aiTask.value = initial
  aiParsing.value = initial.status === 'processing'
  try {
    const task =
      initial.status === 'processing'
        ? await pollUntil(() => getLLMTask<ParseStrategyResult>(initial.id), (v) => v.status !== 'processing', {
            signal: controller.signal,
          })
        : initial
    aiTask.value = task
    if (task.status === 'failed') throw new Error(task.error || '自然语言选股解析失败')
    if (!task.result) throw new Error('解析任务已完成，但未返回结果')
    applyParseResult(task.result)
  } catch (e) {
    if (isPollCancelled(e)) return
    aiTaskError.value = (e as Error).message
    if (!silentFailure) message.error(aiTaskError.value)
  } finally {
    if (aiPollAbort === controller) {
      aiPollAbort = null
      aiParsing.value = false
    }
  }
}

async function restoreParseTask() {
  const tasks = await listLLMTasks<ParseStrategyResult>({ kind: 'screener_parse', limit: 1 }).catch(() => [])
  const summary = tasks[0]
  if (!summary) return
  const terminalIsRecent = Date.now() - new Date(summary.updated_at || summary.created_at).getTime() < 15 * 60 * 1000
  if (summary.status !== 'processing' && !terminalIsRecent) return
  const task =
    summary.status === 'processing' ? summary : await getLLMTask<ParseStrategyResult>(summary.id).catch(() => summary)
  editorShow.value = true
  if (!editorRows.value.length) editorRows.value = [{ factor: 'chg_pct', op: 'between', value: 1, value2: 6 }]
  if (task.status === 'processing') {
    void trackParseTask(task)
  } else if (task.status === 'success' && task.result) {
    aiTask.value = task
    aiResult.value = task.result
  } else if (task.status === 'failed') {
    aiTask.value = task
    aiTaskError.value = task.error || '自然语言选股解析失败'
  }
}

onMounted(() => void restoreParseTask())

function adoptAiResult() {
  const r = aiResult.value
  if (!r?.tree) return
  const rows = flattenTree(r.tree)
  if (rows) {
    editorRows.value = rows
    aiAdvancedTree.value = null
    aiAdvancedConditions.value = []
  } else {
    aiAdvancedTree.value = r.tree
    aiAdvancedConditions.value = r.conditions ?? []
    message.info('AI 生成了嵌套条件组（满足其一），已按只读方式套用，可直接保存或试扫')
  }
  if (!editorForm.value.desc && r.explain) {
    editorForm.value.desc = r.explain.slice(0, 120)
  }
  message.success('已套用到编辑器，请确认后保存')
}

function clearAdvancedTree() {
  aiAdvancedTree.value = null
  aiAdvancedConditions.value = []
  if (!editorRows.value.length) addRow()
}

// 编辑器当前生效的条件树：AI 嵌套树优先，否则由编辑行构造。
function editorTree(): CondNode | null {
  return aiAdvancedTree.value ?? rowsToTree(editorRows.value)
}

function isRevisionConflict(error: unknown): boolean {
  if (error instanceof ApiRequestError && (error.status === 409 || error.code === 'strategy_revision_conflict')) return true
  return /刷新并比较|版本冲突|其他页面更新/.test((error as Error)?.message ?? '')
}

async function saveEditor() {
  const tree = editorTree()
  if (!editorForm.value.name.trim()) {
    message.warning('请填写策略名称')
    return
  }
  if (!tree) {
    message.warning('请补全条件（每行需选因子并填值）')
    return
  }
  editorSaving.value = true
  const savedBaseRevisionId = editorBaseRevisionId.value
  try {
    const saved = await saveScreenerStrategy({
      id: editorForm.value.id || undefined,
      base_revision_id: editorForm.value.id ? savedBaseRevisionId : undefined,
      name: editorForm.value.name,
      desc: editorForm.value.desc,
      period: editorForm.value.period,
      risk: editorForm.value.risk,
      tree,
    })
    if (editorForm.value.id && saved.current_revision_id === savedBaseRevisionId) {
      message.info(`内容未变化，继续使用 v${saved.revision}`)
    } else {
      message.success(`策略已保存为 v${saved.revision}`)
    }
    editorShow.value = false
    await load()
  } catch (e) {
    if (editorForm.value.id && isRevisionConflict(e)) {
      editorConflict.value = {
        staleRevisionId: savedBaseRevisionId ?? 0,
        currentRevisionId: 0,
        currentRevision: 0,
      }
      conflictCompared.value = false
      try {
        data.value = await getScreenerStrategies()
        const latest = customList.value.find((strategy) => strategy.id === editorForm.value.id)
        if (latest) {
          editorConflict.value = {
            staleRevisionId: savedBaseRevisionId ?? 0,
            currentRevisionId: latest.current_revision_id,
            currentRevision: latest.revision,
          }
        }
      } catch {
        // 保留编辑内容和原始冲突提示，历史比较按钮仍可再次主动加载。
      }
      message.error('策略已被其他页面更新。当前编辑内容已保留，请刷新并比较版本后再决定是否保存')
    } else {
      message.error((e as Error).message)
    }
  } finally {
    editorSaving.value = false
  }
}

async function tryScanEditor() {
  const tree = editorTree()
  if (!tree) {
    message.warning('请补全条件后再试扫')
    return
  }
  editorShow.value = false
  await runScan({ tree }, 'temp')
}

async function removeCustom(id: number) {
  try {
    await deleteScreenerStrategy(id)
    message.success('策略已归档，历史版本和既有研究不受影响')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
</script>

<template>
  <PageContainer
    title="策略选股"
    subtitle="基于全市场日线因子宽表的条件选股：内置白话策略一键扫描，命中原因逐条可解释"
  >
    <div class="screener" :style="styleVars">
      <!-- 宽表状态条 -->
      <div class="status-line qv-anim-in">
        <n-spin v-if="status?.building || scanning" :size="14" />
        <span class="status-text">{{ statusText }}</span>
      </div>

      <SectionCard title="按目标选股" class="block">
        <n-spin :show="loading">
          <div class="retail-grid">
            <section v-for="template in retailTemplates" :key="template.key" class="retail-template">
              <div class="sc-head">
                <span class="sc-name">{{ template.name }}</span>
                <span class="sc-tags">
                  <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[template.period] || template.period }}</n-tag>
                  <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[template.risk_level] || 'default'">
                    {{ RISK_LABEL[template.risk_level] || template.risk_level }}
                  </n-tag>
                </span>
              </div>
              <dl class="retail-notes">
                <div><dt>适用</dt><dd>{{ template.scenario }}</dd></div>
                <div><dt>风险</dt><dd>{{ template.risk }}</dd></div>
                <div><dt>数据</dt><dd>{{ template.data_requirements }}</dd></div>
              </dl>
              <div class="retail-params">
                <label v-for="param in template.params" :key="param.key">
                  <span>{{ param.label }}</span>
                  <n-input-number
                    v-model:value="retailParamValues[template.key][param.key]"
                    :min="param.min"
                    :max="param.max"
                    :step="param.step"
                    size="small"
                  >
                    <template v-if="param.unit" #suffix>{{ param.unit }}</template>
                  </n-input-number>
                </label>
              </div>
              <div class="sc-conds">
                <n-tag v-for="condition in template.conditions" :key="condition" size="small" :bordered="false" class="cond-tag">
                  {{ condition }}
                </n-tag>
              </div>
              <div class="retail-action">
                <n-button
                  size="small"
                  type="primary"
                  secondary
                  :loading="scanning === `retail-${template.key}`"
                  :disabled="!!scanning && scanning !== `retail-${template.key}`"
                  @click="runRetailTemplate(template)"
                >
                  开始扫描
                </n-button>
              </div>
            </section>
          </div>
        </n-spin>
      </SectionCard>

      <!-- 扫描结果 -->
      <SectionCard v-if="result" :title="`扫描结果 · ${result.strategy}`" class="block">
        <template #extra>
          <div class="result-switches">
            <label class="switch-item">
              <n-switch v-model:value="includeST" size="small" @update:value="rescan" />
              <span>含ST</span>
            </label>
            <label class="switch-item">
              <n-switch v-model:value="includeStale" size="small" @update:value="rescan" />
              <span>含停牌</span>
            </label>
          </div>
        </template>
        <p class="result-stats">
          {{ resultStats }}
          <span class="muted">（数据为 {{ result.trade_date }} 收盘口径）</span>
        </p>
        <p v-if="result.strategy_revision_id" class="revision-meta">
          固定快照
          <n-tag size="tiny" type="info" :bordered="false">v{{ result.strategy_revision }}</n-tag>
          <code>{{ shortHash(result.strategy_hash) }}</code>
        </p>
        <p v-if="result.conditions?.length" class="result-conds">
          条件：<n-tag v-for="c in result.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
        </p>
        <div v-if="result.items?.length" class="batch-toolbar">
          <div class="batch-selection">
            <n-checkbox
              :checked="allDisplayedSelected"
              :indeterminate="someDisplayedSelected"
              @update:checked="toggleAllResults"
            >
              全选当前结果
            </n-checkbox>
            <span>已选 {{ selectedSymbols.length }} / 100</span>
          </div>
          <div class="batch-actions">
            <n-button v-if="selectedSymbols.length" size="small" quaternary @click="selectedSymbols = []">清空</n-button>
            <n-button
              size="small"
              type="primary"
              :disabled="!selectedSymbols.length"
              :loading="batchLoading"
              @click="openWatchlistBatch"
            >
              批量加入自选
            </n-button>
          </div>
        </div>
        <n-empty v-if="!result.items?.length" description="无命中标的" class="empty-pad" />
        <div v-else class="qv-scroll-x">
          <n-table size="small" :single-line="false" class="hits-table">
            <thead>
              <tr>
                <th class="select-col">选择</th>
                <th colspan="2">股票</th>
                <th class="num">现价</th>
                <th class="num">涨跌</th>
                <th class="num">成交额(亿)</th>
                <th v-if="!isMobile" class="num">换手%</th>
                <th v-if="!isMobile" class="num">60日位置</th>
                <th>命中原因</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="h in result.items" :key="h.symbol">
                <td class="select-col">
                  <n-checkbox
                    :checked="selectedSymbols.includes(h.symbol)"
                    :aria-label="`选择 ${h.name || h.symbol}`"
                    @update:checked="(checked) => toggleResultSymbol(h.symbol, checked)"
                  />
                </td>
                <td colspan="2"><StockIdentity :symbol="h.symbol" market="cn" :name="h.name" density="table" clickable actions /></td>
                <td class="num qv-tnum">{{ h.price.toFixed(2) }}</td>
                <td class="num"><ChangeTag :value="h.chg_pct" size="small" /></td>
                <td class="num qv-tnum">{{ h.amount_yi.toFixed(2) }}</td>
                <td v-if="!isMobile" class="num qv-tnum">{{ h.turnover_rate ? h.turnover_rate.toFixed(2) : '—' }}</td>
                <td v-if="!isMobile" class="num qv-tnum">{{ h.pos_60 ? h.pos_60.toFixed(0) + '%' : '—' }}</td>
                <td class="reasons-cell">
                  <n-popover trigger="hover" placement="top" :disabled="h.reasons.length <= 2">
                    <template #trigger>
                      <span class="reasons-brief">
                        <n-tag v-for="r in h.reasons.slice(0, 2)" :key="r" size="small" :bordered="false" class="cond-tag">{{ r }}</n-tag>
                        <n-tag v-if="h.reasons.length > 2" size="small" :bordered="false" class="cond-tag more-tag"
                          >+{{ h.reasons.length - 2 }}</n-tag
                        >
                      </span>
                    </template>
                    <div class="reasons-full">
                      <div v-for="r in h.reasons" :key="r">{{ r }}</div>
                    </div>
                  </n-popover>
                </td>
                <td>
                  <div class="row-actions">
                    <n-button size="tiny" quaternary @click="actions.goDetail({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >详情</n-button
                    >
                    <n-button size="tiny" quaternary @click="actions.goAnalysis({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >AI分析</n-button
                    >
                    <n-button
                      size="tiny"
                      quaternary
                      :loading="actions.adding.value"
                      @click="actions.addToWatchlist({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >加自选</n-button
                    >
                  </div>
                </td>
              </tr>
            </tbody>
          </n-table>
        </div>
      </SectionCard>

      <n-modal
        v-model:show="batchShow"
        preset="card"
        title="批量加入自选"
        :mask-closable="!batchLoading"
        :style="[styleVars, { width: 'min(620px, calc(100vw - 24px))' }]"
      >
        <template v-if="!watchlistBatch">
          <n-form label-placement="top">
            <n-form-item label="目标分组" required>
              <n-select
                v-model:value="batchGroupId"
                :options="watchlistGroups.map((group) => ({ label: group.name, value: group.id }))"
                placeholder="选择自选分组"
              />
            </n-form-item>
          </n-form>
          <p class="batch-modal-summary">本次处理 {{ selectedSymbols.length }} 只股票</p>
          <div class="batch-modal-actions">
            <n-button :disabled="batchLoading" @click="batchShow = false">取消</n-button>
            <n-button type="primary" :loading="batchLoading" :disabled="!batchGroupId" @click="applyWatchlistBatch">
              确认加入
            </n-button>
          </div>
        </template>
        <template v-else>
          <n-alert
            :type="watchlistBatch.status === 'undo_conflict' || watchlistBatch.failed ? 'warning' : watchlistBatch.status === 'undone' ? 'info' : 'success'"
            :bordered="false"
          >
            新增 {{ watchlistBatch.created }} · 已存在 {{ watchlistBatch.existed }} · 失败 {{ watchlistBatch.failed }}
            <template v-if="watchlistBatch.status !== 'applied'">
              · 已撤销 {{ watchlistBatch.removed }} · 冲突 {{ watchlistBatch.conflicts }}
            </template>
          </n-alert>
          <div v-if="watchlistBatchIssues.length" class="batch-issues qv-scroll-x">
            <n-table size="small" :single-line="false">
              <thead><tr><th>代码</th><th>状态</th><th>说明</th></tr></thead>
              <tbody>
                <tr v-for="item in watchlistBatchIssues" :key="item.id">
                  <td class="qv-tnum">{{ item.symbol }}</td>
                  <td>{{ item.status === 'conflict' ? '撤销冲突' : '加入失败' }}</td>
                  <td>{{ item.message || '未处理' }}</td>
                </tr>
              </tbody>
            </n-table>
          </div>
          <div class="batch-modal-actions">
            <n-popconfirm
              v-if="watchlistBatch.status === 'applied' && watchlistBatch.created > 0"
              @positive-click="undoCurrentWatchlistBatch"
            >
              <template #trigger><n-button :loading="batchLoading">撤销本批新增</n-button></template>
              只移除本批新建且之后未被修改的自选项；原有项不会删除。
            </n-popconfirm>
            <n-button type="primary" @click="batchShow = false">完成</n-button>
          </div>
        </template>
      </n-modal>

      <SectionCard title="扫描历史" class="block">
        <n-empty v-if="!scanHistory.length" description="暂无持久扫描结果" />
        <div v-else class="qv-scroll-x">
          <n-table size="small" :single-line="false">
            <thead><tr><th>策略</th><th>版本</th><th>状态</th><th>数据时点</th><th>完成时间</th><th>操作</th></tr></thead>
            <tbody>
              <tr v-for="item in scanHistory" :key="item.id">
                <td>{{ item.strategy_name }}</td>
                <td><span v-if="item.strategy_revision">v{{ item.strategy_revision }} · </span><code>{{ shortHash(item.strategy_hash) }}</code></td>
                <td><n-tag size="small" :type="item.status === 'success' ? 'success' : item.status === 'failed' ? 'error' : 'info'">{{ taskStatusLabel(item.status) }}</n-tag></td>
                <td>{{ item.as_of?.slice(0, 10) || '—' }}</td>
                <td>{{ (item.finished_at || item.created_at).replace('T', ' ').slice(0, 16) }}</td>
                <td><n-button size="tiny" quaternary @click="openScanHistory(item)">{{ item.status === 'success' ? '查看' : '任务详情' }}</n-button></td>
              </tr>
            </tbody>
          </n-table>
        </div>
      </SectionCard>

      <!-- 策略广场 -->
      <SectionCard title="进阶内置策略" class="block">
        <template #extra>
          <n-radio-group v-model:value="periodFilter" size="small">
            <n-radio-button value="all">全部</n-radio-button>
            <n-radio-button value="short">短线</n-radio-button>
            <n-radio-button value="swing">波段</n-radio-button>
            <n-radio-button value="mid">中线</n-radio-button>
          </n-radio-group>
        </template>
        <n-spin :show="loading">
          <n-grid cols="1 s:2 l:3" responsive="screen" :x-gap="12" :y-gap="12">
            <n-gi v-for="b in builtinFiltered" :key="b.key">
              <div class="strategy-card">
                <div class="sc-head">
                  <span class="sc-name">{{ b.name }}</span>
                  <span class="sc-tags">
                    <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[b.period] || b.period }}</n-tag>
                    <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[b.risk] || 'default'">{{
                      RISK_LABEL[b.risk] || b.risk
                    }}</n-tag>
                  </span>
                </div>
                <p class="sc-desc">{{ b.desc }}</p>
                <div class="sc-conds">
                  <n-tag v-for="c in b.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
                </div>
                <div class="sc-foot">
                  <n-button
                    size="small"
                    type="primary"
                    secondary
                    :loading="scanning === b.key"
                    :disabled="!!scanning && scanning !== b.key"
                    @click="runScan({ strategy_key: b.key }, b.key)"
                    >一键扫描</n-button
                  >
                  <n-button size="small" quaternary @click="router.push(`/backtest?strategy_key=${b.key}`)">回测</n-button>
                </div>
              </div>
            </n-gi>
          </n-grid>
        </n-spin>
      </SectionCard>

      <!-- 我的策略 -->
      <SectionCard title="我的自定义策略" class="block">
        <template #extra>
          <n-button size="small" @click="openCreate">新建策略</n-button>
        </template>
        <n-empty v-if="!customList.length" description="还没有自定义策略：点右上角「新建策略」，用因子条件组合自己的选股逻辑" class="empty-pad" />
        <div v-else class="custom-list">
          <div v-for="cs in customList" :key="cs.id" class="custom-row">
            <div class="cr-main">
              <div class="cr-head">
                <span class="sc-name">{{ cs.name }}</span>
                <n-tag size="tiny" type="info" :bordered="false">v{{ cs.revision }}</n-tag>
                <code class="revision-hash">{{ shortHash(cs.content_hash) }}</code>
                <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[cs.period] || cs.period }}</n-tag>
                <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[cs.risk] || 'default'">{{
                  RISK_LABEL[cs.risk] || cs.risk
                }}</n-tag>
              </div>
              <p v-if="cs.desc" class="sc-desc">{{ cs.desc }}</p>
              <div class="sc-conds">
                <n-tag v-for="c in cs.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
              </div>
            </div>
            <div class="cr-actions">
              <n-button
                size="small"
                type="primary"
                secondary
                :loading="scanning === `custom-${cs.id}`"
                :disabled="!!scanning && scanning !== `custom-${cs.id}`"
                @click="runScan({ strategy_id: cs.id, strategy_revision_id: cs.current_revision_id }, `custom-${cs.id}`)"
                >扫描</n-button
              >
              <n-button
                size="small"
                quaternary
                @click="router.push({ path: '/backtest', query: { strategy_id: String(cs.id), strategy_revision_id: String(cs.current_revision_id) } })"
                >回测</n-button
              >
              <n-button size="small" quaternary @click="openHistory(cs)">版本</n-button>
              <n-button size="small" quaternary @click="openEdit(cs)">编辑</n-button>
              <n-popconfirm @positive-click="removeCustom(cs.id)">
                <template #trigger>
                  <n-button size="small" quaternary type="error">归档</n-button>
                </template>
                归档策略「{{ cs.name }}」？归档后默认列表不再显示，历史版本和既有研究不受影响。
              </n-popconfirm>
            </div>
          </div>
        </div>
      </SectionCard>

      <!-- revision 历史：两版并排比较，恢复仅加载快照，不移动当前指针。 -->
      <n-modal
        v-model:show="historyShow"
        preset="card"
        :title="`版本历史 · ${historyName}`"
        class="history-modal"
        :style="[styleVars, { width: 'min(980px, calc(100vw - 24px))', maxHeight: 'calc(100vh - 32px)' }]"
      >
        <n-spin :show="historyLoading">
          <n-alert v-if="historyError" type="error" :bordered="false">{{ historyError }}</n-alert>
          <n-empty v-else-if="!historyLoading && !strategyHistory?.revisions.length" description="暂无版本历史" />
          <template v-else-if="strategyHistory">
            <p class="history-hint">
              最近 {{ strategyHistory.revisions.length }} 个不可变版本。载入旧快照只会打开编辑器；确认保存后才创建新版本，当前指针和历史行不会被改写。
            </p>
            <div class="history-selects">
              <div class="history-select">
                <span>版本 A</span>
                <n-select v-model:value="historyLeftId" :options="historyOptions" size="small" />
              </div>
              <div class="history-select">
                <span>版本 B</span>
                <n-select v-model:value="historyRightId" :options="historyOptions" size="small" />
              </div>
            </div>
            <div v-if="historyLeft && historyRight" class="revision-compare">
              <section class="revision-pane">
                <div class="revision-pane-head">
                  <div>
                    <strong>v{{ historyLeft.revision }}</strong>
                    <n-tag
                      v-if="historyLeft.id === strategyHistory.current_revision_id"
                      size="tiny"
                      type="success"
                      :bordered="false"
                      >当前</n-tag
                    >
                    <code>{{ shortHash(historyLeft.content_hash) }}</code>
                  </div>
                  <div class="revision-actions">
                    <n-button size="tiny" quaternary @click="scanHistoryRevision(historyLeft)">扫描</n-button>
                    <n-button size="tiny" quaternary @click="backtestHistoryRevision(historyLeft)">回测</n-button>
                    <n-button size="tiny" type="primary" secondary @click="restoreRevision(historyLeft)">载入编辑器</n-button>
                  </div>
                </div>
                <div class="revision-time">{{ formatRevisionTime(historyLeft.created_at) }}</div>
                <dl class="revision-fields">
                  <div :class="{ changed: historyFieldChanged('name') }"><dt>名称</dt><dd>{{ historyLeft.name }}</dd></div>
                  <div :class="{ changed: historyFieldChanged('desc') }"><dt>说明</dt><dd>{{ historyLeft.desc || '—' }}</dd></div>
                  <div :class="{ changed: historyFieldChanged('period') }">
                    <dt>周期</dt><dd>{{ PERIOD_LABEL[historyLeft.period] || historyLeft.period }}</dd>
                  </div>
                  <div :class="{ changed: historyFieldChanged('risk') }">
                    <dt>风险</dt><dd>{{ RISK_LABEL[historyLeft.risk] || historyLeft.risk }}</dd>
                  </div>
                </dl>
                <div class="tree-title">
                  条件树
                  <n-tag size="tiny" :type="historyTreeChanged ? 'warning' : 'success'" :bordered="false">
                    {{ historyTreeChanged ? '有差异' : '相同' }}
                  </n-tag>
                </div>
                <pre class="tree-code"><code><span
                  v-for="(line, index) in historyTreeDiff.left"
                  :key="index"
                  :class="{ changed: line.changed }"
                >{{ line.text }}</span></code></pre>
              </section>
              <section class="revision-pane">
                <div class="revision-pane-head">
                  <div>
                    <strong>v{{ historyRight.revision }}</strong>
                    <n-tag
                      v-if="historyRight.id === strategyHistory.current_revision_id"
                      size="tiny"
                      type="success"
                      :bordered="false"
                      >当前</n-tag
                    >
                    <code>{{ shortHash(historyRight.content_hash) }}</code>
                  </div>
                  <div class="revision-actions">
                    <n-button size="tiny" quaternary @click="scanHistoryRevision(historyRight)">扫描</n-button>
                    <n-button size="tiny" quaternary @click="backtestHistoryRevision(historyRight)">回测</n-button>
                    <n-button size="tiny" type="primary" secondary @click="restoreRevision(historyRight)">载入编辑器</n-button>
                  </div>
                </div>
                <div class="revision-time">{{ formatRevisionTime(historyRight.created_at) }}</div>
                <dl class="revision-fields">
                  <div :class="{ changed: historyFieldChanged('name') }"><dt>名称</dt><dd>{{ historyRight.name }}</dd></div>
                  <div :class="{ changed: historyFieldChanged('desc') }"><dt>说明</dt><dd>{{ historyRight.desc || '—' }}</dd></div>
                  <div :class="{ changed: historyFieldChanged('period') }">
                    <dt>周期</dt><dd>{{ PERIOD_LABEL[historyRight.period] || historyRight.period }}</dd>
                  </div>
                  <div :class="{ changed: historyFieldChanged('risk') }">
                    <dt>风险</dt><dd>{{ RISK_LABEL[historyRight.risk] || historyRight.risk }}</dd>
                  </div>
                </dl>
                <div class="tree-title">
                  条件树
                  <n-tag size="tiny" :type="historyTreeChanged ? 'warning' : 'success'" :bordered="false">
                    {{ historyTreeChanged ? '有差异' : '相同' }}
                  </n-tag>
                </div>
                <pre class="tree-code"><code><span
                  v-for="(line, index) in historyTreeDiff.right"
                  :key="index"
                  :class="{ changed: line.changed }"
                >{{ line.text }}</span></code></pre>
              </section>
            </div>
          </template>
        </n-spin>
      </n-modal>

      <!-- 自定义策略编辑器（单根约束：n-modal 必须在 PageContainer 内） -->
      <n-modal
        v-model:show="editorShow"
        preset="card"
        :title="editorForm.id ? '编辑策略' : '新建策略'"
        class="editor-modal"
        :style="[styleVars, { maxWidth: '760px' }]"
      >
        <n-alert v-if="editorConflict" type="error" :bordered="false" class="editor-revision-alert">
          <div>策略已被其他页面更新。当前编辑内容已保留；请刷新并比较后，再显式采用最新版本作为保存基线。</div>
          <div class="alert-actions">
            <n-button size="tiny" @click="compareConflict">刷新并比较</n-button>
            <n-button size="tiny" type="primary" :disabled="!conflictCompared" @click="acceptLatestConflictBase">
              采用最新基线
            </n-button>
          </div>
        </n-alert>
        <n-alert v-else-if="editorForm.id" :type="editorRestored ? 'warning' : 'info'" :bordered="false" class="editor-revision-alert">
          <template v-if="editorRestored">
            已载入 v{{ editorLoadedRevision }} 快照；当前仍为 v{{ editorCurrentRevision }}。只有确认保存后才会创建新版本。
          </template>
          <template v-else>正在编辑 v{{ editorLoadedRevision }}；保存不会覆盖该版本。</template>
        </n-alert>
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 76">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="名称" required>
                <n-input v-model:value="editorForm.name" placeholder="如：温和放量低位股" maxlength="32" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="说明">
                <n-input v-model:value="editorForm.desc" placeholder="一句话描述策略意图（可选）" maxlength="120" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="周期">
                <n-radio-group v-model:value="editorForm.period" size="small">
                  <n-radio-button value="short">短线</n-radio-button>
                  <n-radio-button value="swing">波段</n-radio-button>
                  <n-radio-button value="mid">中线</n-radio-button>
                </n-radio-group>
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="风险">
                <n-radio-group v-model:value="editorForm.risk" size="small">
                  <n-radio-button value="low">低</n-radio-button>
                  <n-radio-button value="mid">中</n-radio-button>
                  <n-radio-button value="high">高</n-radio-button>
                </n-radio-group>
              </n-form-item>
            </n-gi>
          </n-grid>
        </n-form>
        <!-- AI 白话生成（P3c）：预览确认后才落编辑器，AI 不直接执行扫描 -->
        <div class="ai-gen">
          <p class="rows-hint">
            AI 生成：用白话描述选股条件（如「缩量回踩 20 日线且获利盘低于 15%」），生成后先预览，点「套用」才会写入下方编辑器（消耗 1 次 AI 配额）。
          </p>
          <div class="ai-input-row">
            <n-input
              v-model:value="aiText"
              type="textarea"
              :rows="2"
              maxlength="300"
              show-count
              placeholder="例：量比 2 以上放量突破 20 日新高，换手率别超过 20%"
            />
            <n-button size="small" type="primary" secondary :loading="aiParsing" @click="runAiParse">AI 生成</n-button>
          </div>
          <n-alert v-if="aiTask?.status === 'processing'" type="info" :bordered="false">
            正在后台解析选股条件，页面刷新或关闭不会中断任务。
          </n-alert>
          <n-alert v-else-if="aiTaskError" type="error" :bordered="false">解析失败：{{ aiTaskError }}</n-alert>
          <div v-if="aiResult" class="ai-preview">
            <p v-if="aiResult.explain" class="ai-explain">AI 理解：{{ aiResult.explain }}</p>
            <p v-if="llmLabel(aiResult)" class="ai-explain">解析模型：{{ llmLabel(aiResult) }}</p>
            <div v-if="aiResult.conditions?.length" class="sc-conds">
              <n-tag v-for="c in aiResult.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
            </div>
            <div v-if="aiResult.unmatched?.length" class="ai-unmatched">
              <p class="ai-unmatched-hint">以下表述在因子库中没有对应数据，未纳入条件（AI 不会硬凑相近因子）：</p>
              <n-tag v-for="u in aiResult.unmatched" :key="u" size="small" type="warning" :bordered="false" class="cond-tag"
                >⚠ {{ u }}</n-tag
              >
            </div>
            <div v-if="aiResult.tree" class="ai-adopt">
              <n-button size="small" type="primary" @click="adoptAiResult">套用到编辑器</n-button>
            </div>
          </div>
        </div>
        <div v-if="aiAdvancedTree" class="editor-rows">
          <p class="rows-hint">
            AI 生成的条件含嵌套组（满足其一），不支持逐行编辑，以下为只读条件清单——可直接保存或试扫。
          </p>
          <div class="sc-conds">
            <n-tag v-for="c in aiAdvancedConditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
          </div>
          <n-button size="small" quaternary @click="clearAdvancedTree">放弃嵌套条件，改用逐行编辑</n-button>
        </div>
        <div v-else class="editor-rows">
          <p class="rows-hint">条件之间为「且」（全部满足才命中）；布尔因子选「为是/为否」，数值因子可与固定值或另一因子比较。</p>
          <div v-for="(row, i) in editorRows" :key="i" class="cond-row">
            <n-select
              v-model:value="row.factor"
              :options="factorOptions"
              filterable
              placeholder="因子"
              size="small"
              class="w-factor"
              @update:value="onRowFactorChange(row)"
            />
            <n-select v-model:value="row.op" :options="opOptions(row.factor)" size="small" class="w-op" />
            <template v-if="row.op === 'between'">
              <n-input-number v-model:value="row.value" size="small" class="w-num" placeholder="下限" />
              <span class="tilde">~</span>
              <n-input-number v-model:value="row.value2" size="small" class="w-num" placeholder="上限" />
            </template>
            <n-select
              v-else-if="row.op === '>ref' || row.op === '<ref'"
              v-model:value="row.ref"
              :options="refFactorOptions"
              filterable
              placeholder="对比因子"
              size="small"
              class="w-factor"
            />
            <n-input-number
              v-else-if="row.op !== 'is_true' && row.op !== 'is_false'"
              v-model:value="row.value"
              size="small"
              class="w-num"
              placeholder="值"
            />
            <n-button size="small" quaternary type="error" :disabled="editorRows.length <= 1" @click="removeRow(i)">删</n-button>
          </div>
          <n-button size="small" dashed block @click="addRow">+ 添加条件</n-button>
        </div>
        <template #footer>
          <div class="editor-foot">
            <n-button size="small" @click="tryScanEditor">先试扫一次</n-button>
            <n-button size="small" type="primary" :loading="editorSaving" @click="saveEditor">
              {{ editorForm.id ? '保存为新版本' : '保存策略' }}
            </n-button>
          </div>
        </template>
      </n-modal>
    </div>
  </PageContainer>
</template>

<style scoped>
.screener {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.status-line {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  opacity: 0.75;
}
.block {
  width: 100%;
}
.retail-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  column-gap: 24px;
}
.retail-template {
  min-width: 0;
  padding: 14px 0;
  border-bottom: 1px solid var(--qv-divider);
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.retail-notes {
  margin: 0;
  display: grid;
  gap: 5px;
  font-size: 12px;
  line-height: 1.55;
}
.retail-notes > div {
  display: grid;
  grid-template-columns: 34px minmax(0, 1fr);
  gap: 6px;
}
.retail-notes dt {
  opacity: 0.58;
}
.retail-notes dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.retail-params {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}
.retail-params label {
  min-width: 0;
  display: grid;
  gap: 4px;
  font-size: 12px;
}
.retail-action {
  margin-top: auto;
  display: flex;
  justify-content: flex-end;
}
.result-switches {
  display: flex;
  gap: 14px;
  align-items: center;
}
.switch-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  opacity: 0.85;
}
.result-stats {
  margin: 0 0 8px;
  font-size: 13px;
}
.result-stats .muted {
  opacity: 0.6;
}
.revision-meta {
  margin: -2px 0 10px;
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  opacity: 0.8;
}
.result-conds {
  margin: 0 0 10px;
  font-size: 12px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.batch-toolbar {
  margin: 10px 0;
  padding: 8px 0;
  border-top: 1px solid var(--qv-divider);
  border-bottom: 1px solid var(--qv-divider);
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.batch-selection,
.batch-actions,
.batch-modal-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.batch-selection > span,
.batch-modal-summary {
  font-size: 12px;
  opacity: 0.68;
}
.batch-modal-summary {
  margin: 0;
}
.batch-modal-actions {
  margin-top: 16px;
  justify-content: flex-end;
}
.batch-issues {
  margin-top: 12px;
}
.select-col {
  width: 52px;
  text-align: center;
}
.cond-tag {
  font-size: 11px;
}
.more-tag {
  opacity: 0.75;
}
.hits-table th.num,
.hits-table td.num {
  text-align: right;
}
.hits-table td,
.hits-table th {
  white-space: nowrap;
}
.reasons-cell {
  max-width: 340px;
}
.reasons-brief {
  display: inline-flex;
  gap: 4px;
  flex-wrap: wrap;
}
.reasons-full {
  max-width: 380px;
  font-size: 12px;
  line-height: 1.9;
}
.stock-link {
  cursor: pointer;
  text-decoration: none;
}
.stock-link:hover {
  text-decoration: underline;
}
.row-actions {
  display: flex;
  gap: 2px;
}
.empty-pad {
  padding: 22px 0;
}
/* 策略卡 */
.strategy-card {
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
  padding: 12px 14px;
  height: 100%;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.sc-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.sc-name {
  font-weight: 600;
  font-size: 14px;
}
.sc-tags {
  display: inline-flex;
  gap: 4px;
}
.sc-desc {
  margin: 0;
  font-size: 12px;
  line-height: 1.7;
  opacity: 0.72;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.sc-conds {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.sc-foot {
  margin-top: auto;
  display: flex;
  justify-content: flex-end;
}
/* 自定义策略行 */
.custom-list {
  display: flex;
  flex-direction: column;
}
.custom-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 2px;
  border-bottom: 1px solid var(--qv-divider);
  flex-wrap: wrap;
}
.custom-row:last-child {
  border-bottom: none;
}
.cr-main {
  flex: 1;
  min-width: 240px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.cr-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.revision-hash {
  font-size: 11px;
  opacity: 0.62;
}
.cr-actions {
  display: flex;
  gap: 4px;
  align-items: flex-start;
  flex-wrap: wrap;
  justify-content: flex-end;
}
/* 不可变版本历史 */
.history-hint {
  margin: 0 0 14px;
  font-size: 12px;
  line-height: 1.7;
  opacity: 0.7;
}
.history-modal :deep(.n-card__content) {
  overflow: auto;
}
.history-selects {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 12px;
  margin-bottom: 14px;
}
.history-select {
  display: grid;
  grid-template-columns: 58px minmax(0, 1fr);
  gap: 8px;
  align-items: center;
  font-size: 12px;
}
.revision-compare {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  border-top: 1px solid var(--qv-divider);
}
.revision-pane {
  min-width: 0;
  padding: 14px 14px 0 0;
}
.revision-pane + .revision-pane {
  padding: 14px 0 0 14px;
  border-left: 1px solid var(--qv-divider);
}
.revision-pane-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.revision-pane-head > div:first-child {
  display: flex;
  align-items: center;
  gap: 6px;
}
.revision-pane-head code {
  font-size: 11px;
  opacity: 0.65;
}
.revision-actions {
  display: flex;
  gap: 2px;
  flex-wrap: wrap;
}
.revision-time {
  margin-top: 3px;
  font-size: 11px;
  opacity: 0.55;
}
.revision-fields {
  margin: 12px 0;
  font-size: 12px;
}
.revision-fields > div {
  display: grid;
  grid-template-columns: 44px minmax(0, 1fr);
  gap: 8px;
  padding: 4px 6px;
  border-radius: 4px;
}
.revision-fields > div.changed {
  background: color-mix(in srgb, var(--qv-warning) 12%, transparent);
}
.revision-fields dt {
  opacity: 0.58;
}
.revision-fields dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.tree-title {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 6px;
  font-size: 12px;
  font-weight: 600;
}
.tree-code {
  max-height: 320px;
  margin: 0;
  padding: 8px;
  overflow: auto;
  border-radius: 4px;
  background: var(--qv-code-bg);
  font-size: 11px;
  line-height: 1.55;
}
.tree-code span {
  display: block;
  min-height: 1.55em;
  white-space: pre;
}
.tree-code span.changed {
  background: color-mix(in srgb, var(--qv-warning) 18%, transparent);
}
.editor-revision-alert {
  margin-bottom: 14px;
}
.alert-actions {
  display: flex;
  gap: 6px;
  margin-top: 8px;
}
/* 编辑器 */
.ai-gen {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-bottom: 14px;
  padding: 10px 12px;
  border: 1px dashed var(--qv-divider);
  border-radius: 8px;
}
.ai-input-row {
  display: flex;
  gap: 8px;
  align-items: flex-end;
}
.ai-input-row .n-input {
  flex: 1;
}
.ai-preview {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ai-explain {
  margin: 0;
  font-size: 12px;
  opacity: 0.8;
}
.ai-unmatched {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.ai-unmatched-hint {
  margin: 0;
  font-size: 12px;
  opacity: 0.7;
  flex-basis: 100%;
}
.ai-adopt {
  display: flex;
  justify-content: flex-end;
}
.editor-rows {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.rows-hint {
  margin: 0 0 2px;
  font-size: 12px;
  opacity: 0.6;
}
.cond-row {
  display: flex;
  gap: 6px;
  align-items: center;
  flex-wrap: wrap;
}
.w-factor {
  width: 200px;
}
.w-op {
  width: 120px;
}
.w-num {
  width: 110px;
}
.tilde {
  opacity: 0.5;
}
.editor-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
@media (max-width: 768px) {
  .retail-grid,
  .retail-params {
    grid-template-columns: minmax(0, 1fr);
  }
  .retail-grid {
    column-gap: 0;
  }
  .batch-toolbar,
  .batch-selection,
  .batch-actions {
    align-items: stretch;
  }
  .batch-toolbar {
    flex-direction: column;
  }
  .batch-selection,
  .batch-actions {
    width: 100%;
    justify-content: space-between;
  }
  .ai-input-row {
    flex-direction: column;
    align-items: stretch;
  }
  .cr-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
  .history-selects,
  .revision-compare {
    grid-template-columns: minmax(0, 1fr);
  }
  .revision-pane,
  .revision-pane + .revision-pane {
    padding: 12px 0 0;
    border-left: none;
  }
  .revision-pane + .revision-pane {
    margin-top: 12px;
    border-top: 1px solid var(--qv-divider);
  }
  .w-factor {
    width: 100%;
  }
  .w-op,
  .w-num {
    flex: 1;
    min-width: 96px;
  }
  .reasons-cell {
    max-width: 220px;
  }
}
</style>
