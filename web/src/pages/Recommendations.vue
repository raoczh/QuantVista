<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useMessage } from 'naive-ui'
import {
  ackRecommendationReview,
  createStopLossAlert,
  deleteRecommendation,
  emptyRecFilters,
  generateRecommendations,
  getDiscoveryStatus,
  getPerformance,
  getRecommendation,
  listRecommendations,
  listStrategies,
  trackRecommendation,
  type DiscoveryStatusView,
  type PerformanceStats,
  type RecFilters,
  type RecommendRequest,
  type RecommendationBatch,
  type RecommendationItem,
  type RecommendationView,
  type Strategy,
} from '@/api/recommendation'
import { isAbortError } from '@/api/client'
import { SCORE_PROFILE_LABEL } from '@/api/screener'
import { getSessionEpoch } from '@/api/token'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getTodos, type TodoItem } from '@/api/todo'
import { getPreference, updatePreference, type UserPreference, type UserPreferenceUpdate } from '@/api/user'
import {
  booleanQuery,
  enumListQuery,
  enumQuery,
  integerQuery,
  numberQuery,
  queryRef,
  replaceRouteQuery,
  stringQuery,
  useListPageScroll,
  useRouteQueryState,
} from '@/composables/useListPageState'
import { useBusinessTask } from '@/composables/useBusinessTask'
import { useResultPolling } from '@/composables/useResultPolling'
import PageContainer from '@/components/PageContainer.vue'
import DisplayModeSwitch from '@/components/DisplayModeSwitch.vue'
import InvestmentPreferenceGuide from '@/components/InvestmentPreferenceGuide.vue'
import AiTaskStatusPanel from '@/components/ai/AiTaskStatusPanel.vue'
import RecommendationGenerator from '@/components/recommendations/RecommendationGenerator.vue'
import RecommendationHistoryTracking from '@/components/recommendations/RecommendationHistoryTracking.vue'
import RecommendationResearchAudit from '@/components/recommendations/RecommendationResearchAudit.vue'
import RecommendationResultsWorkspace from '@/components/recommendations/RecommendationResultsWorkspace.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const pageSession = getSessionEpoch()
const pageRouteName = route.name
let disposed = false
const active = () => !disposed && route.name === pageRouteName && getSessionEpoch() === pageSession
const errorText = (reason: unknown) => reason instanceof Error ? reason.message : '请求处理失败，请稍后重试'

const filterQueryFields: Array<[string, keyof RecFilters]> = [
  ['price_min', 'price_min'],
  ['price_max', 'price_max'],
  ['cap_min', 'float_cap_min_yi'],
  ['cap_max', 'float_cap_max_yi'],
  ['turnover_min', 'turnover_min'],
  ['turnover_max', 'turnover_max'],
  ['max_gain_5d', 'max_gain_5d_pct'],
  ['exclude_limit_up', 'exclude_limit_up'],
  ['exclude_gem_star', 'exclude_gem_star'],
]
const explicitFilterFields = new Set(
  filterQueryFields.filter(([key]) => route.query[key] !== undefined).map(([, field]) => field),
)
const recTypeExplicitInURL = route.query.rec_type !== undefined
const countExplicitInURL = route.query.count !== undefined
const recTypeQuery = enumQuery<'short_term' | 'long_term'>('short_term', ['short_term', 'long_term'])
const strategyQuery = stringQuery('', 80)
const countQuery = integerQuery(5, 3, 5)
const verifyQuery = booleanQuery(false)
const bearCheckQuery = booleanQuery(false)
const initialVerify = verifyQuery.parse(route.query.verify)

const form = ref<RecommendRequest>({
  type: recTypeQuery.parse(route.query.rec_type),
  market: 'cn',
  strategy: strategyQuery.parse(route.query.strategy),
  count: countQuery.parse(route.query.count),
  verify: initialVerify,
  bear_check: route.query.bear_check === undefined ? initialVerify : bearCheckQuery.parse(route.query.bear_check),
})

