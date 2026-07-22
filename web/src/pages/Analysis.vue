<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NInputNumber,
  NSelect,
  NForm,
  NFormItem,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NAlert,
  NSpace,
  NSwitch,
  NModal,
  NTooltip,
  NDropdown,
  NDatePicker,
  useMessage,
  useDialog,
} from 'naive-ui'
import {
  createAnalysis,
  listAnalysis,
  getAnalysis,
  deleteAnalysis,
  getAnalysisDiff,
  getAnalysisHindsight,
  type AnalyzeRequest,
  type AnalysisModule,
  type AnalysisView,
  type AnalysisRecord,
  type AnalysisRating,
  type AnalysisDiff,
  type PanelRoleKind,
  type HindsightView,
} from '@/api/analysis'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getApiErrorCode } from '@/api/client'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { pollUntil, isPollCancelled } from '@/lib/poll'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'
import type { RiskFlag } from '@/api/trust'

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha, pctColor } = useUi()
const { llmLabel } = useLlmLabel()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const moduleOptions: { label: string; value: AnalysisModule }[] = [
  { label: '个股分析', value: 'stock' },
  { label: '全市场分析', value: 'market' },
  { label: '板块分析', value: 'sector' },
  { label: '自选股分析', value: 'watchlist' },
  { label: '持仓分析', value: 'position' },
]
const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

// ---------- 表单 ----------
const form = ref<AnalyzeRequest>({
  module: 'stock',
  market: 'cn',
  symbol: '',
  target: '',
  llm_config_id: undefined,
  question: '',
})
const needSymbol = computed(() => form.value.module === 'stock')
const needMarket = computed(() =>
  form.value.module === 'stock' || form.value.module === 'market' || form.value.module === 'sector',
)
const needTarget = computed(() => form.value.module === 'sector')
// 多角色观点（仅个股模块）：一次调用输出技术面/动量/风控/反方四个立场的独立结论。
const panelMode = ref(false)
// AI 复核：额外一次独立复核员调用逐项挑刺（panel/降级不复核）。
const verifyMode = ref(false)
// M2 回溯诊断日期（时间戳；null=实时分析）。仅个股标准模式；选了回溯日期则 panel 不可用。
const asOfTs = ref<number | null>(null)
const asOfStr = computed(() => {
  if (!asOfTs.value) return ''
  const d = new Date(asOfTs.value)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
})
// 回溯日期只能选过去（当天与未来禁用；后端同样校验）。
function asOfDisabled(ts: number) {
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  return ts >= today.getTime()
}

// ---------- LLM 配置 ----------
const llmConfigs = ref<LLMConfig[]>([])
const llmLoading = ref(false)
const llmOptions = computed(() =>
  llmConfigs.value.map((c) => ({
    label: c.is_default ? `${c.name}（默认）` : c.name,
    value: c.id,
  })),
)
async function loadLLM() {
  llmLoading.value = true
  try {
    llmConfigs.value = await listLLMConfigs()
    const def = llmConfigs.value.find((c) => c.is_default) || llmConfigs.value[0]
    if (def && form.value.llm_config_id === undefined) form.value.llm_config_id = def.id
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    llmLoading.value = false
  }
}

// ---------- 发起分析 ----------
const running = ref(false)
const current = ref<AnalysisView | null>(null)

function analysisPayload(allowStale = false): AnalyzeRequest | null {
  if (form.value.module === 'stock' && !form.value.symbol?.trim()) {
    message.warning('请输入股票代码')
    return null
  }
  const payload: AnalyzeRequest = {
    module: form.value.module,
    llm_config_id: form.value.llm_config_id,
    question: form.value.question?.trim() || undefined,
  }
  if (needMarket.value) payload.market = form.value.market
  if (needSymbol.value) payload.symbol = form.value.symbol?.trim()
  if (needTarget.value) payload.target = form.value.target?.trim() || undefined
  if (form.value.module === 'stock' && panelMode.value && !asOfStr.value) payload.mode = 'panel'
  else if (verifyMode.value) payload.verify = true // 复核仅对标准模式生效（panel 无标准结论字段）
  if (form.value.module === 'stock' && !payload.mode && asOfStr.value) payload.as_of = asOfStr.value
  if (allowStale) payload.allow_stale = true // 用户已确认：行情过期，按截至时刻的历史数据解释
  return payload
}

function handleAnalysisFailure(msg: string, refusalCode = '', payload?: AnalyzeRequest) {
  // 行情时效 fail-closed：异步终态同样保留 error_code，完成轮询后继续原来的显式确认交互。
  const canRetryAsHistorical =
    payload?.module === 'stock' && payload.mode !== 'panel' && !payload.as_of && !payload.allow_stale
  if (canRetryAsHistorical && (refusalCode === 'stale_quote' || (!refusalCode && msg.includes('历史数据解释')))) {
    dialog.warning({
      title: '行情已过期，无法给出当前评级',
      content:
        msg +
        '。是否按「截至行情时刻的历史数据解释」模式继续？该模式不代表当前盘面判断，也不会给出当前买卖行动建议。',
      positiveText: '按历史数据解释',
      negativeText: '取消',
      onPositiveClick: () => {
        void submitAnalysis({ ...payload, allow_stale: true })
      },
    })
    return
  }
  message.error(msg || '分析失败')
}

function notifyAnalysisResult(view: AnalysisView, payload?: AnalyzeRequest) {
  if (view.status === 'failed') {
    handleAnalysisFailure(view.error || '分析失败', view.error_code || '', payload)
  } else if (view.status === 'degraded') {
    message.warning('模型输出未通过结构化校验，已降级为原文展示')
  } else {
    message.success('分析完成')
  }
}

let pollAbort: AbortController | null = null
onBeforeUnmount(() => pollAbort?.abort())

