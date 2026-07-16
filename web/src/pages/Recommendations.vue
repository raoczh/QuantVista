<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NInputNumber,
  NForm,
  NFormItem,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NAlert,
  NCollapse,
  NCollapseItem,
  NSwitch,
  NModal,
  NTooltip,
  useMessage,
} from 'naive-ui'
import {
  listStrategies,
  generateRecommendations,
  listRecommendations,
  getRecommendation,
  deleteRecommendation,
  trackRecommendation,
  getPerformance,
  getAttribution,
  createStopLossAlert,
  emptyRecFilters,
  type Strategy,
  type RecommendRequest,
  type RecommendationView,
  type RecommendationBatch,
  type RecommendationItem,
  type PerformanceStats,
  type AttributionReport,
  type RecReject,
  type RecFilters,
  type PoolCandidate,
} from '@/api/recommendation'
import { getPreference, updatePreference, type UserPreference } from '@/api/user'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { pollUntil } from '@/lib/poll'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'

const message = useMessage()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
const { llmLabel } = useLlmLabel()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

// ---------- 表单 ----------
const form = ref<RecommendRequest>({
  type: 'short_term',
  market: 'cn',
  strategy: '',
  llm_config_id: undefined,
  count: 5,
  verify: false,
})

// ---------- 筛选条件（阶段②用户硬过滤，默认从偏好读，可临时改随请求提交） ----------
const filters = ref<RecFilters>(emptyRecFilters())
const pref = ref<UserPreference | null>(null)
const savingFilters = ref(false)

// 价格快捷档（解决「资金少买不起高价股」的一键入口）。
const pricePresets = [
  { label: '价格不限', value: 0 },
  { label: '≤10元', value: 10 },
  { label: '≤20元', value: 20 },
  { label: '≤30元', value: 30 },
  { label: '≤50元', value: 50 },
]
const priceCustom = ref(false)
const pricePreset = computed({
  get: () => {
    if (priceCustom.value) return -1
    if (filters.value.price_min === 0 && pricePresets.some((p) => p.value === filters.value.price_max)) {
      return filters.value.price_max
    }
    return -1 // 当前值不属于任何快捷档 → 显示自定义
  },
  set: (v: number) => {
    if (v === -1) {
      priceCustom.value = true
      return
    }
    priceCustom.value = false
    filters.value.price_min = 0
    filters.value.price_max = v
  },
})
const pricePresetOptions = [...pricePresets.map((p) => ({ label: p.label, value: p.value })), { label: '自定义区间', value: -1 }]

// 市值快捷档（解决「觉得推的都是大票」）。
const capPresets = [
  { label: '市值不限', min: 0, max: 0 },
  { label: '≤50亿(小盘)', min: 0, max: 50 },
  { label: '30~200亿(中小盘)', min: 30, max: 200 },
  { label: '200~800亿', min: 200, max: 800 },
  { label: '≥800亿(大盘)', min: 800, max: 0 },
]
const capCustom = ref(false)
const capPreset = computed({
  get: () => {
    if (capCustom.value) return -1
    const i = capPresets.findIndex((p) => p.min === filters.value.float_cap_min_yi && p.max === filters.value.float_cap_max_yi)
    return i >= 0 ? i : -1
  },
  set: (i: number) => {
    if (i === -1) {
      capCustom.value = true
      return
    }
    capCustom.value = false
    filters.value.float_cap_min_yi = capPresets[i].min
    filters.value.float_cap_max_yi = capPresets[i].max
  },
})
const capPresetOptions = [...capPresets.map((p, i) => ({ label: p.label, value: i })), { label: '自定义区间', value: -1 }]

function parseFiltersJSON(raw: string | undefined | null): RecFilters | null {
  if (!raw) return null
  try {
    const f = JSON.parse(raw)
    return { ...emptyRecFilters(), ...f }
  } catch {
    return null
  }
}

async function loadPrefFilters() {
  try {
    pref.value = await getPreference()
    const f = parseFiltersJSON(pref.value.rec_filters_json)
    if (f) filters.value = f
  } catch {
    /* 偏好读取失败用默认，不打扰 */
  }
}

// 把当前筛选保存为默认（写入偏好 rec_filters_json，之后日报的自动推荐也走同一筛选）。
async function saveFiltersDefault() {
  if (!pref.value) {
    message.warning('偏好未加载，请稍后重试')
    return
  }
  savingFilters.value = true
  try {
    pref.value = await updatePreference({ ...pref.value, rec_filters_json: JSON.stringify(filters.value) })
    message.success('已保存为默认筛选（收盘日报的自动推荐同样生效）')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingFilters.value = false
  }
}

// 策略随类型联动。
const strategies = ref<Strategy[]>([])
const strategyOptions = computed(() =>
  strategies.value.map((s) => ({ label: `${s.name} · ${s.desc}`, value: s.key })),
)
async function loadStrategies() {
  try {
    strategies.value = await listStrategies(form.value.type)
    if (strategies.value.length && !strategies.value.find((s) => s.key === form.value.strategy)) {
      form.value.strategy = strategies.value[0].key
    }
  } catch (e) {
    message.error((e as Error).message)
  }
}
watch(() => form.value.type, loadStrategies)