const filters = ref<RecFilters>(emptyRecFilters())
const pref = ref<UserPreference | null>(null)
const preferenceLoading = ref(false)
const preferenceError = ref('')
let preferenceSequence = 0
const savingFilters = ref(false)
const showInvestmentGuide = ref(false)
const marketOptions = [{ label: 'A 股', value: 'cn' }]
const pricePresets = [
  { label: '价格不限', value: 0 },
  { label: '不高于 10 元', value: 10 },
  { label: '不高于 20 元', value: 20 },
  { label: '不高于 30 元', value: 30 },
  { label: '不高于 50 元', value: 50 },
]
const priceCustom = ref(false)
const pricePreset = computed({
  get: () => !priceCustom.value && filters.value.price_min === 0 && pricePresets.some((item) => item.value === filters.value.price_max)
    ? filters.value.price_max
    : -1,
  set: (value: number) => {
    priceCustom.value = value === -1
    if (value !== -1) filters.value = { ...filters.value, price_min: 0, price_max: value }
  },
})
const pricePresetOptions = [...pricePresets, { label: '自定义价格区间', value: -1 }]
const capPresets = [
  { label: '市值不限', min: 0, max: 0 },
  { label: '不高于 50 亿', min: 0, max: 50 },
  { label: '30 - 200 亿', min: 30, max: 200 },
  { label: '200 - 800 亿', min: 200, max: 800 },
  { label: '不低于 800 亿', min: 800, max: 0 },
]
const capCustom = ref(false)
const capPreset = computed({
  get: () => {
    if (capCustom.value) return -1
    const index = capPresets.findIndex((item) => item.min === filters.value.float_cap_min_yi && item.max === filters.value.float_cap_max_yi)
    return index >= 0 ? index : -1
  },
  set: (value: number) => {
    capCustom.value = value === -1
    if (value !== -1) {
      filters.value = {
        ...filters.value,
        float_cap_min_yi: capPresets[value].min,
        float_cap_max_yi: capPresets[value].max,
      }
    }
  },
})
const capPresetOptions = [...capPresets.map((item, value) => ({ label: item.label, value })), { label: '自定义市值区间', value: -1 }]

function parseFilters(raw?: string | null): RecFilters | null {
  if (!raw) return null
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null
    const saved = emptyRecFilters()
    for (const [, field] of filterQueryFields) {
      const value = (parsed as Record<string, unknown>)[field]
      if (value === undefined) continue
      if (typeof saved[field] === 'boolean' ? typeof value === 'boolean' : typeof value === 'number' && Number.isFinite(value) && value >= 0) {
        Object.assign(saved, { [field]: value })
      } else return null
    }
    return saved
  }
  catch { return null }
}
async function loadPreference() {
  if (!active()) return
  const sequence = ++preferenceSequence
  const before = { type: form.value.type, count: form.value.count, filters: { ...filters.value } }
  preferenceLoading.value = true
  preferenceError.value = ''
  try {
    const value = await getPreference()
    if (!active() || sequence !== preferenceSequence) return
    pref.value = value
    if (!recTypeExplicitInURL && route.query.rec_type === undefined && form.value.type === before.type) form.value.type = value.horizon_pref === 'short_term' ? 'short_term' : 'long_term'
    if (!countExplicitInURL && route.query.count === undefined && form.value.count === before.count && value.default_rec_count >= 3 && value.default_rec_count <= 5) form.value.count = value.default_rec_count
    const saved = parseFilters(value.rec_filters_json)
    if (value.rec_filters_json?.trim() && !saved) preferenceError.value = '默认筛选数据无效，请在设置中重新保存'
    if (saved) {
      const merged = { ...filters.value }
      for (const [key, field] of filterQueryFields) {
        if (!explicitFilterFields.has(field) && route.query[key] === undefined && Object.is(filters.value[field], before.filters[field])) Object.assign(merged, { [field]: saved[field] })
      }
      filters.value = merged
    }
  } catch (reason) {
    if (active() && sequence === preferenceSequence) preferenceError.value = errorText(reason)
  } finally {
    if (sequence === preferenceSequence) preferenceLoading.value = false
  }
}
function applyGuidePreference(value: UserPreference, fields: UserPreferenceUpdate) {
  if (!active()) return
  preferenceSequence++
  preferenceLoading.value = false
  pref.value = value
  preferenceError.value = ''
  if (fields.horizon_pref !== undefined) form.value.type = value.horizon_pref === 'short_term' ? 'short_term' : 'long_term'
}
async function saveFiltersDefault() {
  if (!active() || savingFilters.value || showInvestmentGuide.value) return
  if (!pref.value) {
    message.warning('投资偏好尚未加载，请稍后重试')
    return
  }
  preferenceSequence++
  preferenceLoading.value = false
  savingFilters.value = true
  try {
    const value = await updatePreference({ rec_filters_json: JSON.stringify(filters.value) })
    if (!active()) return
    pref.value = value
    message.success('已保存为默认筛选；收盘日报自动推荐会使用同一设置')
  } catch (reason) { if (active()) message.error(errorText(reason)) }
  finally { savingFilters.value = false }
}