async function trackAnalysis(id: number, payload?: AnalyzeRequest) {
  running.value = true
  pollAbort?.abort()
  const controller = new AbortController()
  pollAbort = controller
  try {
    const view = await pollUntil(() => getAnalysis(id), (v) => v.status !== 'processing', {
      signal: controller.signal,
      timeoutMs: 11 * 60 * 1000,
    })
    if (!current.value || current.value.id === id) current.value = view
    notifyAnalysisResult(view, payload)
  } catch (e) {
    if (isPollCancelled(e)) return
    message.error((e as Error).message)
  } finally {
    if (pollAbort === controller) {
      pollAbort = null
      running.value = false
      await loadHistory()
    }
  }
}

async function submitAnalysis(payload: AnalyzeRequest) {
  running.value = true
  try {
    const view = await createAnalysis(payload)
    current.value = view
    diff.value = null
    if (view.status === 'processing') {
      message.info('任务已创建，正在后台分析（刷新或关闭页面不影响任务）')
      await loadHistory()
      await trackAnalysis(view.id, payload)
      return
    }
    notifyAnalysisResult(view, payload)
  } catch (e) {
    const msg = (e as Error).message || ''
    handleAnalysisFailure(msg, getApiErrorCode(e), payload)
  } finally {
    running.value = false
    await loadHistory()
  }
}

async function runAnalysis(allowStale = false) {
  const payload = analysisPayload(allowStale)
  if (payload) await submitAnalysis(payload)
}

