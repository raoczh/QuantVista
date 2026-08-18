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
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getTodos, type TodoItem } from '@/api/todo'
import { getPreference, updatePreference, type UserPreference } from '@/api/user'
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
watch(() => form.value.verify, (value) => { form.value.bear_check = value })

const filters = ref<RecFilters>(emptyRecFilters())
const pref = ref<UserPreference | null>(null)
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
  try { return { ...emptyRecFilters(), ...JSON.parse(raw) } }
  catch { return null }
}
async function loadPreference() {
  try {
    const value = await getPreference()
    pref.value = value
    if (!recTypeExplicitInURL) form.value.type = value.horizon_pref === 'short_term' ? 'short_term' : 'long_term'
    if (!countExplicitInURL && value.default_rec_count >= 3 && value.default_rec_count <= 5) form.value.count = value.default_rec_count
    const saved = parseFilters(value.rec_filters_json)
    if (saved) {
      const merged = { ...saved }
      for (const [, field] of filterQueryFields) {
        if (explicitFilterFields.has(field)) Object.assign(merged, { [field]: filters.value[field] })
      }
      filters.value = merged
    }
  } catch { /* 内置默认值仍可明确提交 */ }
}
function applyGuidePreference(value: UserPreference) {
  pref.value = value
  if (!recTypeExplicitInURL) form.value.type = value.horizon_pref === 'short_term' ? 'short_term' : 'long_term'
  if (!countExplicitInURL) form.value.count = value.default_rec_count
}
async function saveFiltersDefault() {
  if (!pref.value) {
    message.warning('投资偏好尚未加载，请稍后重试')
    return
  }
  savingFilters.value = true
  try {
    pref.value = await updatePreference({ ...pref.value, rec_filters_json: JSON.stringify(filters.value) })
    message.success('已保存为默认筛选；收盘日报自动推荐会使用同一设置')
  } catch (reason) { message.error((reason as Error).message) }
  finally { savingFilters.value = false }
}

const strategies = ref<Strategy[]>([])
const strategyOptions = computed(() => strategies.value.map((item) => ({ label: `${item.name} · ${item.desc}`, value: item.key })))
async function loadStrategies() {
  try {
    strategies.value = await listStrategies(form.value.type)
    if (strategies.value.length && !strategies.value.some((item) => item.key === form.value.strategy)) form.value.strategy = strategies.value[0].key
  } catch (reason) { message.error((reason as Error).message) }
}
watch(() => form.value.type, () => void loadStrategies())

const llmConfigs = ref<LLMConfig[]>([])
const llmOptions = computed(() => llmConfigs.value.map((item) => ({ label: item.is_default ? `${item.name}（默认）` : item.name, value: item.id })))
async function loadLLM() {
  try {
    llmConfigs.value = await listLLMConfigs()
    const fallback = llmConfigs.value.find((item) => item.is_default) || llmConfigs.value[0]
    if (fallback && form.value.llm_config_id === undefined) form.value.llm_config_id = fallback.id
  } catch (reason) { message.error((reason as Error).message) }
}

const current = ref<RecommendationView | null>(null)
const submitting = ref(false)
const currentID = computed(() => current.value?.id || null)
const history = ref<RecommendationBatch[]>([])
const historyLoading = ref(false)
const discovery = ref<DiscoveryStatusView | null>(null)