const strategies = ref<Strategy[]>([])
const strategiesLoading = ref(false)
const strategiesError = ref('')
let strategiesSequence = 0
// 策略下拉分组：推荐内置 / 我的策略 / 选股内置 / 新手模板（选股页全部策略均可作推荐策略）。
const strategyGroupLabels: Record<string, string> = { rec: '推荐策略', custom: '我的选股策略', screen: '内置选股策略', template: '新手模板' }
const selectedStrategy = computed(() => strategies.value.find((item) => item.key === form.value.strategy) || null)
const strategyDescription = computed(() => {
  const strategy = selectedStrategy.value
  if (!strategy) return ''
  const profile = strategy.score_profile && SCORE_PROFILE_LABEL[strategy.score_profile]
  return [strategy.desc, profile ? `评分侧重：${profile}` : ''].filter(Boolean).join(' · ')
})
const strategyOptions = computed(() => {
  const groups = new Map<string, Array<{ label: string; value: string; desc?: string }>>()
  for (const item of strategies.value) {
    const group = item.group || 'rec'
    if (!groups.has(group)) groups.set(group, [])
    // 选股类策略的 desc 已带「周期 · 风险 · 讲解首句」，与名称拼在一起会很长；label 只放名称，
    // desc 交给下拉项副标题（renderLabel）与选中后的说明行展示。
    groups.get(group)!.push({ label: item.name, value: item.key, desc: item.desc })
  }
  if (groups.size <= 1) return [...groups.values()].flat()
  return [...groups.entries()].map(([key, children]) => ({ type: 'group' as const, label: strategyGroupLabels[key] || key, key, children }))
})
async function loadStrategies() {
  if (!active()) return
  const sequence = ++strategiesSequence
  const type = form.value.type
  strategiesLoading.value = true
  strategiesError.value = ''
  strategies.value = []
  try {
    const result = await listStrategies(type)
    if (!active() || sequence !== strategiesSequence || type !== form.value.type) return
    strategies.value = result
    if (strategies.value.length && !strategies.value.some((item) => item.key === form.value.strategy)) form.value.strategy = strategies.value[0].key
  } catch (reason) {
    if (!active() || sequence !== strategiesSequence) return
    strategiesError.value = errorText(reason)
  } finally {
    if (sequence === strategiesSequence) strategiesLoading.value = false
  }
}
watch(() => form.value.type, () => void loadStrategies())

