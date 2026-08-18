<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useDialog, useMessage } from 'naive-ui'
import {
  createAnalysis,
  deleteAnalysis,
  getAnalysis,
  listAnalysis,
  type AnalyzeRequest,
  type AnalysisModule,
  type AnalysisRecord,
  type AnalysisView,
} from '@/api/analysis'
import { getApiErrorCode } from '@/api/client'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getPreference } from '@/api/user'
import type { StockRef } from '@/composables/useStockActions'
import { useBusinessTask } from '@/composables/useBusinessTask'
import { useResultPolling } from '@/composables/useResultPolling'
import {
  enumQuery,
  queryRef,
  replaceRouteQuery,
  useListPageScroll,
  useRouteQueryState,
} from '@/composables/useListPageState'
import PageContainer from '@/components/PageContainer.vue'
import DisplayModeSwitch from '@/components/DisplayModeSwitch.vue'
import AiTaskStatusPanel from '@/components/ai/AiTaskStatusPanel.vue'
import AnalysisHistory from '@/components/analysis/AnalysisHistory.vue'
import AnalysisLauncher from '@/components/analysis/AnalysisLauncher.vue'
import AnalysisResultWorkspace from '@/components/analysis/AnalysisResultWorkspace.vue'

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const router = useRouter()

const moduleOptions: Array<{ label: string; value: AnalysisModule }> = [
  { label: '单票分析', value: 'stock' },
  { label: '本人持仓组合分析', value: 'position' },
  { label: '本人自选组合分析', value: 'watchlist' },
  { label: '全市场分析', value: 'market' },
  { label: '板块分析', value: 'sector' },
]
const marketOptions = [{ label: 'A 股', value: 'cn' }]
const form = ref<AnalyzeRequest>({ module: 'stock', market: 'cn', symbol: '', target: '', question: '' })
const selectedStock = ref<StockRef | null>(null)
const panelMode = ref(false)
const verifyMode = ref(false)
const asOfTs = ref<number | null>(null)
const asOf = computed(() => {
  if (!asOfTs.value) return ''
  const value = new Date(asOfTs.value)
  const pad = (part: number) => String(part).padStart(2, '0')
  return `${value.getFullYear()}-${pad(value.getMonth() + 1)}-${pad(value.getDate())}`
})
const riskLabel = ref('均衡')

function updateSelectedStock(stock: StockRef | null) {
  selectedStock.value = stock
  form.value.symbol = stock?.symbol || ''
  if (stock) form.value.market = stock.market || 'cn'
}
async function loadPreference() {
  try {
    const pref = await getPreference()
    riskLabel.value = pref.risk_level === 'conservative' ? '保守' : pref.risk_level === 'aggressive' ? '激进' : '均衡'
  } catch { riskLabel.value = '均衡' }
}

const llmConfigs = ref<LLMConfig[]>([])
const llmLoading = ref(false)
const llmOptions = computed(() => llmConfigs.value.map((item) => ({ label: item.is_default ? `${item.name}（默认）` : item.name, value: item.id })))
async function loadLLM() {
  llmLoading.value = true
  try {
    llmConfigs.value = await listLLMConfigs()
    const fallback = llmConfigs.value.find((item) => item.is_default) || llmConfigs.value[0]
    if (fallback && form.value.llm_config_id === undefined) form.value.llm_config_id = fallback.id
  } catch (reason) { message.error((reason as Error).message) }
  finally { llmLoading.value = false }
}

const current = ref<AnalysisView | null>(null)
const submitting = ref(false)
const currentID = computed(() => current.value?.id || null)
const history = ref<AnalysisRecord[]>([])
const historyLoading = ref(false)
type HistoryModule = 'all' | AnalysisModule
const historyModuleQuery = enumQuery<HistoryModule>('all', ['all', 'stock', 'market', 'sector', 'watchlist', 'position'])
const historyModule = ref<HistoryModule>(historyModuleQuery.parse(route.query.history_module))
const historyFilterOptions = [{ label: '全部范围', value: 'all' }, ...moduleOptions]
useRouteQueryState(route, router, [queryRef('history_module', historyModule, historyModuleQuery)])
const { restoreScroll } = useListPageScroll(route, 'analysis')