async function loadHistory() {
  historyLoading.value = true
  try { history.value = await listRecommendations('', 30) }
  catch (reason) { message.error((reason as Error).message) }
  finally { historyLoading.value = false }
}
async function loadDiscovery() {
  try { discovery.value = await getDiscoveryStatus(120) }
  catch {
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
    if (!current.value || current.value.id === id) current.value = value
    notifyResult(value)
  },
  onError: (error) => message.error(error.message),
  onSettled: async () => {
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

let submitLocked = false
async function generate() {
  if (submitLocked || running.value) return
  submitLocked = true
  submitting.value = true
  try {
    if (!pref.value) await loadPreference()
    const created = await generateRecommendations({ ...form.value, filters: { ...filters.value } })
    current.value = created
    await replaceRouteQuery(route, router, { batch_id: created.id })
    await Promise.all([loadHistory(), refreshTask()])
    if (created.status === 'processing') {
      message.info('任务已创建；刷新或关闭页面不影响后台执行')
      void track(created.id)
    } else notifyResult(created)
  } catch (reason) {
    message.error((reason as Error).message)
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
let routeSequence = 0
async function openRouteBatch(): Promise<boolean> {
  const id = routeBatchID()
  if (!id) return false
  if (current.value?.id === id) return true
  const sequence = ++routeSequence
  stop()
  try {
    const value = await getRecommendation(id)
    if (sequence !== routeSequence || routeBatchID() !== id) return true
    current.value = value
    if (value.status === 'processing') void track(id)
  } catch (reason) {
    if (sequence === routeSequence) message.error((reason as Error).message)
  }
  return true
}
watch(() => route.query.batch_id, () => void openRouteBatch())
async function openBatch(item: RecommendationBatch) {
  try {
    stop()
    current.value = await getRecommendation(item.id)
    await replaceRouteQuery(route, router, { batch_id: item.id })
    if (current.value.status === 'processing') void track(item.id)
  } catch (reason) { message.error((reason as Error).message) }
}
async function removeBatch(item: RecommendationBatch) {
  try {
    await deleteRecommendation(item.id)
    if (current.value?.id === item.id) {
      current.value = null
      await replaceRouteQuery(route, router, { batch_id: undefined })
    }
    await loadHistory()
    message.success('本人推荐记录已删除')
  } catch (reason) { message.error((reason as Error).message) }
}
async function cancelCurrentTask() {
  try {
    await cancelTask()
    if (current.value) current.value = await getRecommendation(current.value.id).catch(() => current.value)
    message.success('已提交取消请求')
  } catch (reason) { message.error((reason as Error).message) }
}
async function retryCurrentTask() {
  try {
    const rerun = await retryTask()
    if (!rerun?.result_id) {
      await refreshTask()
      message.info('重试任务已创建，可在任务中心查看')
      return
    }
    const value = await getRecommendation(rerun.result_id)
    current.value = value
    await replaceRouteQuery(route, router, { batch_id: value.id })
    await loadHistory()
    if (value.status === 'processing') void track(value.id)
  } catch (reason) { message.error((reason as Error).message) }
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
  reviewsController?.abort()
  const controller = new AbortController()
  reviewsController = controller
  reviewsLoading.value = true
  reviewsError.value = ''
  try {
    const result = await getTodos('research', controller.signal)
    if (reviewsController !== controller) return
    reviews.value = result.items.filter((item) => item.kind === 'rec_review')
    if (!result.complete) reviewsError.value = result.errors?.filter(Boolean).join('；') || '部分追踪数据暂不可用'
  } catch (reason) {
    if (reviewsController === controller && !isAbortError(reason)) reviewsError.value = (reason as Error).message
  } finally {
    if (reviewsController === controller) reviewsLoading.value = false
  }
}
async function ackReview(item: TodoItem) {
  if (reviewAcking.value) return
  reviewAcking.value = item.ref_id
  try {
    await ackRecommendationReview(item.ref_id)
    await loadReviews()
  } catch (reason) { message.error((reason as Error).message) }
  finally { reviewAcking.value = null }
}
onBeforeUnmount(() => reviewsController?.abort())

const performance = ref<PerformanceStats | null>(null)
const tracking = ref(false)
async function loadPerformance() {
  try { performance.value = await getPerformance(form.value.type) }
  catch { performance.value = null }
}
watch(() => form.value.type, () => void loadPerformance())
async function refreshTracking() {
  if (!current.value || tracking.value) return
  tracking.value = true
  try {
    current.value = await trackRecommendation(current.value.id)
    await loadPerformance()
    message.success('已追加最新追踪状态；历史推荐内容未改写')
  } catch (reason) { message.error((reason as Error).message) }
  finally { tracking.value = false }
}
const stopAlerting = ref<Record<number, boolean>>({})
async function addStopAlert(item: RecommendationItem) {
  if (stopAlerting.value[item.id]) return
  stopAlerting.value = { ...stopAlerting.value, [item.id]: true }
  try {
    await createStopLossAlert(item.id)
    message.success(`已为 ${item.name || item.symbol} 设置止损提醒；不会自动下单`)
  } catch (reason) { message.error((reason as Error).message) }
  finally { stopAlerting.value = { ...stopAlerting.value, [item.id]: false } }
}

type ResultSection = 'pool' | 'rejected'
const resultSectionsQuery = enumListQuery<ResultSection>(['pool', 'rejected'], 2)
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
  queryRef('price_min', filterState('price_min'), numberQuery(0, 0, 1_000_000)),
  queryRef('price_max', filterState('price_max'), numberQuery(50, 0, 1_000_000)),
  queryRef('cap_min', filterState('float_cap_min_yi'), numberQuery(0, 0, 100_000_000)),
  queryRef('cap_max', filterState('float_cap_max_yi'), numberQuery(0, 0, 100_000_000)),
  queryRef('turnover_min', filterState('turnover_min'), numberQuery(0, 0, 25)),
  queryRef('turnover_max', filterState('turnover_max'), numberQuery(0, 0, 30)),
  queryRef('max_gain_5d', filterState('max_gain_5d_pct'), numberQuery(25, 0, 100)),
  queryRef('exclude_limit_up', filterState('exclude_limit_up'), booleanQuery(true)),
  queryRef('exclude_gem_star', filterState('exclude_gem_star'), booleanQuery(false)),
  queryRef('sections', resultSections, resultSectionsQuery),
])
const { restoreScroll } = useListPageScroll(route, 'recommendations')

onMounted(async () => {
  await Promise.all([loadStrategies(), loadLLM(), loadHistory(), loadPerformance(), loadPreference(), loadReviews(), loadDiscovery()])
  if (await openRouteBatch()) {
    await restoreScroll()
    return
  }
  const processing = history.value.find((item) => item.status === 'processing')
  if (processing) {
    current.value = await getRecommendation(processing.id).catch(() => null)
    if (current.value) {
      await replaceRouteQuery(route, router, { batch_id: processing.id })
      void track(processing.id)
    }
  }
  await restoreScroll()
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
          :loading="running"
          :tracking="tracking"
          :stop-alerting="stopAlerting"
          @refresh-tracking="refreshTracking"
          @stop-alert="addStopAlert"
        />
        <RecommendationHistoryTracking
          :history="history"
          :current-i-d="current?.id"
          :history-loading="historyLoading"
          :reviews="reviews"
          :reviews-loading="reviewsLoading"
          :reviews-error="reviewsError"
          :review-acking="reviewAcking"
          :performance="performance"
          @open="openBatch"
          @remove="removeBatch"
          @refresh-history="loadHistory"
          @refresh-reviews="loadReviews"
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
          :strategy-options="strategyOptions"
          :market-options="marketOptions"
          :price-preset-options="pricePresetOptions"
          :cap-preset-options="capPresetOptions"
          :llm-options="llmOptions"
          :llm-configured="!!llmConfigs.length"
          :running="running"
          :saving-filters="savingFilters"
          @generate="generate"
          @save-filters="saveFiltersDefault"
          @preferences="showInvestmentGuide = true"
          @onboarding="router.push({ query: { ...route.query, onboarding: '1' } })"
        />
        <AiTaskStatusPanel
          :task="task"
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
</style>