const llmConfigs = ref<LLMConfig[]>([])
const llmLoading = ref(false)
const llmError = ref('')
let llmSequence = 0
const llmOptions = computed(() => llmConfigs.value.map((item) => ({ label: item.is_default ? `${item.name}（默认）` : item.name, value: item.id })))
async function loadLLM() {
  if (!active()) return
  const sequence = ++llmSequence
  llmLoading.value = true
  llmError.value = ''
  try {
    const value = await listLLMConfigs()
    if (!active() || sequence !== llmSequence) return
    llmConfigs.value = value
    const fallback = llmConfigs.value.find((item) => item.is_default) || llmConfigs.value[0]
    if (fallback && form.value.llm_config_id === undefined) form.value.llm_config_id = fallback.id
  } catch (reason) { if (active() && sequence === llmSequence) llmError.value = errorText(reason) }
  finally { if (sequence === llmSequence) llmLoading.value = false }
}

const current = ref<RecommendationView | null>(null)
const resultLoading = ref(false)
const resultError = ref('')
let resultSequence = 0
let requestedBatchID: number | null = null
let resultController: AbortController | null = null
const deleting = ref(new Set<number>())
const deleted = new Set<number>()
const submitting = ref(false)
const currentID = computed(() => current.value?.id || null)
const history = ref<RecommendationBatch[]>([])
const historyLoading = ref(false)
const historyError = ref('')
let historySequence = 0
const discovery = ref<DiscoveryStatusView | null>(null)
let discoverySequence = 0

async function loadHistory() {
  if (!active()) return
  const sequence = ++historySequence
  historyLoading.value = true
  historyError.value = ''
  try {
    const value = await listRecommendations('', 30)
    if (active() && sequence === historySequence) history.value = value.filter(item => !deleted.has(item.id))
  } catch (reason) { if (active() && sequence === historySequence) historyError.value = errorText(reason) }
  finally { if (sequence === historySequence) historyLoading.value = false }
}
async function loadDiscovery() {
  if (!active()) return
  const sequence = ++discoverySequence
  try {
    const value = await getDiscoveryStatus(120)
    if (active() && sequence === discoverySequence) discovery.value = value
  }
  catch {
    if (!active() || sequence !== discoverySequence) return
    discovery.value = { scope: 'global', market: 'cn', status: 'unavailable', reason: '发现状态暂不可用', run: null, items: [], channels: [] }
  }
}
function notifyResult(value: RecommendationView) {
  if (value.status === 'failed') message.error(value.error || '推荐生成失败')
  else if (value.status === 'degraded') message.warning(value.error || '推荐部分成功，请查看缺失原因')
  else if (value.items.length) message.success(`已生成 ${value.items.length} 条推荐研究结果`)
}

const { polling, track, stop } = useResultPolling<RecommendationView>({
  load: getRecommendation,
  isDone: (value) => value.status !== 'processing',
  onResult: (id, value) => {
    if (!active() || requestedBatchID !== id || deleted.has(id)) return
    current.value = value
    notifyResult(value)
  },
  onError: (error) => { if (active()) resultError.value = error.message },
  onSettled: async () => {
    if (!active()) return
    await Promise.all([loadHistory(), refreshTask()])
  },
})
const running = computed(() => submitting.value || polling.value)
const {
  task,
  loading: taskLoading,
  actionLoading: taskActionLoading,
  error: taskError,
  refresh: refreshTask,
  cancel: cancelTask,
  retry: retryTask,
} = useBusinessTask('recommendation', currentID)

function beginResultOperation(id: number | null) {
  resultController?.abort()
  resultController = null
  stop()
  requestedBatchID = id
  resultLoading.value = false
  resultError.value = ''
  tracking.value = false
  return ++resultSequence
}
const ownsResult = (sequence: number) => active() && sequence === resultSequence