// ---------- 历史 ----------
const history = ref<AnalysisRecord[]>([])
const historyLoading = ref(false)
// 历史模块筛选（PRD 3.14：按模块筛选历史，后端 History 已支持）。
const historyModule = ref<string>('all')
const historyFilterOptions: { label: string; value: string }[] = [
  { label: '全部模块', value: 'all' },
  ...moduleOptions,
]
async function loadHistory() {
  historyLoading.value = true
  try {
    history.value = await listAnalysis(historyModule.value, 30)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    historyLoading.value = false
  }
}
watch(historyModule, () => loadHistory())
async function openRecord(rec: AnalysisRecord) {
  try {
    current.value = await getAnalysis(rec.id)
    diff.value = null
    if (current.value.status === 'processing') void trackAnalysis(rec.id)
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeRecord(rec: AnalysisRecord) {
  try {
    await deleteAnalysis(rec.id)
    if (current.value?.id === rec.id) current.value = null
    await loadHistory()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 展示辅助 ----------
const ratingMeta: Record<AnalysisRating, { text: string; color: () => string }> = {
  bullish: { text: '偏多', color: () => upColor.value },
  bearish: { text: '偏空', color: () => downColor.value },
  neutral: { text: '中性', color: () => flatColor.value },
}
function ratingText(r: string) {
  return ratingMeta[r as AnalysisRating]?.text || '—'
}
// 风险闸门标签（S1）。
function riskTagType(f: RiskFlag): 'error' | 'warning' | 'default' {
  if (f.level === 'block') return 'error'
  if (f.level === 'warn') return 'warning'
  return 'default'
}
function riskTagLabel(f: RiskFlag): string {
  const names: Record<string, string> = {
    st: 'ST/风险警示',
    delist: '退市风险',
    limit_board: '一字板',
    low_liquidity: '流动性不足',
    small_cap: '小市值',
  }
  return names[f.code] || f.code
}

function ratingColor(r: string) {
  return ratingMeta[r as AnalysisRating]?.color() || flatColor.value
}
function moduleText(m: string) {
  return moduleOptions.find((o) => o.value === m)?.label || m
}
function statusText(s: string) {
  return s === 'processing' ? '分析中' : s === 'success' ? '成功' : s === 'degraded' ? '降级' : '失败'
}
function fmtTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}
const panelRoleMeta: Record<PanelRoleKind, { label: string; desc: string }> = {
  technical: { label: '技术面研究员', desc: '趋势 · 均线 · 支撑压力' },
  momentum: { label: '动量交易者', desc: '短期动能 · 量比换手' },
  risk: { label: '风控经理', desc: '波动回撤 · 宁可错过' },
  contrarian: { label: '反方唱空者', desc: '刻意反驳主流叙事' },
}
function panelRoleLabel(r: string) {
  return panelRoleMeta[r as PanelRoleKind]?.label || r
}
function panelRoleDesc(r: string) {
  return panelRoleMeta[r as PanelRoleKind]?.desc || ''
}

// AI 复核结论 → n-alert 类型。
function reviewAlertType(v: string): 'success' | 'warning' | 'error' {
  if (v === 'pass') return 'success'
  if (v === 'reject') return 'error'
  return 'warning'
}

// ---------- 数据快照透明面板（Get 已回传 data_snapshot） ----------
const snapshotShow = ref(false)
const snapshotText = computed(() => {
  const raw = current.value?.data_snapshot
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

// ---------- 变化检测（与上次对比） ----------
const diff = ref<AnalysisDiff | null>(null)
const diffShow = ref(false)
const diffLoading = ref(false)
async function openDiff() {
  if (!current.value) return
  diffLoading.value = true
  try {
    diff.value = await getAnalysisDiff(current.value.id)
    diffShow.value = true
  } catch (e) {
    message.warning((e as Error).message)
  } finally {
    diffLoading.value = false
  }
}
const canDiff = computed(
  () => current.value?.status === 'success' && current.value.mode !== 'panel',
)

// ---------- 继续问答（复用本次分析的数据快照） ----------
const canAskQa = computed(
  () =>
    current.value?.module === 'stock' &&
    !!current.value.symbol &&
    (current.value.status === 'success' || current.value.status === 'degraded'),
)
function askFromAnalysis() {
  if (!current.value) return
  router.push({
    name: 'qa',
    query: {
      from_analysis: String(current.value.id),
      symbol: current.value.symbol,
      market: current.value.market || 'cn',
      name: current.value.target || current.value.symbol,
    },
  })
}
// 按最新数据提问：只带标的深链，Qa 页首问按最新行情采集新快照（不沿用分析快照）。
function askFromAnalysisFresh() {
  if (!current.value) return
  router.push({
    name: 'qa',
    query: {
      symbol: current.value.symbol,
      market: current.value.market || 'cn',
      name: current.value.target || current.value.symbol,
    },
  })
}
const askQaOptions = [
  { label: '沿用本次分析快照', key: 'snapshot' },
  { label: '按最新数据提问', key: 'fresh' },
]
function onAskQaSelect(key: string) {
  if (key === 'fresh') askFromAnalysisFresh()
  else askFromAnalysis()
}

// ---------- 回溯校验（M2 hindsight：当时怎么看 → 后来怎么走） ----------
const hindsightShow = ref(false)
const hindsightLoading = ref(false)
const hindsight = ref<HindsightView | null>(null)
const hsTargetPrice = ref<number | null>(null)
const hsStopPrice = ref<number | null>(null)
const canHindsight = computed(
  () =>
    current.value?.status !== 'processing' &&
    current.value?.module === 'stock' &&
    (current.value.market || 'cn') === 'cn' &&
    !!current.value.symbol,
)
async function openHindsight(refresh = false) {
  if (!current.value) return
  hindsightLoading.value = true
  if (!refresh) {
    hindsight.value = null
    hsTargetPrice.value = null
    hsStopPrice.value = null
    hindsightShow.value = true
  }
  try {
    hindsight.value = await getAnalysisHindsight(
      current.value.id,
      hsTargetPrice.value ?? undefined,
      hsStopPrice.value ?? undefined,
    )
  } catch (e) {
    message.warning((e as Error).message)
    if (!refresh) hindsightShow.value = false
  } finally {
    hindsightLoading.value = false
  }
}
const hindsightNodes: { key: 'd5' | 'd10' | 'd20' | 'd60'; label: string }[] = [
  { key: 'd5', label: '+5 交易日' },
  { key: 'd10', label: '+10 交易日' },
  { key: 'd20', label: '+20 交易日' },
  { key: 'd60', label: '+60 交易日' },
]
function fmtSignedPct(v: number | undefined | null): string {
  if (v === undefined || v === null) return '—'
  return `${v > 0 ? '+' : ''}${v.toFixed(2)}%`
}

onMounted(async () => {
  // 从个股页/自选跳转带参：预填模块与标的。
  if (route.query.module) form.value.module = String(route.query.module) as AnalysisModule
  if (route.query.symbol) form.value.symbol = String(route.query.symbol)
  if (route.query.market) form.value.market = String(route.query.market)
  await Promise.all([loadLLM(), loadHistory()])
  const processing = history.value.find((h) => h.status === 'processing')
  if (processing) {
    current.value = await getAnalysis(processing.id).catch(() => null)
    void trackAnalysis(processing.id)
  }
})
</script>

<template>
  <PageContainer title="AI 分析" subtitle="按模块组织的研究参考 · 结构化输出 · 历史可复现">
    <div class="analysis" :style="styleVars">
      <!-- 左：发起分析 -->
      <div class="col-form">
        <SectionCard title="发起分析">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="分析模块">
              <n-select v-model:value="form.module" :options="moduleOptions" />
            </n-form-item>
            <n-form-item v-if="needMarket" label="市场">
              <n-select v-model:value="form.market" :options="marketOptions" />
            </n-form-item>
            <n-form-item v-if="needSymbol" label="股票代码">
              <n-input v-model:value="form.symbol" placeholder="如 600000" />
            </n-form-item>
            <n-form-item v-if="needTarget" label="关注板块（可选）">
              <n-input v-model:value="form.target" placeholder="如 半导体 / 银行" />
            </n-form-item>
            <n-form-item v-if="form.module === 'stock'" label="回溯日期（可选 · 历史诊断）">
              <n-date-picker
                v-model:value="asOfTs"
                type="date"
                clearable
                :is-date-disabled="asOfDisabled"
                placeholder="留空=实时分析"
                style="width: 100%"
              />
            </n-form-item>
            <div v-if="form.module === 'stock' && asOfStr" class="hint" style="margin-bottom: 8px">
              回溯模式：日线截断至 {{ asOfStr }} 组装数据（无未来泄露），估值/新闻/财务不可得会如实声明；
              完成后可用「回溯校验」对照真实走势。
            </div>
            <n-form-item v-if="form.module === 'stock' && !asOfStr" label="多角色观点">
              <n-switch v-model:value="panelMode" />
              <span class="switch-hint">技术面 / 动量 / 风控 / 反方四视角独立评判</span>
            </n-form-item>
            <n-form-item v-if="!(form.module === 'stock' && panelMode)" label="AI 复核（更可信，多一次调用）">
              <n-switch v-model:value="verifyMode" />
              <span class="switch-hint">独立复核员对照数据快照逐项挑刺，可否决并压低置信度</span>
            </n-form-item>
            <n-form-item label="LLM 配置">
              <n-select
                v-model:value="form.llm_config_id"
                :options="llmOptions"
                :loading="llmLoading"
                :placeholder="llmConfigs.length ? '选择模型配置' : '未配置将使用系统默认配置'"
              />
            </n-form-item>
            <n-form-item label="附加问题（可选）">
              <n-input
                v-model:value="form.question"
                type="textarea"
                :autosize="{ minRows: 2, maxRows: 4 }"
                placeholder="想让 AI 特别关注的问题（可选）"
                maxlength="500"
              />
            </n-form-item>
            <n-button
              type="primary"
              block
              :loading="running"
              :disabled="running"
              @click="runAnalysis()"
            >
              {{ running ? '分析中…' : '开始分析' }}
            </n-button>
            <div v-if="!llmLoading && !llmConfigs.length" class="hint">
              未配置 LLM，将使用系统默认配置（若管理员未开启回退，提交后会提示）。
            </div>
          </n-form>
        </SectionCard>

        <SectionCard title="历史记录">
          <template #extra>
            <n-select
              v-model:value="historyModule"
              size="tiny"
              :options="historyFilterOptions"
              style="width: 110px"
            />
            <n-button size="tiny" quaternary :loading="historyLoading" @click="loadHistory"
              >刷新</n-button
            >
          </template>
          <n-spin :show="historyLoading && !history.length">
            <n-empty v-if="!history.length" description="暂无分析记录" size="small" />
            <div v-else class="hist">
              <div
                v-for="h in history"
                :key="h.id"
                class="hist-item"
                :class="{ active: current?.id === h.id }"
                @click="openRecord(h)"
              >
                <div class="hist-main">
                  <div class="hist-title">
                    <n-tag size="tiny" round :bordered="false">{{ moduleText(h.module) }}</n-tag>
                    <n-tag v-if="h.mode === 'panel'" size="tiny" round :bordered="false" type="info">多角色</n-tag>
                    <n-tag v-if="h.as_of" size="tiny" round :bordered="false" type="warning">回溯</n-tag>
                    <span class="hist-target">{{ h.target || h.title }}</span>
                  </div>
                  <div class="hist-sub">{{ fmtTime(h.created_at) }}</div>
                </div>
                <div class="hist-side">
                  <span
                    v-if="h.status === 'success'"
                    class="hist-rating"
                    :style="{ color: ratingColor(h.rating) }"
                    >{{ ratingText(h.rating) }}</span
                  >
                  <n-tag
                    v-else
                    size="tiny"
                    :type="h.status === 'failed' ? 'error' : h.status === 'processing' ? 'info' : 'warning'"
                    :bordered="false"
                  >
                    {{ statusText(h.status) }}
                  </n-tag>
                  <n-popconfirm v-if="h.status !== 'processing'" @positive-click="removeRecord(h)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error" @click.stop>删</n-button>
                    </template>
                    删除这条分析记录？
                  </n-popconfirm>
                </div>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>

      <!-- 右：结果 -->
      <div class="col-result">
        <SectionCard title="分析结果">
          <n-spin :show="running">
            <n-empty
              v-if="!current"
              description="选择模块并发起分析，或点击左侧历史记录查看"
              style="padding: 40px 0"
            />
            <div v-else class="result">
              <!-- 头部：模块 + 评级 + 置信度 + 操作 -->
              <div class="res-head">
                <div class="res-title">
                  <n-tag size="small" round :bordered="false">{{ moduleText(current.module) }}</n-tag>
                  <n-tag v-if="current.mode === 'panel'" size="small" round :bordered="false" type="info">多角色</n-tag>
                  <n-tag v-if="current.as_of" size="small" round :bordered="false" type="warning">回溯@{{ current.as_of }}</n-tag>
                  <n-tag v-if="current.stale_mode" size="small" round :bordered="false" type="warning">历史解释</n-tag>
                  <span class="res-target">{{ current.target || current.title }}</span>
                </div>
                <div class="res-side">
                  <n-button v-if="canDiff" size="tiny" quaternary :loading="diffLoading" @click="openDiff"
                    >与上次对比</n-button
                  >
                  <n-button v-if="canHindsight" size="tiny" quaternary @click="openHindsight()">回溯校验</n-button>
                  <n-dropdown v-if="canAskQa" trigger="click" :options="askQaOptions" @select="onAskQaSelect">
                    <n-button size="tiny" quaternary>继续问答</n-button>
                  </n-dropdown>
                  <div v-if="current.status === 'success' && current.result" class="res-rating">
                    <span
                      class="rating-badge"
                      :style="{
                        color: ratingColor(current.result.rating),
                        background: withAlpha(ratingColor(current.result.rating), 0.12),
                      }"
                      >{{ current.stale_mode ? '历史解读·' : '' }}{{ ratingText(current.result.rating) }}</span
                    >
                    <!-- 「AI 自评」而非「置信度」：与综合置信（程序合成）区分主次，防误读为胜率。 -->
                    <span class="confidence">AI 自评 {{ current.result.confidence }}</span>
                  </div>
                  <div v-else-if="current.status === 'success' && current.panel" class="res-rating">
                    <span
                      class="rating-badge"
                      :style="{
                        color: ratingColor(current.rating),
                        background: withAlpha(ratingColor(current.rating), 0.12),
                      }"
                      >多数 {{ ratingText(current.rating) }}</span
                    >
                  </div>
                </div>
              </div>

              <n-alert
                v-if="current.status === 'processing'"
                type="info"
                :bordered="false"
                style="margin-bottom: 12px"
              >
                正在后台采集数据并生成分析。关闭或刷新页面不会中断任务，完成后会自动展示。
              </n-alert>

              <!-- 历史解释模式横幅：结果不是当前盘面判断，禁按普通评级消费 -->
              <n-alert v-if="current.stale_mode" type="warning" :bordered="false" class="stale-mode-banner">
                {{
                  current.stale_mode_note ||
                  '行情已过期，本次为截至行情时刻的历史数据解释：评级与结论只反映对该时点数据的解读，不代表当前盘面判断，不含当前买卖参考。'
                }}
              </n-alert>

              <!-- 风险闸门标签（S1：快照 risk_gate 程序化判定，与注入 prompt 同源） -->
              <div v-if="current.risk_flags?.length" class="risk-flags">
                <n-tooltip v-for="f in current.risk_flags" :key="f.code" trigger="hover" style="max-width: 340px">
                  <template #trigger>
                    <n-tag size="small" :type="riskTagType(f)" :bordered="false">
                      {{ f.level === 'block' ? '⛔' : f.level === 'warn' ? '⚠' : 'ⓘ' }} {{ riskTagLabel(f) }}
                    </n-tag>
                  </template>
                  {{ f.text }}
                </n-tooltip>
              </div>

              <!-- 信任徽章：证据核验 · 综合置信 · AI 复核（仅标准成功结果） -->
              <TrustBadges
                v-if="current.status === 'success' && current.result"
                class="trust-mb"
                :evidence-check="current.result.evidence_check"
                :sys-confidence="current.result.sys_confidence"
                :sys-confidence-why="current.result.sys_confidence_why"
                :review="current.result.review"
              />

              <!-- 降级/失败提示 -->
              <n-alert
                v-if="current.status === 'degraded'"
                type="warning"
                :bordered="false"
                style="margin-bottom: 12px"
              >
                模型输出未通过结构化校验，以下为原文展示。
              </n-alert>
              <n-alert
                v-else-if="current.status === 'failed'"
                type="error"
                :bordered="false"
                style="margin-bottom: 12px"
              >
                {{ current.error || '分析失败' }}
              </n-alert>

              <!-- 结构化结果 -->
              <template v-if="current.status === 'success' && current.result">
                <n-alert
                  v-if="current.result.review"
                  :type="reviewAlertType(current.result.review.verdict)"
                  :bordered="false"
                  style="margin-bottom: 12px"
                >
                  AI 复核：{{ current.result.review.comment || '（无补充说明）' }}
                </n-alert>
                <p class="summary">{{ current.result.summary }}</p>

                <div v-if="current.result.highlights.length" class="block">
                  <div class="block-title">关键要点</div>
                  <ul>
                    <li v-for="(x, i) in current.result.highlights" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div v-if="current.result.opportunities.length" class="block">
                  <div class="block-title" :style="{ color: upColor }">机会</div>
                  <ul>
                    <li v-for="(x, i) in current.result.opportunities" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div v-if="current.result.risks.length" class="block">
                  <div class="block-title" :style="{ color: downColor }">风险</div>
                  <ul>
                    <li v-for="(x, i) in current.result.risks" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div v-if="current.result.suggestions.length" class="block">
                  <div class="block-title">关注点 / 研究方向</div>
                  <ul>
                    <li v-for="(x, i) in current.result.suggestions" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div
                  v-if="current.result.anti_thesis?.length"
                  class="block anti-block"
                  :style="{
                    background: withAlpha(vars.warningColor, 0.08),
                    borderColor: withAlpha(vars.warningColor, 0.35),
                  }"
                >
                  <div class="block-title" :style="{ color: vars.warningColor }">反方观点 · 为什么可能是错的</div>
                  <ul>
                    <li v-for="(x, i) in current.result.anti_thesis" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div v-if="current.result.kill_switches?.length" class="block">
                  <div class="block-title">结论失效条件</div>
                  <ul>
                    <li v-for="(x, i) in current.result.kill_switches" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <div v-if="current.result.unknowns?.length" class="block">
                  <div class="block-title">数据盲区</div>
                  <ul>
                    <li v-for="(x, i) in current.result.unknowns" :key="i">{{ x }}</li>
                  </ul>
                </div>
                <!-- M3c 交易员阶段：交易计划 + 量化仓位 -->
                <div
                  v-if="current.result.trade_plan && !current.result.trade_plan.no_plan"
                  class="block plan-block"
                  :style="{
                    background: withAlpha(vars.primaryColor, 0.06),
                    borderColor: withAlpha(vars.primaryColor, 0.3),
                  }"
                >
                  <div class="block-title" :style="{ color: vars.primaryColor }">交易计划（研究参考，非操作指令）</div>
                  <div class="plan-grid">
                    <div class="plan-cell">
                      <div class="pc-label">买入区间</div>
                      <div class="pc-value">{{ current.result.trade_plan.buy_low }} ~ {{ current.result.trade_plan.buy_high }}</div>
                    </div>
                    <div class="plan-cell">
                      <div class="pc-label">目标价</div>
                      <div class="pc-value" :style="{ color: upColor }">{{ current.result.trade_plan.target_price }}</div>
                    </div>
                    <div class="plan-cell">
                      <div class="pc-label">止损价</div>
                      <div class="pc-value" :style="{ color: downColor }">{{ current.result.trade_plan.stop_price }}</div>
                    </div>
                    <div class="plan-cell">
                      <div class="pc-label">持有周期</div>
                      <div class="pc-value">{{ current.result.trade_plan.horizon_days }} 交易日</div>
                    </div>
                    <div class="plan-cell">
                      <div class="pc-label">盈亏比</div>
                      <div class="pc-value">{{ current.result.trade_plan.rr_ratio }}</div>
                    </div>
                    <div v-if="current.result.trade_plan.position" class="plan-cell">
                      <div class="pc-label">
                        建议仓位
                        <n-tooltip trigger="hover" placement="top" style="max-width: 340px">
                          <template #trigger>
                            <span class="pc-help">?</span>
                          </template>
                          仓位% = 100 × clip(2.5/20日波动率, 0.3, 1.0) × 择时系数。 20日波动率
                          {{ current.result.trade_plan.position.vol_20d }}% → 波动系数
                          {{ current.result.trade_plan.position.vol_coef }}；择时系数
                          {{ current.result.trade_plan.position.timing_coef
                          }}<template v-if="current.result.trade_plan.position.advance_ratio">
                            （涨家占比
                            {{ Math.round(current.result.trade_plan.position.advance_ratio * 100) }}%）</template
                          >。<template v-if="current.result.trade_plan.discipline_notes?.length"
                            >盈亏比不足 2:1，展示值已按纪律减半。</template
                          >{{ current.result.trade_plan.position.note }}
                        </n-tooltip>
                      </div>
                      <div class="pc-value">
                        <template v-if="current.result.trade_plan.position.position_pct > 0"
                          >{{ current.result.trade_plan.position.position_pct }}%</template
                        >
                        <template v-else>—</template>
                      </div>
                    </div>
                  </div>
                  <p v-if="current.result.trade_plan.plan_note" class="plan-note">
                    {{ current.result.trade_plan.plan_note }}
                  </p>
                  <div
                    v-for="(d, i) in current.result.trade_plan.discipline_notes || []"
                    :key="i"
                    class="plan-discipline"
                    :style="{ color: vars.warningColor }"
                  >
                    ⚠ {{ d }}
                  </div>
                  <div v-if="current.result.trade_plan.checklist?.length" class="plan-checklist">
                    <div class="pc-label">操作清单（买入前逐项核对）</div>
                    <ul>
                      <li v-for="(c, i) in current.result.trade_plan.checklist" :key="i">{{ c }}</li>
                    </ul>
                  </div>
                </div>
                <div v-else-if="current.result.trade_plan?.no_plan" class="block">
                  <div class="block-title">交易计划</div>
                  <p class="panel-text">未生成：{{ current.result.trade_plan.no_plan_reason }}</p>
                </div>
                <p class="disclaimer">{{ current.result.disclaimer }}</p>
              </template>

              <!-- 多角色观点 -->
              <template v-else-if="current.status === 'success' && current.panel">
                <div class="panel-roles">
                  <div v-for="r in current.panel.roles" :key="r.role" class="panel-role">
                    <div class="pr-head">
                      <span class="pr-name">{{ panelRoleLabel(r.role) }}</span>
                      <span
                        class="pr-rating"
                        :style="{ color: ratingColor(r.rating), background: withAlpha(ratingColor(r.rating), 0.12) }"
                        >{{ ratingText(r.rating) }}</span
                      >
                    </div>
                    <div class="pr-desc">{{ panelRoleDesc(r.role) }}</div>
                    <div class="pr-summary">{{ r.summary }}</div>
                  </div>
                </div>
                <div class="block" style="margin-top: 14px">
                  <div class="block-title">共识</div>
                  <p class="panel-text">{{ current.panel.consensus }}</p>
                </div>
                <div
                  v-if="current.panel.disagreement"
                  class="block anti-block"
                  :style="{
                    background: withAlpha(vars.warningColor, 0.08),
                    borderColor: withAlpha(vars.warningColor, 0.35),
                  }"
                >
                  <div class="block-title" :style="{ color: vars.warningColor }">主要分歧</div>
                  <p class="panel-text">{{ current.panel.disagreement }}</p>
                </div>
              </template>

              <!-- 降级原文 -->
              <pre v-else-if="current.status === 'degraded'" class="raw">{{ current.raw }}</pre>

              <!-- 元信息 -->
              <div class="meta">
                <n-space size="small" :wrap="true" align="center">
                  <span>模型 {{ llmLabel(current) || current.model || '—' }}</span>
                  <span v-if="current.total_tokens">· token {{ current.total_tokens }}</span>
                  <span v-if="current.latency_ms">· 耗时 {{ (current.latency_ms / 1000).toFixed(1) }}s</span>
                  <span>· 版本 {{ current.prompt_version }}/{{ current.strategy_version }}</span>
                  <span>· {{ fmtTime(current.created_at) }}</span>
                  <n-button v-if="snapshotText" size="tiny" quaternary @click="snapshotShow = true">数据快照</n-button>
                </n-space>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>

    <!-- 与上次对比：差异卡 -->
    <n-modal v-model:show="diffShow" preset="card" title="与上次分析对比" style="max-width: 640px">
      <div v-if="diff" class="diff" :style="styleVars">
        <div class="diff-row">
          <span class="diff-label">上次分析</span>
          <span>{{ diff.prev_title }} · {{ fmtTime(diff.prev_at) }}</span>
        </div>
        <div class="diff-row">
          <span class="diff-label">评级变化</span>
          <span class="diff-rating">
            <span :style="{ color: ratingColor(diff.rating_from) }">{{ ratingText(diff.rating_from) }}</span>
            <span class="diff-arrow">→</span>
            <span :style="{ color: ratingColor(diff.rating_to), fontWeight: 600 }">{{ ratingText(diff.rating_to) }}</span>
            <n-tag v-if="diff.rating_from !== diff.rating_to" size="tiny" type="warning" :bordered="false" round
              >评级转向</n-tag
            >
          </span>
        </div>
        <div class="diff-row">
          <span class="diff-label">置信度</span>
          <span>
            {{ diff.confidence_from }} → {{ diff.confidence_to }}
            <span
              v-if="diff.confidence_delta !== 0"
              :style="{ color: diff.confidence_delta > 0 ? upColor : downColor }"
            >
              （{{ diff.confidence_delta > 0 ? '+' : '' }}{{ diff.confidence_delta }}）
            </span>
          </span>
        </div>
        <div class="diff-block">
          <div class="diff-label">上次结论</div>
          <p class="diff-summary">{{ diff.summary_prev }}</p>
          <div class="diff-label">本次结论</div>
          <p class="diff-summary now">{{ diff.summary_now }}</p>
        </div>
        <div v-if="diff.highlights_added.length" class="diff-block">
          <div class="diff-label" :style="{ color: upColor }">新增要点</div>
          <ul>
            <li v-for="(x, i) in diff.highlights_added" :key="i">{{ x }}</li>
          </ul>
        </div>
        <div v-if="diff.highlights_removed.length" class="diff-block">
          <div class="diff-label" :style="{ color: flatColor }">不再提及的要点</div>
          <ul>
            <li v-for="(x, i) in diff.highlights_removed" :key="i">{{ x }}</li>
          </ul>
        </div>
        <div v-if="diff.risks_added.length" class="diff-block">
          <div class="diff-label" :style="{ color: downColor }">新增风险</div>
          <ul>
            <li v-for="(x, i) in diff.risks_added" :key="i">{{ x }}</li>
          </ul>
        </div>
        <div v-if="diff.risks_removed.length" class="diff-block">
          <div class="diff-label" :style="{ color: flatColor }">解除的风险</div>
          <ul>
            <li v-for="(x, i) in diff.risks_removed" :key="i">{{ x }}</li>
          </ul>
        </div>
      </div>
    </n-modal>

    <!-- 数据快照：本次分析所依据的结构化数据（凭它复现结论） -->
    <n-modal v-model:show="snapshotShow" preset="card" title="数据快照" style="max-width: 720px">
      <pre class="snapshot-pre">{{ snapshotText }}</pre>
    </n-modal>

    <!-- 回溯校验（M2 hindsight）：当时怎么看 → 后来怎么走 -->
    <n-modal v-model:show="hindsightShow" preset="card" title="回溯校验 · 事后走势对照" style="max-width: 620px">
      <n-spin :show="hindsightLoading">
        <n-empty v-if="!hindsight" description="加载中…" />
        <div v-else class="hs">
          <div class="hs-meta">
            <span class="hs-name">{{ hindsight.name || hindsight.symbol }}</span>
            <span class="dim qv-tnum">{{ hindsight.symbol }}</span>
            <n-tag size="small" :bordered="false" type="warning">基准日 {{ hindsight.base_date }}</n-tag>
            <span class="dim">基准价 {{ hindsight.base_price.toFixed(2) }}（当日收盘）</span>
          </div>
          <div class="hs-grid">
            <div v-for="n in hindsightNodes" :key="n.key" class="hs-node">
              <div class="hs-node-label">{{ n.label }}</div>
              <div
                class="hs-node-value qv-figure"
                :style="{ color: hindsight.returns[n.key] ? pctColor(hindsight.returns[n.key]!.return_pct) : undefined }"
              >
                {{ hindsight.returns[n.key] ? fmtSignedPct(hindsight.returns[n.key]!.return_pct) : '未到期' }}
              </div>
              <div v-if="hindsight.returns[n.key]" class="hs-node-sub dim qv-tnum">{{ hindsight.returns[n.key]!.date }}</div>
            </div>
          </div>
          <div class="hs-line">
            区间最大上涨
            <span class="qv-tnum" :style="{ color: pctColor(hindsight.max_gain_pct) }">{{ fmtSignedPct(hindsight.max_gain_pct) }}</span>
            · 最大下跌
            <span class="qv-tnum" :style="{ color: pctColor(-hindsight.max_drawdown_pct) }">-{{ hindsight.max_drawdown_pct.toFixed(2) }}%</span>
            <template v-if="hindsight.alpha_pct !== undefined">
              · 同期上证 {{ fmtSignedPct(hindsight.bench_return_pct) }} · 超额(α)
              <span class="qv-tnum" :style="{ color: pctColor(hindsight.alpha_pct) }">{{ fmtSignedPct(hindsight.alpha_pct) }}</span>
            </template>
          </div>
          <div v-if="hindsight.rating" class="hs-line">
            当时评级 <n-tag size="small" :bordered="false">{{ ratingText(hindsight.rating) }}</n-tag>
            <template v-if="hindsight.rating_hit !== undefined && hindsight.rating_hit !== null">
              → 按 +20 交易日方向
              <n-tag size="small" :bordered="false" :type="hindsight.rating_hit ? 'success' : 'error'">
                {{ hindsight.rating_hit ? '命中' : '未命中' }}
              </n-tag>
            </template>
            <span v-else class="dim">（中性评级或未到 +20 日，不判方向命中）</span>
          </div>
          <div class="hs-touch">
            <div class="hs-touch-form">
              <n-input-number v-model:value="hsTargetPrice" size="small" :min="0" placeholder="目标价" style="width: 130px" />
              <n-input-number v-model:value="hsStopPrice" size="small" :min="0" placeholder="止损价" style="width: 130px" />
              <n-button size="small" :loading="hindsightLoading" @click="openHindsight(true)">验证价位首触</n-button>
            </div>
            <div v-if="hindsight.target_touch" class="hs-line">
              目标价 {{ hindsight.target_touch.price }} 于 {{ hindsight.target_touch.date }}（第
              {{ hindsight.target_touch.day_index }} 个交易日）盘中上穿
            </div>
            <div v-else-if="hsTargetPrice" class="hs-line dim">目标价 {{ hsTargetPrice }} 窗口内未触及</div>
            <div v-if="hindsight.stop_touch" class="hs-line">
              止损价 {{ hindsight.stop_touch.price }} 于 {{ hindsight.stop_touch.date }}（第
              {{ hindsight.stop_touch.day_index }} 个交易日）盘中下破
            </div>
            <div v-else-if="hsStopPrice" class="hs-line dim">止损价 {{ hsStopPrice }} 窗口内未触及</div>
          </div>
          <div class="hs-note dim">{{ hindsight.note }}</div>
        </div>
      </n-spin>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.analysis {
  display: grid;
  grid-template-columns: 360px 1fr;
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .analysis {
    grid-template-columns: 1fr;
  }
}
.col-form,
.col-result {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-width: 0;
}
/* 整页滚动下左栏固定：长结果滚动时发起表单与历史始终可见，历史在栏内滚动 */
.col-form {
  position: sticky;
  top: 76px;
  max-height: calc(100vh - 100px);
  overflow-y: auto;
  padding: 4px;
  margin: -4px;
}
@media (max-width: 900px) {
  .col-form {
    position: static;
    max-height: none;
    overflow: visible;
  }
  /* 折叠单列后历史列表（最多 30 条）横亘在表单与结果之间，限高栏内滚动 */
  .hist {
    max-height: 40vh;
    overflow-y: auto;
  }
}
.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.hint {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 8px;
}
/* 历史 */
.hist {
  display: flex;
  flex-direction: column;
}
.hist-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 6px;
  border-bottom: 1px solid var(--qv-divider);
  cursor: pointer;
  border-radius: 6px;
}
.hist-item:last-child {
  border-bottom: none;
}
.hist-item:hover,
.hist-item.active {
  background: v-bind('withAlpha(vars.primaryColor, 0.08)');
}
.hist-main {
  flex: 1;
  min-width: 0;
}
.hist-title {
  display: flex;
  align-items: center;
  gap: 6px;
}
.hist-target {
  font-size: 13px;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.hist-sub {
  font-size: 11px;
  opacity: 0.5;
  margin-top: 2px;
}
.hist-side {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}
.hist-rating {
  font-size: 13px;
  font-weight: 600;
}
/* 结果 */
.res-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.res-title {
  display: flex;
  align-items: center;
  gap: 8px;
}
.res-target {
  font-size: 16px;
  font-weight: 600;
}
.res-side {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.res-rating {
  display: flex;
  align-items: center;
  gap: 10px;
}
.switch-hint {
  font-size: 12px;
  opacity: 0.5;
  margin-left: 10px;
}
/* 反方观点等强调块 */
.anti-block {
  border: 1px solid transparent;
  border-radius: 10px;
  padding: 10px 12px;
}
/* 多角色 */
.panel-roles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(230px, 1fr));
  gap: 12px;
}
.panel-role {
  border: 1px solid var(--qv-divider);
  border-radius: 10px;
  padding: 12px;
}
.pr-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.pr-name {
  font-size: 13px;
  font-weight: 600;
}
.pr-rating {
  font-size: 12px;
  font-weight: 600;
  padding: 2px 10px;
  border-radius: 14px;
}
.pr-desc {
  font-size: 11px;
  opacity: 0.5;
  margin-top: 3px;
}
.pr-summary {
  font-size: 13px;
  line-height: 1.6;
  margin-top: 8px;
  opacity: 0.9;
}
.panel-text {
  font-size: 13px;
  line-height: 1.7;
  margin: 0;
  opacity: 0.9;
}
/* 差异卡 */
.diff {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.diff-row {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 13px;
}
.diff-label {
  font-size: 12px;
  font-weight: 600;
  opacity: 0.7;
  flex-shrink: 0;
  min-width: 64px;
}
.diff-rating {
  display: flex;
  align-items: center;
  gap: 8px;
}
.diff-arrow {
  opacity: 0.4;
}
.diff-block {
  border-top: 1px solid var(--qv-divider);
  padding-top: 10px;
}
.diff-block ul {
  margin: 6px 0 0;
  padding-left: 20px;
}
.diff-block li {
  font-size: 13px;
  line-height: 1.7;
}
.diff-summary {
  font-size: 13px;
  line-height: 1.6;
  margin: 4px 0 10px;
  opacity: 0.75;
}
.diff-summary.now {
  opacity: 1;
  font-weight: 500;
}
.rating-badge {
  font-size: 14px;
  font-weight: 700;
  padding: 3px 12px;
  border-radius: 20px;
}
.confidence {
  font-size: 12px;
  opacity: 0.6;
}
.summary {
  font-size: 15px;
  font-weight: 500;
  line-height: 1.6;
  margin: 0 0 16px;
}
.block {
  margin-bottom: 14px;
}
.block-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 6px;
  opacity: 0.85;
}
.block ul {
  margin: 0;
  padding-left: 20px;
}
.block li {
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.9;
}
.disclaimer {
  font-size: 12px;
  opacity: 0.5;
  line-height: 1.6;
  margin: 16px 0 0;
  padding-top: 12px;
  border-top: 1px solid var(--qv-divider);
}
.plan-block {
  border: 1px solid transparent;
  border-radius: 8px;
  padding: 12px 14px;
}
.plan-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(110px, 1fr));
  gap: 10px 14px;
  margin-bottom: 8px;
}
.pc-label {
  font-size: 12px;
  opacity: 0.65;
  margin-bottom: 2px;
}
.pc-value {
  font-size: 15px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.pc-help {
  display: inline-block;
  width: 14px;
  height: 14px;
  line-height: 14px;
  text-align: center;
  border-radius: 50%;
  border: 1px solid var(--qv-divider);
  font-size: 10px;
  opacity: 0.7;
  cursor: help;
  margin-left: 2px;
}
.plan-note {
  font-size: 13px;
  line-height: 1.7;
  margin: 4px 0;
  opacity: 0.9;
}
.plan-discipline {
  font-size: 12px;
  line-height: 1.7;
}
.plan-checklist {
  margin-top: 8px;
}
.raw {
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  margin: 0;
  font-family: inherit;
}
.meta {
  font-size: 11px;
  opacity: 0.45;
  margin-top: 16px;
  padding-top: 12px;
  border-top: 1px solid var(--qv-divider);
}
.stale-mode-banner {
  margin-bottom: 12px;
}
.risk-flags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 10px;
}
.trust-mb {
  margin-bottom: 14px;
}
.snapshot-pre {
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 60vh;
  overflow: auto;
  margin: 0;
}

/* 回溯校验面板 */
.hs {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.hs-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.hs-name {
  font-weight: 600;
  font-size: 15px;
}
.hs-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 10px;
}
@media (max-width: 640px) {
  .hs-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}
.hs-node {
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
  padding: 10px 12px;
}
.hs-node-label {
  font-size: 12px;
  opacity: 0.65;
  margin-bottom: 4px;
}
.hs-node-value {
  font-size: 18px;
  font-weight: 700;
}
.hs-node-sub {
  margin-top: 2px;
  font-size: 11px;
}
.hs-line {
  font-size: 13px;
  line-height: 1.7;
}
.hs-touch {
  border-top: 1px dashed var(--qv-divider);
  padding-top: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.hs-touch-form {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  align-items: center;
}
.hs-note {
  font-size: 12px;
}
.dim {
  opacity: 0.6;
  font-size: 12px;
}
</style>