// ---------- LLM ----------
const llmConfigs = ref<LLMConfig[]>([])
const llmOptions = computed(() =>
  llmConfigs.value.map((c) => ({
    label: c.is_default ? `${c.name}（默认）` : c.name,
    value: c.id,
  })),
)
async function loadLLM() {
  try {
    llmConfigs.value = await listLLMConfigs()
    const def = llmConfigs.value.find((c) => c.is_default) || llmConfigs.value[0]
    if (def && form.value.llm_config_id === undefined) form.value.llm_config_id = def.id
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 生成 ----------
const running = ref(false)
const current = ref<RecommendationView | null>(null)

async function generate() {
  if (!llmConfigs.value.length) {
    message.warning('请先在「设置」中添加并测试 LLM 配置')
    return
  }
  // 偏好接口慢/失败时表单仍是内置默认——生成前补一次加载，避免用户以为提交的是自己保存的偏好。
  if (!pref.value) await loadPrefFilters()
  running.value = true
  try {
    // 2026-07-14 异步任务化：接口立即返回 processing 批次（建池/评分/AI 精选在服务端
    // 后台执行），轮询详情直到脱离 processing——浏览器超时/刷新不再中断任务。
    const v = await generateRecommendations({ ...form.value, filters: { ...filters.value } })
    current.value = v
    if (v.status === 'processing') {
      message.info('任务已创建，正在后台生成（刷新或关闭页面不影响任务）')
      await loadHistory()
      await trackBatch(v.id)
      return
    }
    notifyResult(v)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
    await loadHistory()
  }
}

// trackBatch 轮询后台任务直到脱离 processing；页面刷新后凭历史里的 processing 批次恢复跟踪。
async function trackBatch(id: number) {
  running.value = true
  try {
    const v = await pollUntil(
      () => getRecommendation(id),
      (r) => r.status !== 'processing',
    )
    if (!current.value || current.value.id === id) current.value = v
    notifyResult(v)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
    await loadHistory()
  }
}

function notifyResult(v: RecommendationView) {
  if (v.status === 'failed') {
    message.error(v.error || '生成失败')
  } else if (v.status === 'degraded') {
    message.warning(
      v.items.length
        ? 'AI 精选超时，已生成量化降级推荐（规则生成，未经 AI 解读）'
        : '模型未给出候选池内的有效推荐，请调整策略或稍后重试',
    )
  } else if (v.items.length) {
    message.success(`已生成 ${v.items.length} 个${v.type === 'short_term' ? '短线' : '长线'}标的`)
  }
}

// ---------- 历史 ----------
const history = ref<RecommendationBatch[]>([])
const historyLoading = ref(false)
async function loadHistory() {
  historyLoading.value = true
  try {
    history.value = await listRecommendations('', 30)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    historyLoading.value = false
  }
}
async function openBatch(b: RecommendationBatch) {
  try {
    current.value = await getRecommendation(b.id)
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeBatch(b: RecommendationBatch) {
  try {
    await deleteRecommendation(b.id)
    if (current.value?.id === b.id) current.value = null
    await loadHistory()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// 一键建仓：跳持仓页预填（带 rec_id 血缘，落库后详情可见「已建仓」与价格对比）。
function buildPosition(item: RecommendationItem) {
  router.push({
    name: 'positions',
    query: { add: '1', symbol: item.symbol, market: item.market, name: item.name, rec_id: String(item.id) },
  })
}

// 点击推荐标的名/代码：新窗口打开个股详情（保留本页的生成/追踪上下文）。
function openStockDetail(item: RecommendationItem) {
  const href = router.resolve({
    name: 'stock-detail',
    params: { market: item.market || 'cn', symbol: item.symbol },
  }).href
  window.open(href, '_blank')
}

// 推荐参考价 vs 实际买入价 偏离 %。
function buyDeviationPct(item: RecommendationItem) {
  if (!item.position || !item.ref_price) return null
  return ((item.position.buy_price - item.ref_price) / item.ref_price) * 100
}

// 「为什么没选它」：详情接口返回的池内落选理由。
const rejectedList = computed<RecReject[]>(() => {
  const raw = current.value?.rejected_json
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw) as RecReject[]
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
})

// ---------- 候选池全景（透明化核心：来源/量化分/排名/被筛原因全可见） ----------
const poolList = computed<PoolCandidate[]>(() => {
  const raw = current.value?.candidate_pool
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw) as PoolCandidate[]
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
})
// 参与排名的（未被筛掉）按 rank 升序；被筛掉的排后面。
const poolRanked = computed(() =>
  poolList.value
    .filter((c) => !c.excluded)
    .sort((a, b) => (a.rank || 999) - (b.rank || 999)),
)
const poolExcluded = computed(() => poolList.value.filter((c) => !!c.excluded))

const SOURCE_LABEL: Record<string, string> = {
  watchlist: '自选',
  gainer: '涨幅榜',
  active: '成交额榜',
  turnover: '换手率榜',
  dipper: '回调榜',
  lowpb: '低PB榜',
  strategy_signal: '策略信号',
}
function sourceText(c: PoolCandidate) {
  const arr = c.sources && c.sources.length ? c.sources : c.source ? [c.source] : []
  return arr.map((s) => SOURCE_LABEL[s] || s).join('+') || '—'
}
function fmtCapYi(v: number | undefined) {
  return v && v > 0 ? (v / 1e8).toFixed(0) : '—'
}

// 详情接口 filters_json：生效条件回显 + 池快照容量保护的省略计数，同源解析。
const filtersPayload = computed<{ applied?: string[]; pool_omitted?: number }>(() => {
  const raw = current.value?.filters_json
  if (!raw) return {}
  try {
    return JSON.parse(raw) as { applied?: string[]; pool_omitted?: number }
  } catch {
    return {}
  }
})
// 本次生效的筛选条件。
const appliedFilters = computed<string[]>(() =>
  Array.isArray(filtersPayload.value.applied) ? filtersPayload.value.applied : [],
)
// 池快照被省略的条数（快照 >150 时的容量保护，被筛掉者按序截断）。
const poolOmitted = computed<number>(() => filtersPayload.value.pool_omitted || 0)

// AI 复核整体点评（verify 模式）。
const reviewOverall = computed<string>(() => {
  const raw = current.value?.review_json
  if (!raw) return ''
  try {
    const parsed = JSON.parse(raw) as { overall?: string }
    return parsed.overall || ''
  } catch {
    return ''
  }
})

// ---------- 展示辅助 ----------
function typeLabel(t: string) {
  return t === 'short_term' ? '短线' : '长线'
}
// 全量静态字典：历史列表两种类型混排，绝不能用「当前所选类型的策略列表」查名
// （旧版跨类型批次会显示原始 key 如 "value"，且随类型切换变化）。
const STRATEGY_NAME: Record<string, string> = {
  momentum: '动量突破',
  pullback: '强势回踩',
  active: '热点活跃',
  value: '价值低估',
  growth: '成长趋势',
  leader: '龙头优选',
}
function strategyName(key: string) {
  return STRATEGY_NAME[key] || strategies.value.find((s) => s.key === key)?.name || key
}
// 历史/结果标题：后端固化的 title 优先（含筛选条件），旧记录回退策略名。
function batchTitle(b: { title?: string; strategy: string }) {
  return b.title || strategyName(b.strategy)
}
function actionText(a: string) {
  return a === 'buy' ? '可考虑' : '观察'
}
function actionColor(a: string) {
  return a === 'buy' ? upColor.value : flatColor.value
}
function fmt(n: number | undefined) {
  return n == null || n === 0 ? '—' : n.toFixed(2)
}
function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

onMounted(async () => {
  await Promise.all([loadStrategies(), loadLLM(), loadHistory(), loadPerformance(), loadPrefFilters()])
  // 页面刷新恢复：仍在后台生成中的批次自动恢复跟踪（后端对陈旧 processing 会惰性判 failed）。
  const processing = history.value.find((h) => h.status === 'processing')
  if (processing) {
    current.value = await getRecommendation(processing.id).catch(() => null)
    void trackBatch(processing.id)
  }
})

// ---------- 追踪 + 表现统计 ----------
const performance = ref<PerformanceStats | null>(null)
const tracking = ref(false)
async function loadPerformance() {
  try {
    performance.value = await getPerformance(form.value.type)
  } catch {
    performance.value = null
  }
}
watch(() => form.value.type, loadPerformance)

async function refreshTracking() {
  if (!current.value) return
  tracking.value = true
  try {
    current.value = await trackRecommendation(current.value.id)
    await loadPerformance()
    message.success('追踪已刷新')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    tracking.value = false
  }
}

const OUTCOME_LABEL: Record<string, string> = {
  active: '进行中',
  take_profit: '已达止盈',
  stop_loss: '已触止损',
  expired: '已过有效期',
  tracking: '跟踪中',
  no_data: '暂无数据',
}
function outcomeLabel(o: string) {
  return OUTCOME_LABEL[o] || o
}
function outcomeColor(o: string) {
  if (o === 'take_profit') return upColor.value
  if (o === 'stop_loss') return downColor.value
  if (o === 'expired') return downColor.value
  return flatColor.value
}
// 收益率着色：涨红跌绿（沿用全站口径）。
function pctColorOf(n: number) {
  if (n > 0) return upColor.value
  if (n < 0) return downColor.value
  return flatColor.value
}
function signedPct(n: number | undefined) {
  if (n == null) return '—'
  const s = n.toFixed(2)
  return (n > 0 ? '+' : '') + s + '%'
}

// ---------- S1-1 市场状态（regime，影子模式只展示不改写） ----------
const REGIME_LABEL: Record<string, string> = { offense: '进攻', neutral: '中性', defense: '防守' }
function regimeLabel(r: string | undefined) {
  return r ? REGIME_LABEL[r] || r : ''
}
function regimeTagType(r: string | undefined): 'success' | 'warning' | 'error' | 'default' {
  if (r === 'offense') return 'success'
  if (r === 'defense') return 'error'
  if (r === 'neutral') return 'warning'
  return 'default'
}
// regime 判定依据明细（详情接口 regime_json）。
const regimeSignals = computed<string[]>(() => {
  const raw = current.value?.regime_json
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw) as { signals?: string[] }
    return Array.isArray(parsed.signals) ? parsed.signals : []
  } catch {
    return []
  }
})

// ---------- S1-4 一键挂止损提醒 ----------
const stopAlerting = ref<Record<number, boolean>>({})
async function addStopAlert(it: RecommendationItem) {
  stopAlerting.value[it.id] = true
  try {
    await createStopLossAlert(it.id)
    message.success(`已创建止损提醒：${it.name || it.symbol} ≤ ${it.detail?.stop_loss?.toFixed(2)}（命中自动暂停，可在「提醒」页管理）`)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    stopAlerting.value[it.id] = false
  }
}

// ---------- S0-6 归因报表 ----------
const showAttribution = ref(false)
const attrLoading = ref(false)
const attrReport = ref<AttributionReport | null>(null)
const attrHorizon = ref(10)
const attrHorizonOptions = [1, 5, 10, 20, 60].map((h) => ({ label: `${h} 交易日`, value: h }))
const ATTR_DIM_LABEL: Record<string, string> = {
  action: '动作',
  chg5d_bucket: '入场特征（近5日涨幅）',
  regime: '市场状态',
  strategy: '策略',
  source: '来源',
  industry: '行业',
}
const attrDims = computed(() => {
  const groups = attrReport.value?.groups || []
  const byDim = new Map<string, typeof groups>()
  for (const g of groups) {
    if (!byDim.has(g.dim)) byDim.set(g.dim, [])
    byDim.get(g.dim)!.push(g)
  }
  return [...byDim.entries()].map(([dim, cells]) => ({ dim, label: ATTR_DIM_LABEL[dim] || dim, cells }))
})
async function loadAttribution() {
  attrLoading.value = true
  try {
    attrReport.value = await getAttribution(form.value.type, attrHorizon.value)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    attrLoading.value = false
  }
}
function openAttribution() {
  showAttribution.value = true
  void loadAttribution()
}
watch(attrHorizon, () => {
  if (showAttribution.value) void loadAttribution()
})

</script>

<template>
  <PageContainer title="短线 / 长线推荐" subtitle="候选池内精选 · 结构化输出 · 不编造池外标的">
    <div class="rec" :style="styleVars">
      <!-- 左：生成 + 历史 -->
      <div class="col-form">
        <SectionCard title="生成推荐">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="类型">
              <n-radio-group v-model:value="form.type">
                <n-radio-button value="short_term">短线</n-radio-button>
                <n-radio-button value="long_term">长线</n-radio-button>
              </n-radio-group>
            </n-form-item>
            <n-form-item label="策略">
              <n-select v-model:value="form.strategy" :options="strategyOptions" />
            </n-form-item>
            <n-form-item label="市场">
              <n-select v-model:value="form.market" :options="marketOptions" />
            </n-form-item>
            <n-form-item label="数量（3-5）">
              <n-input-number v-model:value="form.count" :min="3" :max="5" style="width: 100%" />
            </n-form-item>
            <n-form-item label="筛选条件（硬过滤，被筛掉的股票会在结果页列出原因）">
              <div class="filters">
                <n-select v-model:value="pricePreset" :options="pricePresetOptions" size="small" />
                <div v-if="pricePreset === -1" class="filters-row">
                  <n-input-number v-model:value="filters.price_min" :min="0" size="small" placeholder="价格下限" style="width: 100%" />
                  <span class="filters-sep">~</span>
                  <n-input-number v-model:value="filters.price_max" :min="0" size="small" placeholder="上限，0=不限" style="width: 100%" />
                </div>
                <n-select v-model:value="capPreset" :options="capPresetOptions" size="small" />
                <div v-if="capPreset === -1" class="filters-row">
                  <n-input-number v-model:value="filters.float_cap_min_yi" :min="0" size="small" placeholder="流通市值下限(亿)" style="width: 100%" />
                  <span class="filters-sep">~</span>
                  <n-input-number v-model:value="filters.float_cap_max_yi" :min="0" size="small" placeholder="上限(亿)，0=不限" style="width: 100%" />
                </div>
                <div class="filters-row">
                  <n-input-number v-model:value="filters.turnover_min" :min="0" :max="25" size="small" placeholder="换手%下限" style="width: 100%" />
                  <span class="filters-sep">~</span>
                  <n-input-number v-model:value="filters.turnover_max" :min="0" :max="30" size="small" placeholder="换手%上限" style="width: 100%" />
                </div>
                <div class="filters-hint">换手 >30% 一律排除；20~30% 仅高位（60日区间 ≥65%）判「死亡换手」排除，低位保留并标注风险</div>
                <div class="filters-switch">
                  <span>排除已涨停（买不进）</span>
                  <n-switch v-model:value="filters.exclude_limit_up" size="small" />
                </div>
                <div class="filters-switch">
                  <span>排除创业板/科创板（仅主板个股）</span>
                  <n-switch v-model:value="filters.exclude_gem_star" size="small" />
                </div>
                <div class="filters-switch">
                  <span>追高保护：近5日涨幅上限%（0=不限）</span>
                  <n-input-number v-model:value="filters.max_gain_5d_pct" :min="0" :max="100" size="small" style="width: 90px" />
                </div>
                <n-button size="tiny" tertiary :loading="savingFilters" @click="saveFiltersDefault">
                  保存为默认筛选（日报自动推荐同样生效）
                </n-button>
              </div>
            </n-form-item>
            <n-form-item label="AI 复核（更可信，多一次调用）">
              <div class="filters-switch">
                <n-switch v-model:value="form.verify" size="small" />
                <span class="verify-hint">开启后由独立「风控复核员」逐条核对证据与价位，可否决推荐</span>
              </div>
            </n-form-item>
            <n-form-item label="LLM 配置">
              <n-select v-model:value="form.llm_config_id" :options="llmOptions" placeholder="选择模型配置" />
            </n-form-item>
            <n-button type="primary" block :loading="running" :disabled="running" @click="generate">
              {{ running ? '生成中…' : '生成推荐' }}
            </n-button>
            <div class="hint">
              流水线：自选 + 随策略组合的榜单（涨幅/成交额/换手率/回调/低PB，深度取数并补「不热」方向）→ 你的筛选 →
              本地量化评分排序（零 AI 成本）→ AI 只在 Top16 里精选并强制引用数据 → 程序核验证据数字。候选池全程透明，可在结果页展开查看每只股为什么进/为什么被筛掉。
            </div>
          </n-form>
        </SectionCard>

        <SectionCard title="历史记录">
          <template #extra>
            <n-button size="tiny" quaternary :loading="historyLoading" @click="loadHistory">刷新</n-button>
          </template>
          <n-spin :show="historyLoading && !history.length">
            <n-empty v-if="!history.length" description="暂无推荐记录" size="small" />
            <div v-else class="hist">
              <div
                v-for="h in history"
                :key="h.id"
                class="hist-item"
                :class="{ active: current?.id === h.id }"
                @click="openBatch(h)"
              >
                <div class="hist-main">
                  <div class="hist-title">
                    <n-tag size="tiny" round :bordered="false" :type="h.type === 'short_term' ? 'warning' : 'info'">{{
                      typeLabel(h.type)
                    }}</n-tag>
                    <span class="hist-name">{{ batchTitle(h) }}</span>
                  </div>
                  <div class="hist-sub">
                    {{ fmtTime(h.created_at) }} · 候选 {{ h.candidate_count }}<template v-if="llmLabel(h)"> · {{ llmLabel(h) }}</template>
                  </div>
                </div>
                <div class="hist-side">
                  <n-tag
                    v-if="h.status !== 'success'"
                    size="tiny"
                    :type="h.status === 'failed' ? 'error' : h.status === 'processing' ? 'info' : 'warning'"
                    :bordered="false"
                    >{{ h.status === 'failed' ? '失败' : h.status === 'processing' ? '生成中' : '降级' }}</n-tag
                  >
                  <n-popconfirm @positive-click="removeBatch(h)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error" @click.stop>删</n-button>
                    </template>
                    删除这条推荐记录？
                  </n-popconfirm>
                </div>
              </div>
            </div>
          </n-spin>
        </SectionCard>

        <SectionCard v-if="performance && performance.sample > 0" title="历史表现">
          <template #extra>
            <span class="perf-n">样本 n={{ performance.sample }}</span>
          </template>
          <div class="perf">
            <!-- S0-4 买入成熟口径（主指标）：只统计 action=buy 且已成熟（止盈/止损/过期）的样本，
                 watch 与未成熟不再混入分母虚增胜率。 -->
            <div class="perf-row">
              <span class="perf-label">买入胜率（已成熟）</span>
              <span class="perf-val"
                >{{ performance.buy_matured > 0 ? performance.buy_win_rate.toFixed(1) + '%' : '—' }}
                <span class="perf-sub">n={{ performance.buy_matured }}</span></span
              >
            </div>
            <div v-if="performance.buy_matured > 0" class="perf-row">
              <span class="perf-label">买入平均/中位收益</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.buy_avg_return_pct) }">
                {{ signedPct(performance.buy_avg_return_pct) }}
                <span class="perf-sub">中位 {{ signedPct(performance.buy_median_pct) }}</span>
              </span>
            </div>
            <div v-if="performance.buy_bench_sample > 0" class="perf-row">
              <span class="perf-label">买入平均超额(alpha)</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.buy_avg_alpha_pct) }"
                >{{ signedPct(performance.buy_avg_alpha_pct) }}
                <span class="perf-sub">n={{ performance.buy_bench_sample }}</span></span
              >
            </div>
            <div v-if="performance.watch_sample > 0" class="perf-row">
              <span class="perf-label">观察(watch)后续上涨占比</span>
              <span class="perf-val"
                >{{ performance.watch_win_rate.toFixed(1) }}%
                <span class="perf-sub">n={{ performance.watch_sample }}</span></span
              >
            </div>
            <div class="perf-row">
              <span class="perf-label">全样本胜率（含未成熟，参考）</span>
              <span class="perf-val perf-secondary">{{ performance.win_rate.toFixed(1) }}%</span>
            </div>
            <div class="perf-row">
              <span class="perf-label">平均收益</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.avg_return_pct) }">{{
                signedPct(performance.avg_return_pct)
              }}</span>
            </div>
            <div class="perf-row">
              <span class="perf-label">平均超额(alpha)</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.avg_alpha_pct) }"
                >{{ signedPct(performance.avg_alpha_pct) }}
                <span class="perf-sub">n={{ performance.bench_sample }}</span></span
              >
            </div>
            <div class="perf-row">
              <span class="perf-label">平均最大回撤</span>
              <span class="perf-val" :style="{ color: downColor }">-{{ performance.avg_max_drawdown_pct.toFixed(2) }}%</span>
            </div>
            <div v-if="performance.sample_7d > 0" class="perf-row">
              <span class="perf-label">7 交易日均值</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.avg_7d_pct) }"
                >{{ signedPct(performance.avg_7d_pct) }} <span class="perf-sub">n={{ performance.sample_7d }}</span></span
              >
            </div>
            <div v-if="performance.sample_14d > 0" class="perf-row">
              <span class="perf-label">14 交易日均值</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.avg_14d_pct) }"
                >{{ signedPct(performance.avg_14d_pct) }} <span class="perf-sub">n={{ performance.sample_14d }}</span></span
              >
            </div>
            <div v-if="performance.sample_30d > 0" class="perf-row">
              <span class="perf-label">30 交易日均值</span>
              <span class="perf-val" :style="{ color: pctColorOf(performance.avg_30d_pct) }"
                >{{ signedPct(performance.avg_30d_pct) }} <span class="perf-sub">n={{ performance.sample_30d }}</span></span
              >
            </div>
            <div class="perf-tags">
              <n-tag size="tiny" :bordered="false" round :color="{ color: withAlpha(upColor, 0.14), textColor: upColor }"
                >止盈 {{ performance.take_profit }}</n-tag
              >
              <n-tag size="tiny" :bordered="false" round :color="{ color: withAlpha(downColor, 0.14), textColor: downColor }"
                >止损 {{ performance.stop_loss }}</n-tag
              >
              <n-tag size="tiny" :bordered="false" round>过期 {{ performance.expired }}</n-tag>
              <n-tag size="tiny" :bordered="false" round>进行 {{ performance.active }}</n-tag>
              <n-tag v-if="performance.degraded_excluded > 0" size="tiny" :bordered="false" round
                >降级批次剔除 {{ performance.degraded_excluded }}</n-tag
              >
            </div>
            <n-button size="tiny" tertiary block @click="openAttribution">错误归因报表（按持有期/特征分组）</n-button>
            <div class="perf-note">
              仅统计有价格数据的推荐（量化降级批次单独剔除）；超额收益以上证指数为基准；买入胜率只计已成熟样本（短线终态/长线超复盘周期）。
            </div>
            <div v-if="performance.buy_matured < 10" class="perf-note">成熟买入样本较少（n&lt;10），统计结果波动大，仅供参考。</div>
          </div>
        </SectionCard>
      </div>

      <!-- 右：结果 -->
      <div class="col-result">
        <SectionCard title="推荐结果">
          <n-spin :show="running">
            <n-empty
              v-if="!current"
              description="选择类型与策略并生成，或点击左侧历史查看"
              style="padding: 40px 0"
            />
            <div v-else>
              <div class="res-head">
                <n-tag size="small" round :bordered="false" :type="current.type === 'short_term' ? 'warning' : 'info'">{{
                  typeLabel(current.type)
                }}</n-tag>
                <n-tooltip v-if="current.regime" trigger="hover" placement="bottom">
                  <template #trigger>
                    <n-tag size="small" round :bordered="false" :type="regimeTagType(current.regime)"
                      >市场状态：{{ regimeLabel(current.regime) }}</n-tag
                    >
                  </template>
                  <div class="regime-tip">
                    <div v-for="(s, i) in regimeSignals" :key="i">{{ s }}</div>
                    <div class="regime-tip-note">
                      三档判定为影子模式：仅提示，不改写推荐动作；防守档建议整体保守、降低仓位。
                    </div>
                  </div>
                </n-tooltip>
                <span class="res-strategy">{{ batchTitle(current) }}</span>
                <span class="res-meta"
                  >候选池 {{ current.candidate_count }} · {{ fmtTime(current.created_at)
                  }}<template v-if="llmLabel(current)"> · {{ llmLabel(current) }}</template></span
                >
                <n-button
                  v-if="current.items.length"
                  class="res-track"
                  size="tiny"
                  tertiary
                  :loading="tracking"
                  @click="refreshTracking"
                  >刷新追踪</n-button
                >
              </div>

              <div v-if="appliedFilters.length" class="applied-filters">
                <span class="af-label">本次筛选</span>
                <n-tag v-for="(f, i) in appliedFilters" :key="i" size="tiny" round :bordered="false">{{ f }}</n-tag>
              </div>

              <n-alert v-if="reviewOverall" type="info" :bordered="false" style="margin-bottom: 12px">
                AI 复核员：{{ reviewOverall }}
              </n-alert>

              <n-alert v-if="current.status === 'processing'" type="info" :bordered="false" style="margin-bottom: 12px">
                正在后台生成（建池 → 量化评分 → AI 精选）…… 关闭或刷新页面不影响任务，完成后自动展示。
              </n-alert>

              <n-alert v-if="current.status === 'degraded'" type="warning" :bordered="false" style="margin-bottom: 12px">
                {{ current.error || '模型未给出候选池内的有效推荐' }}
              </n-alert>

              <n-empty
                v-if="!current.items.length && current.status !== 'degraded' && current.status !== 'processing'"
                description="本批无有效标的"
              />

              <div class="cards">
                <div v-for="it in current.items" :key="it.id" class="card">
                  <!-- 头 -->
                  <div class="card-head">
                    <div class="card-title">
                      <span class="ct-name ct-link" title="新窗口打开个股详情" @click="openStockDetail(it)">{{
                        it.name || it.symbol
                      }}</span>
                      <span class="ct-symbol qv-mono ct-link" @click="openStockDetail(it)">{{ it.symbol }}</span>
                      <n-tag v-if="it.detail?.degraded_source" size="tiny" type="warning" :bordered="false" round
                        >量化降级</n-tag
                      >
                      <n-tag v-if="it.position" size="tiny" type="success" :bordered="false" round>已建仓</n-tag>
                    </div>
                    <div class="card-badges">
                      <span
                        class="action-badge"
                        :style="{ color: actionColor(it.action), background: withAlpha(actionColor(it.action), 0.12) }"
                        >{{ actionText(it.action) }}</span
                      >
                      <span class="confidence">置信度 {{ it.confidence }}</span>
                    </div>
                  </div>

                  <div class="card-sub">
                    生成时现价 <b>{{ fmt(it.ref_price) }}</b>
                    <template v-if="it.position">
                      · 实际买入 <b>{{ fmt(it.position.buy_price) }}</b> × {{ it.position.quantity }}
                      <span v-if="it.position.buy_date">（{{ it.position.buy_date }}）</span>
                      <span v-if="buyDeviationPct(it) != null" :style="{ color: pctColorOf(buyDeviationPct(it)!) }">
                        较推荐价 {{ signedPct(buyDeviationPct(it)!) }}
                      </span>
                      <n-tag v-if="it.position.status === 'closed'" size="tiny" :bordered="false">已卖出</n-tag>
                    </template>
                  </div>

                  <!-- 信任徽章：量化分/排名 · 一手成本 · 证据核验 · 综合置信 · AI 复核 -->
                  <TrustBadges
                    v-if="it.detail"
                    class="trust-mb"
                    :quant-score="it.detail.quant_score"
                    :quant-rank="it.detail.quant_rank"
                    :pool-size="it.detail.pool_size"
                    :lot-cost="it.detail.lot_cost || it.ref_price * 100"
                    :evidence-check="it.detail.evidence_check"
                    :sys-confidence="it.detail.sys_confidence"
                    :sys-confidence-why="it.detail.sys_confidence_why"
                    :review="it.detail.review"
                  />

                  <!-- 追踪状态（阶段6） -->
                  <div v-if="it.status && it.status.outcome !== 'no_data'" class="track">
                    <div class="track-head">
                      <span
                        class="track-outcome"
                        :style="{
                          color: outcomeColor(it.status.outcome),
                          background: withAlpha(outcomeColor(it.status.outcome), 0.12),
                        }"
                        >{{ outcomeLabel(it.status.outcome) }}</span
                      >
                      <span v-if="it.status.review_needed" class="track-review" :style="{ color: downColor }"
                        >建议复盘</span
                      >
                      <span class="track-updated">现价 {{ fmt(it.status.current_price) }}</span>
                    </div>
                    <div class="track-grid">
                      <div class="tk">
                        <span class="tk-label">收益</span>
                        <span class="tk-val" :style="{ color: pctColorOf(it.status.return_pct) }">{{
                          signedPct(it.status.return_pct)
                        }}</span>
                      </div>
                      <div v-if="it.status.actual_return_pct != null" class="tk">
                        <span class="tk-label">实际收益（按你的买入价）</span>
                        <span class="tk-val" :style="{ color: pctColorOf(it.status.actual_return_pct) }">{{
                          signedPct(it.status.actual_return_pct)
                        }}</span>
                      </div>
                      <div class="tk">
                        <span class="tk-label">最大涨幅</span>
                        <span class="tk-val" :style="{ color: upColor }">{{ signedPct(it.status.max_gain_pct) }}</span>
                      </div>
                      <div class="tk">
                        <span class="tk-label">最大回撤</span>
                        <span class="tk-val" :style="{ color: downColor }">-{{ it.status.max_drawdown_pct.toFixed(2) }}%</span>
                      </div>
                      <div class="tk">
                        <span class="tk-label">超额(alpha)</span>
                        <span class="tk-val" :style="{ color: pctColorOf(it.status.alpha_pct) }">{{
                          signedPct(it.status.alpha_pct)
                        }}</span>
                      </div>
                      <div v-if="current.type === 'short_term'" class="tk">
                        <span class="tk-label">交易日</span>
                        <span class="tk-val"
                          >{{ it.status.elapsed_trade_days }}<template v-if="it.status.valid_days > 0">/{{ it.status.valid_days }}</template></span
                        >
                      </div>
                    </div>
                    <div v-if="it.status.note" class="track-note">{{ it.status.note }}</div>
                  </div>

                  <template v-if="it.detail">
                    <!-- 短线关键价位 -->
                    <div v-if="current.type === 'short_term'" class="levels">
                      <div class="lv">
                        <span class="lv-label">买入区间</span>
                        <span class="lv-val">{{ fmt(it.detail.buy_zone_low) }} ~ {{ fmt(it.detail.buy_zone_high) }}</span>
                      </div>
                      <div class="lv">
                        <span class="lv-label">止盈</span>
                        <span class="lv-val" :style="{ color: upColor }">{{ fmt(it.detail.take_profit) }}</span>
                      </div>
                      <div class="lv">
                        <span class="lv-label">止损</span>
                        <span class="lv-val" :style="{ color: downColor }">{{ fmt(it.detail.stop_loss) }}</span>
                      </div>
                      <div class="lv">
                        <span class="lv-label">有效期</span>
                        <span class="lv-val">{{ it.detail.valid_days || '—' }} 交易日</span>
                      </div>
                      <div v-if="it.detail.position_pct" class="lv">
                        <span class="lv-label">建议仓位</span>
                        <n-tooltip trigger="hover" placement="top">
                          <template #trigger>
                            <span class="lv-val lv-help">{{ it.detail.position_pct.toFixed(1) }}%</span>
                          </template>
                          <div class="pos-tip">
                            目标波动模型（程序计算，非 AI 输出）：{{ it.detail.position_why }}
                          </div>
                        </n-tooltip>
                      </div>
                    </div>
                    <!-- 长线关键信息 -->
                    <div v-else class="levels">
                      <div class="lv">
                        <span class="lv-label">估值区间</span>
                        <span class="lv-val"
                          >{{ fmt(it.detail.valuation_low) }} ~ {{ fmt(it.detail.valuation_high) }}</span
                        >
                      </div>
                      <div class="lv">
                        <span class="lv-label">复盘周期</span>
                        <span class="lv-val">{{ it.detail.review_cycle || '—' }}</span>
                      </div>
                      <div v-if="it.detail.position_pct" class="lv">
                        <span class="lv-label">建议仓位</span>
                        <n-tooltip trigger="hover" placement="top">
                          <template #trigger>
                            <span class="lv-val lv-help">{{ it.detail.position_pct.toFixed(1) }}%</span>
                          </template>
                          <div class="pos-tip">
                            目标波动模型（程序计算，非 AI 输出）：{{ it.detail.position_why }}
                          </div>
                        </n-tooltip>
                      </div>
                    </div>

                    <p v-if="current.type === 'long_term' && it.detail.thesis" class="thesis">
                      {{ it.detail.thesis }}
                    </p>

                    <div v-if="it.detail.reason.length" class="block">
                      <div class="block-title">理由</div>
                      <ul>
                        <li v-for="(x, i) in it.detail.reason" :key="i">{{ x }}</li>
                      </ul>
                    </div>
                    <div v-if="it.detail.risks.length" class="block">
                      <div class="block-title" :style="{ color: downColor }">风险</div>
                      <ul>
                        <li v-for="(x, i) in it.detail.risks" :key="i">{{ x }}</li>
                      </ul>
                    </div>
                    <div v-if="it.detail.evidence.length" class="block">
                      <div class="block-title">数据依据</div>
                      <ul>
                        <li v-for="(x, i) in it.detail.evidence" :key="i">{{ x }}</li>
                      </ul>
                    </div>
                    <div v-if="current.type === 'long_term' && it.detail.key_metrics.length" class="block">
                      <div class="block-title">跟踪指标</div>
                      <div class="metrics">
                        <n-tag v-for="(m, i) in it.detail.key_metrics" :key="i" size="small" :bordered="false" round>{{ m }}</n-tag>
                      </div>
                    </div>
                    <div v-if="current.type === 'short_term' && it.detail.invalidation" class="invalid">
                      失效条件：{{ it.detail.invalidation }}
                    </div>
                    <!-- S1-4 执行纪律三条（固定展示：截住「推荐胜率」与「用户执行」的偏差） -->
                    <div v-if="current.type === 'short_term' && it.detail.buy_zone_high > 0" class="discipline">
                      执行纪律：① 买入区间外不追（高于 {{ fmt(it.detail.buy_zone_high) }} 放弃本次机会）；②
                      止损价一键挂提醒，触达坚决执行；③ T+1 首日不满仓（建议首日 ≤ 建议仓位的一半）。
                    </div>
                    <p class="disclaimer">{{ it.detail.disclaimer }}</p>
                  </template>

                  <div class="card-actions">
                    <n-button
                      v-if="current.type === 'short_term' && it.detail && it.detail.stop_loss > 0"
                      size="small"
                      tertiary
                      :loading="stopAlerting[it.id]"
                      @click="addStopAlert(it)"
                      >挂止损提醒</n-button
                    >
                    <n-button v-if="!it.position" size="small" type="primary" ghost @click="buildPosition(it)">一键建仓</n-button>
                    <n-button v-else size="small" tertiary @click="router.push({ name: 'positions' })">查看持仓</n-button>
                  </div>
                </div>
              </div>

              <!-- 候选池全景：每只股为什么进、为什么被筛掉、量化分排第几，全透明 -->
              <n-collapse v-if="poolList.length" class="rejected">
                <n-collapse-item
                  :title="`候选池全景（参与排名 ${poolRanked.length} 只 · 被筛掉 ${poolExcluded.length} 只）`"
                  name="pool"
                >
                  <div class="pool-scroll">
                    <table class="pool-table">
                      <thead>
                        <tr>
                          <th>#</th>
                          <th>名称</th>
                          <th>现价</th>
                          <th>涨跌%</th>
                          <th>换手%</th>
                          <th>量比</th>
                          <th>流通市值(亿)</th>
                          <th>量化分</th>
                          <th>来源</th>
                          <th>状态</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="c in poolRanked" :key="c.symbol">
                          <td class="qv-tnum">{{ c.rank || '—' }}</td>
                          <td>
                            <span class="pool-name">{{ c.name || c.symbol }}</span>
                            <span class="pool-symbol qv-mono">{{ c.symbol }}</span>
                          </td>
                          <td class="qv-tnum">{{ c.price.toFixed(2) }}</td>
                          <td class="qv-tnum" :style="{ color: pctColorOf(c.change_pct) }">{{ signedPct(c.change_pct) }}</td>
                          <td class="qv-tnum">{{ c.turnover_rate ? c.turnover_rate.toFixed(1) : '—' }}</td>
                          <td class="qv-tnum">{{ c.volume_ratio ? c.volume_ratio.toFixed(1) : '—' }}</td>
                          <td class="qv-tnum">{{ fmtCapYi(c.float_cap) }}</td>
                          <td class="qv-tnum pool-score" :title="(c.bonus || []).join('；') || '无策略加分项'">
                            {{ c.score ? c.score.toFixed(1) : '—' }}
                          </td>
                          <td>{{ sourceText(c) }}</td>
                          <td>
                            <n-tag v-if="c.sent_to_llm" size="tiny" type="primary" :bordered="false" round>AI 名单</n-tag>
                            <span v-else class="pool-dim">已评分</span>
                          </td>
                        </tr>
                        <tr v-for="c in poolExcluded" :key="'x-' + c.symbol" class="pool-excluded">
                          <td>—</td>
                          <td>
                            <span class="pool-name">{{ c.name || c.symbol }}</span>
                            <span class="pool-symbol qv-mono">{{ c.symbol }}</span>
                          </td>
                          <td class="qv-tnum">{{ c.price.toFixed(2) }}</td>
                          <td class="qv-tnum">{{ signedPct(c.change_pct) }}</td>
                          <td class="qv-tnum">{{ c.turnover_rate ? c.turnover_rate.toFixed(1) : '—' }}</td>
                          <td class="qv-tnum">{{ c.volume_ratio ? c.volume_ratio.toFixed(1) : '—' }}</td>
                          <td class="qv-tnum">{{ fmtCapYi(c.float_cap) }}</td>
                          <td>—</td>
                          <td>{{ sourceText(c) }}</td>
                          <td class="pool-reason">{{ c.excluded }}</td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <div class="pool-note">
                    来源=进池原因（自选/涨幅榜/成交额榜/换手率榜/回调榜/低PB榜/策略信号[全市场因子扫描命中]，随策略组合、可叠加）；量化分=五维技术评分+策略加分（0-100，悬停查看加分明细，仅排序参考不代表预期收益）；「AI
                    名单」=量化排序 Top16 交给 AI 精选，其余仅参与排名对照。
                    <template v-if="poolOmitted > 0">另有 {{ poolOmitted }} 只被筛掉的标的未展示（快照容量保护）。</template>
                  </div>
                </n-collapse-item>
              </n-collapse>

              <!-- 为什么没选它：池内落选标的的一句话理由（复盘筛选逻辑用） -->
              <n-collapse v-if="rejectedList.length" class="rejected">
                <n-collapse-item :title="`为什么没选它（${rejectedList.length}）`" name="rejected">
                  <div v-for="(r, i) in rejectedList" :key="i" class="rej-row">
                    <span class="rej-name">{{ r.name || r.symbol }}<span class="rej-symbol qv-mono"> {{ r.symbol }}</span></span>
                    <span class="rej-reason">{{ r.reason }}</span>
                  </div>
                </n-collapse-item>
              </n-collapse>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>

    <!-- S0-6 确定性错误归因报表：成熟标签按维度分组的胜率/中位/尾部亏损 -->
    <n-modal v-model:show="showAttribution" preset="card" title="错误归因报表" class="attr-modal" :style="{ maxWidth: '860px', width: 'calc(100vw - 32px)' }">
      <div class="attr-toolbar">
        <n-select v-model:value="attrHorizon" :options="attrHorizonOptions" size="small" style="width: 140px" />
        <span class="attr-meta" v-if="attrReport"
          >成熟样本 {{ attrReport.sample }} · 未成熟 {{ attrReport.pending }} · 无法成交 {{ attrReport.skipped }}</span
        >
      </div>
      <n-spin :show="attrLoading">
        <n-empty v-if="attrReport && attrReport.sample === 0" description="暂无成熟样本：标签自本批功能上线起积累，需等推荐走完持有期" />
        <div v-else-if="attrReport" class="attr-body">
          <div v-for="d in attrDims" :key="d.dim" class="attr-dim">
            <div class="attr-dim-title">{{ d.label }}</div>
            <div class="pool-scroll">
              <table class="pool-table attr-table">
                <thead>
                  <tr>
                    <th>分组</th>
                    <th>样本</th>
                    <th>胜率(净)</th>
                    <th>净收益均值</th>
                    <th>中位</th>
                    <th>P10(尾部)</th>
                    <th>严重亏损&lt;-5%</th>
                    <th>均alpha</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="c in d.cells" :key="c.key" :class="{ 'attr-thin': c.sample < 5 }">
                    <td>{{ c.key }}<span v-if="c.sample < 5" class="pool-dim">（样本不足）</span></td>
                    <td class="qv-tnum">{{ c.sample }}</td>
                    <td class="qv-tnum">{{ c.win_rate.toFixed(0) }}%</td>
                    <td class="qv-tnum" :style="{ color: pctColorOf(c.avg_net_pct) }">{{ signedPct(c.avg_net_pct) }}</td>
                    <td class="qv-tnum" :style="{ color: pctColorOf(c.median_net_pct) }">{{ signedPct(c.median_net_pct) }}</td>
                    <td class="qv-tnum" :style="{ color: pctColorOf(c.p10_net_pct) }">{{ signedPct(c.p10_net_pct) }}</td>
                    <td class="qv-tnum">{{ c.severe_loss_pct.toFixed(0) }}%</td>
                    <td class="qv-tnum" :style="{ color: pctColorOf(c.avg_alpha_pct) }">
                      {{ c.alpha_sample > 0 ? signedPct(c.avg_alpha_pct) : '—' }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
          <div class="pool-note">
            <div v-for="(n, i) in attrReport.notes" :key="i">{{ n }}</div>
          </div>
        </div>
      </n-spin>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.rec {
  display: grid;
  grid-template-columns: 340px 1fr;
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .rec {
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
/* 整页滚动下左栏固定：长结果滚动时生成表单与历史始终可见，历史在栏内滚动 */
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
}
.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.hint {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 8px;
  line-height: 1.5;
}
/* 筛选条件区 */
.filters {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 100%;
}
.filters-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.filters-sep {
  opacity: 0.5;
  flex-shrink: 0;
}
.filters-switch {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12px;
  opacity: 0.85;
}
.filters-hint {
  font-size: 12px;
  opacity: 0.55;
  line-height: 1.4;
}
.verify-hint {
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.4;
}
/* 本次筛选回显 */
.applied-filters {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.af-label {
  font-size: 12px;
  opacity: 0.55;
}
/* 信任徽章 */
.trust-mb {
  margin-bottom: 12px;
}
/* 候选池全景 */
.pool-scroll {
  overflow-x: auto;
}
.pool-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
  min-width: 720px;
}
.pool-table th {
  text-align: left;
  font-weight: 600;
  opacity: 0.6;
  padding: 6px 8px;
  border-bottom: 1px solid var(--qv-divider);
  white-space: nowrap;
}
.pool-table td {
  padding: 6px 8px;
  border-bottom: 1px dashed var(--qv-divider);
  white-space: nowrap;
}
.pool-name {
  font-weight: 500;
}
.pool-symbol {
  font-size: 11px;
  opacity: 0.5;
  margin-left: 4px;
}
.pool-score {
  font-weight: 600;
  cursor: help;
}
.pool-dim {
  opacity: 0.45;
  font-size: 11px;
}
.pool-excluded td {
  opacity: 0.5;
}
.pool-reason {
  font-size: 11px;
  white-space: normal;
  min-width: 160px;
}
.pool-note {
  font-size: 11px;
  opacity: 0.5;
  line-height: 1.6;
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
.hist-name {
  font-size: 13px;
  font-weight: 500;
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
/* 结果头 */
.res-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.res-strategy {
  font-size: 16px;
  font-weight: 600;
}
.res-meta {
  font-size: 12px;
  opacity: 0.5;
}
/* 卡片 */
.cards {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.card {
  border: 1px solid var(--qv-divider);
  border-radius: 12px;
  padding: 16px;
}
.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.card-title {
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.ct-name {
  font-size: 16px;
  font-weight: 600;
}
.ct-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.ct-link {
  cursor: pointer;
}
.ct-link:hover {
  text-decoration: underline;
  text-underline-offset: 3px;
}
.card-badges {
  display: flex;
  align-items: center;
  gap: 10px;
}
.action-badge {
  font-size: 13px;
  font-weight: 700;
  padding: 2px 10px;
  border-radius: 20px;
}
.confidence {
  font-size: 12px;
  opacity: 0.6;
}
.card-sub {
  font-size: 12px;
  opacity: 0.7;
  margin: 6px 0 12px;
}
/* 追踪状态 */
.track {
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 12px;
  background: v-bind('withAlpha(vars.primaryColor, 0.04)');
}
.track-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}
.track-outcome {
  font-size: 12px;
  font-weight: 700;
  padding: 2px 10px;
  border-radius: 20px;
}
.track-review {
  font-size: 12px;
  font-weight: 600;
}
.track-updated {
  font-size: 12px;
  opacity: 0.6;
  margin-left: auto;
}
.track-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
}
.tk {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.tk-label {
  font-size: 11px;
  opacity: 0.55;
}
.tk-val {
  font-size: 14px;
  font-weight: 600;
}
.track-note {
  font-size: 11px;
  opacity: 0.55;
  margin-top: 8px;
}
/* 历史表现 */
.perf {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.perf-n {
  font-size: 12px;
  opacity: 0.6;
}
.perf-row {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
}
.perf-label {
  font-size: 12px;
  opacity: 0.6;
}
.perf-val {
  font-size: 15px;
  font-weight: 600;
}
.perf-sub {
  font-size: 11px;
  opacity: 0.5;
  font-weight: 400;
}
.perf-tags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 2px;
}
.perf-note {
  font-size: 11px;
  opacity: 0.5;
  line-height: 1.5;
}
.res-track {
  margin-left: auto;
}
.levels {
  display: flex;
  flex-wrap: wrap;
  gap: 18px;
  padding: 10px 12px;
  border-radius: 8px;
  background: v-bind('withAlpha(vars.primaryColor, 0.05)');
  margin-bottom: 12px;
}
.lv {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.lv-label {
  font-size: 11px;
  opacity: 0.55;
}
.lv-val {
  font-size: 14px;
  font-weight: 600;
}
.thesis {
  font-size: 13px;
  line-height: 1.6;
  margin: 0 0 12px;
}
.block {
  margin-bottom: 10px;
}
.block-title {
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 4px;
  opacity: 0.85;
}
.block ul {
  margin: 0;
  padding-left: 18px;
}
.block li {
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.9;
}
.metrics {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.invalid {
  font-size: 12px;
  opacity: 0.7;
  margin: 8px 0;
}
/* S1-4 执行纪律 */
.discipline {
  font-size: 12px;
  line-height: 1.6;
  padding: 8px 10px;
  border-radius: 8px;
  background: v-bind('withAlpha(vars.warningColor, 0.08)');
  margin: 8px 0;
}
/* S1-1 regime tooltip 与 S1-2 仓位 tooltip */
.regime-tip,
.pos-tip {
  max-width: 320px;
  font-size: 12px;
  line-height: 1.6;
}
.regime-tip-note {
  opacity: 0.7;
  margin-top: 6px;
}
.lv-help {
  cursor: help;
  text-decoration: underline dotted;
  text-underline-offset: 3px;
}
.perf-secondary {
  opacity: 0.6;
  font-weight: 500;
}
/* S0-6 归因报表 */
.attr-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.attr-meta {
  font-size: 12px;
  opacity: 0.6;
}
.attr-body {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.attr-dim-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 6px;
}
.attr-table {
  min-width: 620px;
}
.attr-thin td {
  opacity: 0.55;
}
.disclaimer {
  font-size: 11px;
  opacity: 0.45;
  line-height: 1.5;
  margin: 12px 0 0;
  padding-top: 10px;
  border-top: 1px solid var(--qv-divider);
}
.card-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.rejected {
  margin-top: 16px;
}
.rej-row {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 6px 0;
  border-bottom: 1px dashed var(--qv-divider);
  font-size: 13px;
}
.rej-row:last-child {
  border-bottom: none;
}
.rej-name {
  font-weight: 500;
  flex-shrink: 0;
  min-width: 120px;
}
.rej-symbol {
  font-size: 11px;
  opacity: 0.5;
}
.rej-reason {
  opacity: 0.75;
  line-height: 1.5;
}
</style>