let submitLocked = false
async function generate() {
  if (submitLocked || running.value) return
  if (!active()) return
  if (preferenceLoading.value || llmLoading.value) {
    message.warning('生成参数正在读取，请稍后再生成')
    return
  }
  if (!selectedStrategy.value || strategiesLoading.value) {
    message.warning('请先加载并选择有效策略')
    return
  }
  if (!Number.isInteger(form.value.count) || (form.value.count || 0) < 3 || (form.value.count || 0) > 5) {
    message.warning('请选择 3 至 5 条推荐结果')
    return
  }
  const request: RecommendRequest = {
    ...form.value,
    strategy_revision_id: selectedStrategy.value.strategy_revision_id,
    filters: { ...filters.value },
  }
  submitLocked = true
  submitting.value = true
  const sequence = beginResultOperation(null)
  historySequence++
  try {
    const created = await generateRecommendations(request)
    if (!active()) return
    void loadHistory()
    if (!ownsResult(sequence)) return
    requestedBatchID = created.id
    current.value = created
    await replaceRouteQuery(route, router, { batch_id: created.id })
    if (!ownsResult(sequence)) return
    await refreshTask()
    if (!ownsResult(sequence)) return
    if (created.status === 'processing') {
      message.info('任务已创建；刷新或关闭页面不影响后台执行')
      void track(created.id)
    } else notifyResult(created)
  } catch (reason) {
    if (ownsResult(sequence) && !isAbortError(reason)) message.error(errorText(reason))
  } finally {
    submitLocked = false
    submitting.value = false
  }
}

function routeBatchID(): number | null {
  const raw = Array.isArray(route.query.batch_id) ? route.query.batch_id[0] : route.query.batch_id
  const id = Number(raw)
  return Number.isSafeInteger(id) && id > 0 ? id : null
}
async function openRouteBatch(): Promise<boolean> {
  const id = routeBatchID()
  if (!id) {
    beginResultOperation(null)
    current.value = null
    if (route.query.batch_id !== undefined) resultError.value = '推荐批次编号无效，请从历史记录重新选择'
    return route.query.batch_id !== undefined
  }
  if (current.value?.id === id && requestedBatchID === id) return true
  await loadBatch(id, false)
  return true
}
async function loadBatch(id: number, updateRoute: boolean) {
  if (!active() || deleting.value.has(id) || deleted.has(id)) return
  const sequence = beginResultOperation(id)
  const controller = new AbortController()
  resultController = controller
  resultLoading.value = true
  try {
    const value = await getRecommendation(id, controller.signal)
    if (!ownsResult(sequence) || deleted.has(id)) return
    current.value = value
    if (updateRoute) await replaceRouteQuery(route, router, { batch_id: id })
    if (!ownsResult(sequence)) return
    if (value.status === 'processing') void track(id)
  } catch (reason) {
    if (ownsResult(sequence) && !isAbortError(reason)) resultError.value = errorText(reason)
  } finally {
    if (sequence === resultSequence) {
      resultLoading.value = false
      resultController = null
    }
  }
}
watch(() => route.query.batch_id, () => { if (active()) void openRouteBatch() })
async function openBatch(item: RecommendationBatch) {
  await loadBatch(item.id, true)
}
async function retryResult() {
  const id = requestedBatchID || routeBatchID()
  if (id) await loadBatch(id, true)
}
async function removeBatch(item: RecommendationBatch) {
  if (!active() || deleting.value.has(item.id)) return
  deleting.value.add(item.id)
  historySequence++
  if (requestedBatchID === item.id) beginResultOperation(null)
  try {
    await deleteRecommendation(item.id)
    if (!active()) return
    deleted.add(item.id)
    history.value = history.value.filter(row => row.id !== item.id)
    if (current.value?.id === item.id) {
      current.value = null
      if (routeBatchID() === item.id) await replaceRouteQuery(route, router, { batch_id: undefined })
    }
    if (!active()) return
    await loadHistory()
    if (active()) message.success('本人推荐记录已删除')
  } catch (reason) { if (active()) message.error(errorText(reason)) }
  finally { deleting.value.delete(item.id) }
}
async function cancelCurrentTask() {
  if (!active() || !current.value) return
  const sequence = resultSequence, id = current.value.id
  try {
    const updated = await cancelTask()
    if (!updated || !ownsResult(sequence) || current.value?.id !== id) return
    const value = await getRecommendation(id)
    if (!ownsResult(sequence) || current.value?.id !== id) return
    current.value = value
    message.success('已提交取消请求')
  } catch (reason) { if (ownsResult(sequence)) message.error(errorText(reason)) }
}
async function retryCurrentTask() {
  if (!active() || !current.value) return
  const sequence = resultSequence
  try {
    const rerun = await retryTask()
    if (!rerun || !ownsResult(sequence)) return
    if (!rerun.result_id) {
      await refreshTask()
      if (ownsResult(sequence)) message.info('重试任务已创建，可在任务中心查看')
      return
    }
    await loadBatch(rerun.result_id, true)
    if (!active()) return
    await loadHistory()
  } catch (reason) { if (ownsResult(sequence)) message.error(errorText(reason)) }
}
function openTaskAudit() {
  void router.push({ name: 'tasks', query: task.value ? { job_id: String(task.value.source_id) } : { source: 'job', kind: 'recommendation' } })
}