async function loadHistory() {
  historyLoading.value = true
  try { history.value = await listAnalysis(historyModule.value, 30) }
  catch (reason) { message.error((reason as Error).message) }
  finally { historyLoading.value = false }
}
watch(historyModule, () => void loadHistory())

function notifyResult(value: AnalysisView, payload?: AnalyzeRequest) {
  if (value.status === 'failed') handleFailure(value.error || '分析失败', value.error_code || '', payload)
  else if (value.status === 'degraded') message.warning('分析部分成功：结构化结果不完整，原文已保留')
  else message.success('分析完成')
}
const { polling, track, stop } = useResultPolling<AnalysisView>({
  load: getAnalysis,
  isDone: (value) => value.status !== 'processing',
  timeoutMs: 11 * 60 * 1000,
  onResult: (id, value) => {
    if (!current.value || current.value.id === id) current.value = value
    notifyResult(value)
  },
  onError: (error) => message.error(error.message),
  onSettled: async () => { await Promise.all([loadHistory(), refreshTask()]) },
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
} = useBusinessTask('analysis', currentID)

function analysisPayload(allowStale = false): AnalyzeRequest | null {
  const module = form.value.module
  if (module === 'stock' && !selectedStock.value?.symbol) {
    message.warning('请先搜索并选择目标股票')
    return null
  }
  const payload: AnalyzeRequest = {
    module,
    llm_config_id: form.value.llm_config_id,
    question: form.value.question?.trim() || undefined,
  }
  if (['stock', 'market', 'sector'].includes(module)) payload.market = module === 'stock' ? selectedStock.value!.market || 'cn' : form.value.market || 'cn'
  if (module === 'stock') payload.symbol = selectedStock.value!.symbol
  if (module === 'sector') payload.target = form.value.target?.trim() || undefined
  if (module === 'stock' && panelMode.value && !asOf.value) payload.mode = 'panel'
  else if (verifyMode.value) payload.verify = true
  if (module === 'stock' && !payload.mode && asOf.value) payload.as_of = asOf.value
  if (allowStale) payload.allow_stale = true
  return payload
}

function handleFailure(text: string, code = '', payload?: AnalyzeRequest) {
  const canExplainHistory = payload?.module === 'stock' && payload.mode !== 'panel' && !payload.as_of && !payload.allow_stale
  if (canExplainHistory && (code === 'stale_quote' || (!code && text.includes('历史数据解释')))) {
    dialog.warning({
      title: '行情已过期，不能给出当前评级',
      content: `${text}。可以由你明确选择按旧行情时点生成历史解释；它不会被展示成当前建议。`,
      positiveText: '按历史数据解释',
      negativeText: '取消',
      onPositiveClick: () => { void submitAnalysis({ ...payload, allow_stale: true }) },
    })
    return
  }
  message.error(text || '分析失败')
}

let submitLocked = false
async function submitAnalysis(payload: AnalyzeRequest) {
  if (submitLocked || running.value) return
  submitLocked = true
  submitting.value = true
  try {
    const created = await createAnalysis(payload)
    current.value = created
    await replaceRouteQuery(route, router, { record_id: created.id })
    await Promise.all([loadHistory(), refreshTask()])
    if (created.status === 'processing') {
      message.info('分析任务已创建；刷新或关闭页面不影响后台执行')
      void track(created.id)
    } else notifyResult(created, payload)
  } catch (reason) {
    handleFailure((reason as Error).message || '', getApiErrorCode(reason), payload)
  } finally {
    submitLocked = false
    submitting.value = false
  }
}
async function runAnalysis() {
  const payload = analysisPayload()
  if (payload) await submitAnalysis(payload)
}

function routeRecordID(): number | null {
  const raw = Array.isArray(route.query.record_id) ? route.query.record_id[0] : route.query.record_id
  const id = Number(raw)
  return Number.isSafeInteger(id) && id > 0 ? id : null
}
let routeSequence = 0
async function openRouteRecord(): Promise<boolean> {
  const id = routeRecordID()
  if (!id) return false
  if (current.value?.id === id) return true
  const sequence = ++routeSequence
  stop()
  try {
    const value = await getAnalysis(id)
    if (sequence !== routeSequence || routeRecordID() !== id) return true
    current.value = value
    if (value.status === 'processing') void track(id)
  } catch (reason) {
    if (sequence === routeSequence) message.error((reason as Error).message)
  }
  return true
}
watch(() => route.query.record_id, () => void openRouteRecord())
async function openRecord(item: AnalysisRecord) {
  try {
    stop()
    current.value = await getAnalysis(item.id)
    await replaceRouteQuery(route, router, { record_id: item.id })
    if (current.value.status === 'processing') void track(item.id)
  } catch (reason) { message.error((reason as Error).message) }
}
async function removeRecord(item: AnalysisRecord) {
  try {
    await deleteAnalysis(item.id)
    if (current.value?.id === item.id) {
      current.value = null
      await replaceRouteQuery(route, router, { record_id: undefined })
    }
    await loadHistory()
    message.success('本人分析记录已删除')
  } catch (reason) { message.error((reason as Error).message) }
}
async function cancelCurrentTask() {
  try {
    await cancelTask()
    if (current.value) current.value = await getAnalysis(current.value.id).catch(() => current.value)
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
    const value = await getAnalysis(rerun.result_id)
    current.value = value
    await replaceRouteQuery(route, router, { record_id: value.id })
    await loadHistory()
    if (value.status === 'processing') void track(value.id)
  } catch (reason) { message.error((reason as Error).message) }
}
function openTaskAudit() {
  void router.push({ name: 'tasks', query: task.value ? { job_id: String(task.value.source_id) } : { source: 'job', kind: 'analysis' } })
}

function applyStockActionQuery() {
  if (route.query.module) form.value.module = String(route.query.module) as AnalysisModule
  if (route.query.symbol) {
    updateSelectedStock({
      symbol: String(route.query.symbol),
      market: String(route.query.market || 'cn'),
      name: String(route.query.name || ''),
    })
  } else if (route.query.market) form.value.market = String(route.query.market)
  if (route.query.review_context === 'recommendation' && route.query.recommendation_id) {
    const context = String(route.query.recommendation_context || '').trim()
    form.value.question = `请独立复核推荐记录 #${String(route.query.recommendation_id)} 的主要结论和风险。${context ? `原推荐摘要：${context}` : ''}`.slice(0, 500)
    verifyMode.value = true
    panelMode.value = false
  }
}
watch(() => route.query._stock_action, applyStockActionQuery)

onMounted(async () => {
  applyStockActionQuery()
  await Promise.all([loadLLM(), loadHistory(), loadPreference()])
  if (await openRouteRecord()) {
    await restoreScroll()
    return
  }
  const processing = history.value.find((item) => item.status === 'processing')
  if (processing) {
    current.value = await getAnalysis(processing.id).catch(() => null)
    if (current.value) {
      await replaceRouteQuery(route, router, { record_id: processing.id })
      void track(processing.id)
    }
  }
  await restoreScroll()
})
</script>

<template>
  <PageContainer title="AI 分析工作台" subtitle="明确发起、先看结论、再核证据，历史与回溯分开">
    <template #actions><DisplayModeSwitch /></template>
    <div class="workspace">
      <div class="main-column">
        <AnalysisResultWorkspace :current="current" :loading="running" />
        <AnalysisHistory
          :history="history"
          :current-i-d="current?.id"
          :loading="historyLoading"
          :module="historyModule"
          :module-options="historyFilterOptions"
          @update:module="historyModule = $event as HistoryModule"
          @open="openRecord"
          @remove="removeRecord"
          @refresh="loadHistory"
        />
      </div>
      <aside class="side-column">
        <AnalysisLauncher
          v-model:form="form"
          v-model:selected-stock="selectedStock"
          v-model:panel-mode="panelMode"
          v-model:verify-mode="verifyMode"
          v-model:as-of-ts="asOfTs"
          :module-options="moduleOptions"
          :market-options="marketOptions"
          :llm-options="llmOptions"
          :llm-loading="llmLoading"
          :running="running"
          :risk-label="riskLabel"
          :as-of="asOf"
          @stock-change="updateSelectedStock"
          @analyze="runAnalysis"
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
.side-column { display: grid; min-width: 0; gap: 16px; }
.side-column { position: sticky; top: 76px; }
@media (max-width: 1050px) {
  .workspace { grid-template-columns: 1fr; }
  .side-column { position: static; grid-row: 1; }
}
</style>