const reviews = ref<TodoItem[]>([])
const reviewsLoading = ref(false)
const reviewsError = ref('')
const reviewAcking = ref<number | null>(null)
let reviewsController: AbortController | null = null
async function loadReviews() {
  if (!active()) return
  reviewsController?.abort()
  const controller = new AbortController()
  reviewsController = controller
  reviewsLoading.value = true
  reviewsError.value = ''
  try {
    const result = await getTodos('research', controller.signal)
    if (!active() || reviewsController !== controller) return
    reviews.value = result.items.filter((item) => item.kind === 'rec_review')
    if (!result.complete) reviewsError.value = result.errors?.filter(Boolean).join('；') || '部分追踪数据暂不可用'
  } catch (reason) {
    if (active() && reviewsController === controller && !isAbortError(reason)) reviewsError.value = errorText(reason)
  } finally {
    if (reviewsController === controller) reviewsLoading.value = false
  }
}
async function ackReview(item: TodoItem) {
  if (!active() || reviewAcking.value) return
  reviewAcking.value = item.ref_id
  try {
    await ackRecommendationReview(item.ref_id)
    if (!active()) return
    await loadReviews()
  } catch (reason) { if (active()) message.error(errorText(reason)) }
  finally { reviewAcking.value = null }
}

const performance = ref<PerformanceStats | null>(null)
const performanceLoading = ref(false)
const performanceError = ref('')
let performanceSequence = 0
const tracking = ref(false)
async function loadPerformance() {
  if (!active()) return
  const sequence = ++performanceSequence, type = form.value.type
  performanceLoading.value = true
  performanceError.value = ''
  if (performance.value?.type !== type) performance.value = null
  try {
    const value = await getPerformance(type)
    if (active() && sequence === performanceSequence && form.value.type === type) performance.value = value
  } catch (reason) { if (active() && sequence === performanceSequence) performanceError.value = errorText(reason) }
  finally { if (sequence === performanceSequence) performanceLoading.value = false }
}
watch(() => form.value.type, () => void loadPerformance())
async function refreshTracking() {
  if (!active() || !current.value || tracking.value || deleting.value.has(current.value.id)) return
  const sequence = resultSequence, id = current.value.id
  tracking.value = true
  try {
    const value = await trackRecommendation(id)
    if (!ownsResult(sequence) || current.value?.id !== id) return
    current.value = value
    await loadPerformance()
    if (ownsResult(sequence)) message.success('已追加最新追踪状态；历史推荐内容未改写')
  } catch (reason) { if (ownsResult(sequence)) message.error(errorText(reason)) }
  finally { if (sequence === resultSequence) tracking.value = false }
}
const stopAlerting = ref<Record<number, boolean>>({})
/**
 * 补关联持仓血缘后重载当前批次。
 *
 * 只需轻量 Get——补关联接口已同步回填追踪状态里的实际买入价/收益（终态推荐被冻结，
 * 走 refreshTracking 也补不上），这里重载只为拿到 position 血缘字段，不必再发上游请求。
 */
async function reloadAfterLink(id = current.value?.id) {
  if (!active() || !id || current.value?.id !== id) return
  const sequence = resultSequence
  try {
    const value = await getRecommendation(id)
    if (ownsResult(sequence) && current.value?.id === id) current.value = value
  } catch (reason) { if (ownsResult(sequence)) resultError.value = errorText(reason) }
}
async function addStopAlert(item: RecommendationItem) {
  if (!active() || stopAlerting.value[item.id]) return
  const sequence = resultSequence
  stopAlerting.value = { ...stopAlerting.value, [item.id]: true }
  try {
    await createStopLossAlert(item.id)
    if (ownsResult(sequence)) message.success(`已为 ${item.name || '名称待补全'}（${item.symbol}）设置止损提醒；不会自动下单`)
  } catch (reason) { if (ownsResult(sequence)) message.error(errorText(reason)) }
  finally { stopAlerting.value = { ...stopAlerting.value, [item.id]: false } }
}

type ResultSection = 'pool' | 'excluded' | 'rejected' | 'raw' | 'sources'
const resultSectionsQuery = enumListQuery<ResultSection>(['pool', 'excluded', 'rejected', 'raw', 'sources'], 5)
const resultSections = ref<ResultSection[]>(resultSectionsQuery.parse(route.query.sections))
const auditMode = ref<'' | 'attribution' | 'shadow' | 'recall'>('')
const recTypeState = computed({ get: () => form.value.type, set: (value: 'short_term' | 'long_term') => { form.value.type = value } })
const strategyState = computed({ get: () => form.value.strategy || '', set: (value: string) => { form.value.strategy = value } })
const countState = computed({ get: () => form.value.count || 5, set: (value: number) => { form.value.count = value } })
const verifyState = computed({ get: () => !!form.value.verify, set: (value: boolean) => { form.value.verify = value } })
const bearState = computed({ get: () => !!form.value.bear_check, set: (value: boolean) => { form.value.bear_check = value } })
function filterState<K extends keyof RecFilters>(key: K) {
  return computed<RecFilters[K]>({ get: () => filters.value[key], set: (value) => { filters.value = { ...filters.value, [key]: value } } })
}
useRouteQueryState(route, router, [
  queryRef('rec_type', recTypeState, recTypeQuery),
  queryRef('strategy', strategyState, strategyQuery),
  queryRef('count', countState, countQuery),
  queryRef('verify', verifyState, verifyQuery),
  queryRef('bear_check', bearState, bearCheckQuery),
  queryRef('price_min', filterState('price_min'), numberQuery(0, 0, 100_000)),
  queryRef('price_max', filterState('price_max'), numberQuery(50, 0, 100_000)),
  queryRef('cap_min', filterState('float_cap_min_yi'), numberQuery(0, 0, 1_000_000)),
  queryRef('cap_max', filterState('float_cap_max_yi'), numberQuery(0, 0, 1_000_000)),
  queryRef('turnover_min', filterState('turnover_min'), numberQuery(0, 0, 25)),
  queryRef('turnover_max', filterState('turnover_max'), numberQuery(0, 0, 30)),
  queryRef('max_gain_5d', filterState('max_gain_5d_pct'), numberQuery(25, 0, 1000)),
  queryRef('exclude_limit_up', filterState('exclude_limit_up'), booleanQuery(true)),
  queryRef('exclude_gem_star', filterState('exclude_gem_star'), booleanQuery(false)),
  queryRef('sections', resultSections, resultSectionsQuery),
])
const { restoreScroll } = useListPageScroll(route, 'recommendations')

onMounted(async () => {
  const sequence = resultSequence
  await Promise.all([loadStrategies(), loadLLM(), loadHistory(), loadPerformance(), loadPreference(), loadReviews(), loadDiscovery()])
  if (!ownsResult(sequence)) return
  if (await openRouteBatch()) {
    if (active()) await restoreScroll()
    return
  }
  if (!active()) return
  const processing = history.value.find((item) => item.status === 'processing')
  if (processing) await loadBatch(processing.id, true)
  if (active()) await restoreScroll()
})
onBeforeUnmount(() => {
  disposed = true
  resultSequence++
  resultController?.abort()
  reviewsController?.abort()
  stop()
})
</script>

<template>
  <PageContainer title="AI 推荐工作台" subtitle="今天能研究什么、任务为什么这样运行、历史结果后来怎样">
    <template #actions><DisplayModeSwitch /></template>
    <div class="workspace">
      <div class="main-column">
        <RecommendationResultsWorkspace
          v-model:sections="resultSections"
          :current="current"
          :discovery="discovery"
          :loading="running || resultLoading"
          :error="resultError"
          :tracking="tracking"
          :stop-alerting="stopAlerting"
          @refresh-tracking="refreshTracking"
          @stop-alert="addStopAlert"
          @linked="reloadAfterLink"
          @retry="retryResult"
        />
        <RecommendationHistoryTracking
          :history="history"
          :current-i-d="current?.id"
          :history-loading="historyLoading"
          :history-error="historyError"
          :deleting="deleting"
          :reviews="reviews"
          :reviews-loading="reviewsLoading"
          :reviews-error="reviewsError"
          :review-acking="reviewAcking"
          :performance="performance"
          :performance-loading="performanceLoading"
          :performance-error="performanceError"
          @open="openBatch"
          @remove="removeBatch"
          @refresh-history="loadHistory"
          @refresh-reviews="loadReviews"
          @refresh-performance="loadPerformance"
          @ack-review="ackReview"
          @audit="auditMode = $event"
        />
      </div>
      <aside class="side-column">
        <RecommendationGenerator
          v-model:form="form"
          v-model:filters="filters"
          v-model:price-preset="pricePreset"
          v-model:cap-preset="capPreset"
          :pref="pref"
          :preference-loading="preferenceLoading"
          :preference-error="preferenceError"
          :strategy-options="strategyOptions"
          :strategies-loading="strategiesLoading"
          :strategies-error="strategiesError"
          :strategy-desc="strategyDescription"
          :market-options="marketOptions"
          :price-preset-options="pricePresetOptions"
          :cap-preset-options="capPresetOptions"
          :llm-options="llmOptions"
          :llm-configured="!!llmConfigs.length"
          :llm-loading="llmLoading"
          :llm-error="llmError"
          :running="running"
          :submitting="submitting"
          :saving-filters="savingFilters"
          @generate="generate"
          @reload-strategies="loadStrategies"
          @reload-preference="loadPreference"
          @reload-llm="loadLLM"
          @save-filters="saveFiltersDefault"
          @preferences="() => { if (!savingFilters && active()) showInvestmentGuide = true }"
          @onboarding="router.push({ query: { ...route.query, onboarding: '1' } })"
        />
        <AiTaskStatusPanel
          :task="task"
          :result-i-d="currentID"
          :loading="taskLoading"
          :action-loading="taskActionLoading"
          :error="taskError"
          @refresh="refreshTask"
          @cancel="cancelCurrentTask"
          @retry="retryCurrentTask"
          @audit="openTaskAudit"
        />
      </aside>
    </div>
    <RecommendationResearchAudit v-model="auditMode" :type="form.type" />
    <InvestmentPreferenceGuide v-model="showInvestmentGuide" :preference="pref" @updated="applyGuidePreference" />
  </PageContainer>
</template>

<style scoped>
.workspace {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(320px, 380px);
  align-items: start;
  gap: 16px;
}
.main-column,
.side-column {
  display: grid;
  min-width: 0;
  gap: 16px;
}
.side-column {
  position: sticky;
  top: 76px;
}
@media (max-width: 1050px) {
  .workspace { grid-template-columns: 1fr; }
  .side-column { position: static; grid-row: 1; }
}
@media (max-height: 700px) {
  .side-column { position: static; }
}
</style>
