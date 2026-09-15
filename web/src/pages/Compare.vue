<script setup lang="ts">
import { ref, computed, onBeforeUnmount, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NSelect,
  NSwitch,
  NSpin,
  NEmpty,
  NTag,
  NAlert,
  useMessage,
} from 'naive-ui'
import { compareStocks, type CompareResult } from '@/api/compare'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getLLMTask, isLLMTask, listLLMTasks, type LLMTask } from '@/api/llmTask'
import { isPollCancelled, pollUntil } from '@/lib/poll'
import { formatPrice } from '@/lib/formatPrice'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'
import StockPicker from '@/components/StockPicker.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import type { StockRef } from '@/composables/useStockActions'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, upColor, vars, withAlpha } = useUi()
const { llmLabel } = useLlmLabel()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

// ---------- 输入 ----------
const inputs = ref<StockRef[]>([
  { symbol: '', market: 'cn', name: '' },
  { symbol: '', market: 'cn', name: '' },
])
function addRow() {
  if (inputs.value.length < 6) inputs.value.push({ symbol: '', market: 'cn', name: '' })
}
function setCompareStock(index: number, stock: StockRef | null) {
  inputs.value[index] = stock || { symbol: '', market: 'cn', name: '' }
}
function removeRow(i: number) {
  if (inputs.value.length > 2) inputs.value.splice(i, 1)
}

const withAI = ref(false)

// 支持 ?symbols=600519,000001 预填（全局搜索/首页入口），填完即清 query。
function applyStockActionQuery() {
  const q = String(route.query.symbols || '')
  if (!q) return
  const syms = q
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .slice(0, 6)
  if (!syms.length) return
  inputs.value = syms.map((symbol, i) => ({ symbol, market: i === 0 && route.query.market ? String(route.query.market) : 'cn',
    name: i === 0 && route.query.name ? String(route.query.name) : '' }))
  while (inputs.value.length < 2) inputs.value.push({ symbol: '', market: 'cn', name: '' })
  const query = { ...route.query }
  for (const key of ['symbols', 'symbol', 'market', 'name', '_stock_action']) delete query[key]
  void router.replace({ name: 'compare', query })
}

watch(() => route.query._stock_action, applyStockActionQuery)
onMounted(applyStockActionQuery)

// ---------- LLM ----------
const llmConfigs = ref<LLMConfig[]>([])
const llmId = ref<number | undefined>(undefined)
const llmOptions = computed(() =>
  llmConfigs.value.map((c) => ({ label: c.is_default ? `${c.name}（默认）` : c.name, value: c.id })),
)
async function loadLLM() {
  try {
    llmConfigs.value = await listLLMConfigs()
    const def = llmConfigs.value.find((c) => c.is_default) || llmConfigs.value[0]
    if (def) llmId.value = def.id
  } catch {
    /* 无 LLM 也可只做指标对比 */
  }
}
loadLLM()

// ---------- 对比 ----------
const result = ref<CompareResult | null>(null)
const running = ref(false)
const activeTask = ref<LLMTask<CompareResult> | null>(null)
const taskError = ref('')
let viewEpoch = 0
let disposed = false
let pollAbort: AbortController | null = null
const currentView = (epoch: number) => !disposed && epoch === viewEpoch
function invalidateView() {
  viewEpoch++
  pollAbort?.abort()
  pollAbort = null
  running.value = false
  return viewEpoch
}
onBeforeUnmount(() => {
  disposed = true
  invalidateView()
})

function applyCompareResult(value: CompareResult) {
  result.value = value
  taskError.value = ''
  if (value.note) message.info(value.note)
}

async function run() {
  if (running.value || disposed) return
  const symbols = inputs.value.map((r) => ({ symbol: r.symbol.trim(), market: r.market })).filter((r) => r.symbol)
  if (symbols.length < 2) {
    message.warning('请至少搜索并选择两只股票')
    return
  }
  if (withAI.value && !llmConfigs.value.length) {
    message.warning('未配置 LLM，将仅做指标对比')
  }
  const epoch = invalidateView()
  running.value = true
  result.value = null
  activeTask.value = null
  taskError.value = ''
  try {
    const response = await compareStocks({ symbols, with_ai: withAI.value, llm_config_id: llmId.value })
    if (!currentView(epoch)) return
    if (isLLMTask<CompareResult>(response)) {
      activeTask.value = response
      message.info('任务已创建，正在后台生成 AI 点评（刷新或关闭页面不影响任务）')
      await trackCompareTask(response, null, epoch)
    } else {
      activeTask.value = null
      applyCompareResult(response)
    }
  } catch (e) {
    if (currentView(epoch) && !isPollCancelled(e)) {
      taskError.value = (e as Error).message
      message.error(taskError.value)
    }
  } finally {
    if (currentView(epoch)) running.value = false
  }
}

async function trackCompareTask(initial: LLMTask<CompareResult>, expectedRouteTaskID: number | null = null, epoch = viewEpoch) {
  if (!currentView(epoch)) return
  pollAbort?.abort()
  const controller = new AbortController()
  pollAbort = controller
  running.value = true
  activeTask.value = initial
  try {
    const task =
      initial.status === 'processing'
        ? await pollUntil(() => getLLMTask<CompareResult>(initial.id), (v) => v.status !== 'processing', {
            signal: controller.signal,
          })
        : initial
    if (!currentView(epoch) || controller.signal.aborted || (expectedRouteTaskID !== null && routeTaskID() !== expectedRouteTaskID)) return
    activeTask.value = task
    if (task.status === 'failed') {
      const detail = task.error || '横向对比任务执行失败'
      throw new Error(task.error_code ? `${detail}（${task.error_code}）` : detail)
    }
    if (!task.result) throw new Error('横向对比任务已完成，但未返回结果')
    applyCompareResult(task.result)
  } catch (e) {
    if (isPollCancelled(e)) return
    if (!currentView(epoch) || controller.signal.aborted || (expectedRouteTaskID !== null && routeTaskID() !== expectedRouteTaskID)) return
    taskError.value = (e as Error).message
    message.error(taskError.value)
  } finally {
    if (currentView(epoch) && pollAbort === controller) {
      pollAbort = null
      running.value = false
    }
  }
}

async function restoreCompareTask() {
  if (routeTaskID() || running.value || disposed) return
  const epoch = viewEpoch
  const tasks = await listLLMTasks<CompareResult>({ kind: 'compare', limit: 1 }).catch(() => [])
  if (!currentView(epoch) || routeTaskID()) return
  const summary = tasks[0]
  if (!summary) return
  if (summary.status === 'processing') {
    activeTask.value = summary
    void trackCompareTask(summary, null, epoch)
    return
  }
  const task = await getLLMTask<CompareResult>(summary.id).catch(() => summary)
  if (!currentView(epoch) || routeTaskID()) return
  activeTask.value = task
  if (task.status === 'success' && task.result) {
    result.value = task.result
  } else if (task.status === 'failed') {
    taskError.value = task.error || '横向对比任务执行失败'
  }
}

function routeTaskID(): number | null {
  const raw = Array.isArray(route.query.task_id) ? route.query.task_id[0] : route.query.task_id
  const id = Number(raw)
  return Number.isInteger(id) && id > 0 ? id : null
}

async function restoreRouteTask(): Promise<boolean> {
  const id = routeTaskID()
  if (!id || disposed) return false
  const epoch = viewEpoch
  running.value = true
  result.value = null
  activeTask.value = null
  taskError.value = ''
  try {
    const task = await getLLMTask<CompareResult>(id)
    if (!currentView(epoch) || routeTaskID() !== id) return true
    if (task.kind !== 'compare') {
      taskError.value = '该任务不是横向对比任务，无法在此页面打开'
      message.error(taskError.value)
      return true
    }
    await trackCompareTask(task, id, epoch)
  } catch (e) {
    if (currentView(epoch) && routeTaskID() === id) {
      taskError.value = (e as Error).message || '横向对比任务状态读取失败'
      message.error(taskError.value)
    }
  } finally {
    if (currentView(epoch) && !pollAbort) running.value = false
  }
  return true
}

watch(() => route.query.task_id, () => {
  invalidateView()
  result.value = null
  activeTask.value = null
  taskError.value = ''
  void restoreRouteTask()
}, { flush: 'sync' })

onMounted(async () => {
  if (await restoreRouteTask()) return
  await restoreCompareTask()
})

// ---------- 展示辅助 ----------
const rows = computed(() => result.value?.rows.filter((r) => r.quote_ok && r.freshness_status !== 'stale' && r.freshness_status !== 'unknown') || [])
const failed = computed(() => result.value?.rows.filter((r) => !r.quote_ok || r.freshness_status === 'stale' || r.freshness_status === 'unknown') || [])
const comparisonAsOf = computed(() => {
  const dates = rows.value.map((row) => row.quote_as_of).filter(Boolean).sort()
  return dates.length === 0 ? '未知' : dates[0] === dates.at(-1) ? dates[0] : `${dates[0]} 至 ${dates.at(-1)}`
})

function fmt(n: number) {
  return n === 0 ? '—' : n.toFixed(2)
}
// PE 负值 = 亏损，显示为"亏损"更直观。
function fmtPE(n: number) {
  if (n === 0) return '—'
  return n < 0 ? '亏损' : n.toFixed(2)
}
// 市值：元 → 亿元。
function fmtCap(n: number) {
  return n <= 0 ? '—' : (n / 1e8).toFixed(0) + ' 亿'
}
function pctText(n: number | null) {
  if (n == null) return '—'
  return (n > 0 ? '+' : '') + n.toFixed(2) + '%'
}
// 每个指标里的最优值（用于高亮）：涨跌类取最大。
function bestIndex(key: 'change_pct' | 'change_pct_5d' | 'change_pct_20d') {
  let idx = -1
  let best = -Infinity
  rows.value.forEach((r, i) => {
    const value = r[key]
    if (value != null && value > best) {
      best = value
      idx = i
    }
  })
  return idx
}
// 评分最高者。
const bestScoreIndex = computed(() => {
  let idx = -1
  let best = -Infinity
  rows.value.forEach((r, i) => {
    if (r.score != null && r.score > best) {
      best = r.score
      idx = i
    }
  })
  return idx
})
// 评分标签配色：越强越偏涨色。
function scoreTagColor(score: number) {
  const c = score >= 60 ? upColor.value : score >= 45 ? vars.value.textColor3 : vars.value.successColor
  return { color: withAlpha(c, 0.14), textColor: c }
}
// AI 点评证据核验。
const aiCheck = computed(() => {
  const c = result.value?.ai_comment_check
  return c && c.total > 0 ? c : null
})

const aiRefusalLabels: Record<string, string> = {
  insufficient_fresh_quotes: '有效行情不足',
  quota_exhausted: 'AI 次数配额已用尽',
  quota_unavailable: '配额信息暂不可用',
  llm_unavailable: '没有可用的 LLM 配置',
  llm_call_failed: 'LLM 调用失败',
  llm_response_incomplete: 'LLM 响应不完整',
  llm_content_filtered: '内容被上游安全策略拦截',
}
function aiRefusalText(code: string) {
  return aiRefusalLabels[code] || `服务拒绝（${code}）`
}
</script>

<template>
  <PageContainer title="横向对比" subtitle="在同一视图比较行情与技术指标，核对差异后再作判断。">
    <div class="cmp" :style="styleVars">
      <SectionCard title="选择标的">
        <div class="inputs">
          <div v-for="(row, i) in inputs" :key="i" class="in-row">
            <StockPicker
              :model-value="row.symbol ? row : null"
              :placeholder="`搜索第 ${i + 1} 只股票`"
              class="compare-picker"
              :disabled="running"
              @update:model-value="setCompareStock(i, $event)"
            />
            <n-button v-if="inputs.length > 2" size="small" quaternary type="error" :disabled="running" @click="removeRow(i)">移除</n-button>
          </div>
          <n-button v-if="inputs.length < 6" size="small" dashed :disabled="running" @click="addRow">＋ 增加一只（最多 6）</n-button>
        </div>
        <div class="opts">
          <div class="opt">
            <span>AI 点评</span>
            <n-switch v-model:value="withAI" />
          </div>
          <n-select
            v-if="withAI"
            v-model:value="llmId"
            :options="llmOptions"
            placeholder="LLM 配置"
            style="width: 180px"
          />
          <n-button type="primary" :loading="running" @click="run">开始对比</n-button>
        </div>
      </SectionCard>

      <n-alert v-if="activeTask?.status === 'processing'" type="info" :bordered="false">
        AI 点评正在后台生成，页面刷新或关闭不会中断任务。
      </n-alert>
      <n-alert v-else-if="taskError" type="error" :bordered="false"> 横向对比失败：{{ taskError }} </n-alert>

      <SectionCard v-if="result" title="对比结果">
        <n-spin :show="running">
          <p class="result-status">
            参与比较 {{ rows.length }} 只 · 行情时间 {{ comparisonAsOf }} · 过期或时效未知的行情不参与排名，缺失指标显示“—”。
            <TermHelp term="ma" />
          </p>
          <n-empty v-if="!rows.length" description="没有取到有效行情" />
          <div v-else class="table-wrap">
            <table class="cmp-table">
              <thead>
                <tr>
                  <th class="metric-col">指标</th>
                  <th v-for="r in rows" :key="r.symbol">
                    <div class="th-name">
                      <StockIdentity :symbol="r.symbol" :market="r.market" :name="r.name" density="table" clickable />
                      <n-tag v-if="r.is_st" size="tiny" type="warning" :bordered="false">ST</n-tag>
                    </div>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td class="metric-col">综合评分</td>
                  <td
                    v-for="(r, i) in rows"
                    :key="r.symbol"
                    :style="{ background: i === bestScoreIndex ? withAlpha(upColor, 0.12) : '' }"
                  >
                    <template v-if="r.score != null">
                      <span class="score-val">{{ r.score.toFixed(0) }}</span>
                      <n-tag size="tiny" :bordered="false" round :color="scoreTagColor(r.score)">{{ r.score_label }}</n-tag>
                    </template>
                    <template v-else>—</template>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">现价</td>
                  <td v-for="r in rows" :key="r.symbol">{{ formatPrice(r.price) }}</td>
                </tr>
                <tr>
                  <td class="metric-col">当日涨跌</td>
                  <td
                    v-for="(r, i) in rows"
                    :key="r.symbol"
                    :style="{ color: pctColor(r.change_pct), background: i === bestIndex('change_pct') ? withAlpha(upColor, 0.1) : '' }"
                  >
                    {{ pctText(r.change_pct) }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">近 5 日</td>
                  <td
                    v-for="(r, i) in rows"
                    :key="r.symbol"
                    :style="{ color: pctColor(r.change_pct_5d ?? 0), background: i === bestIndex('change_pct_5d') ? withAlpha(upColor, 0.1) : '' }"
                  >
                    {{ pctText(r.change_pct_5d) }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">近 20 日</td>
                  <td
                    v-for="(r, i) in rows"
                    :key="r.symbol"
                    :style="{ color: pctColor(r.change_pct_20d ?? 0), background: i === bestIndex('change_pct_20d') ? withAlpha(upColor, 0.1) : '' }"
                  >
                    {{ pctText(r.change_pct_20d) }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col"><TermHelp term="ma" /> MA5 / 10 / 20</td>
                  <td v-for="r in rows" :key="r.symbol">{{ formatPrice(r.ma5) }} / {{ formatPrice(r.ma10) }} / {{ formatPrice(r.ma20) }}</td>
                </tr>
                <tr>
                  <td class="metric-col">均线位置</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <n-tag v-if="r.above_ma20 != null" size="tiny" :bordered="false" round :type="r.above_ma20 ? 'error' : 'success'">{{
                      r.above_ma20 ? '站上 MA20' : 'MA20 下方'
                    }}</n-tag>
                    <template v-else>—</template>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">区间高 / 低</td>
                  <td v-for="r in rows" :key="r.symbol">{{ formatPrice(r.period_high) }} / {{ formatPrice(r.period_low) }}</td>
                </tr>
                <tr>
                  <td class="metric-col">日线截至</td>
                  <td v-for="r in rows" :key="r.symbol">{{ r.bars_as_of || '—' }}</td>
                </tr>
                <tr>
                  <td class="metric-col">PE-TTM / PB</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <template v-if="r.valuation_ok">{{ fmtPE(r.pe_ttm) }} / {{ fmt(r.pb) }}</template>
                    <template v-else-if="r.is_fund">基金无估值</template>
                    <template v-else>—</template>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">总市值</td>
                  <td v-for="r in rows" :key="r.symbol">
                    {{ r.valuation_ok ? fmtCap(r.total_cap) : r.is_fund ? '基金无估值' : '—' }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">换手 / 量比</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <template v-if="r.valuation_ok">{{ fmt(r.turnover_rate) }}% / {{ fmt(r.volume_ratio) }}</template>
                    <template v-else-if="r.is_fund">基金无估值</template>
                    <template v-else>—</template>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-for="r in rows.filter(row => row.technical_note)" :key="`${r.market}:${r.symbol}`" class="failed">
            {{ r.name || r.symbol }}：{{ r.technical_note }}
          </div>

          <div v-if="failed.length" class="failed">
            未参与对比：{{ failed.map((f) => `${f.name || '名称待补全'}（${f.symbol}，${f.error || f.freshness_status || '行情暂不可用'}）`).join('、') }}
          </div>

          <n-alert v-if="result.ai_refusal_code" type="warning" :bordered="false" class="ai-refusal">
            AI 点评未生成：{{ aiRefusalText(result.ai_refusal_code) }}。当前仅展示程序化指标对比。
          </n-alert>

          <div v-if="result.ai_comment" class="ai">
            <div class="ai-title">
              <span>AI 点评</span>
              <span
                v-if="llmLabel({ llm_config_id: result.ai_llm_config_id, provider: result.ai_provider, model: result.ai_model })"
                class="ai-model"
                >{{ llmLabel({ llm_config_id: result.ai_llm_config_id, provider: result.ai_provider, model: result.ai_model }) }}</span
              >
              <TrustBadges v-if="aiCheck" :evidence-check="aiCheck" />
            </div>
            <p class="ai-body">{{ result.ai_comment }}</p>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.cmp {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.result-status { margin: 0 0 12px; font-size: 12px; opacity: .68; }
.inputs {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.in-row {
  display: flex;
  gap: 10px;
  align-items: center;
}
/* StockPicker 内部是 n-select（默认 width:100%），在 flex 行里必须显式给宽，
 * 否则会撑满整行把「移除」按钮挤到下一行（同 Notes/ThesisCards/Qa 的处理）。
 * 此前这个 class 在模板上挂着但样式里没有定义。 */
.compare-picker {
  flex: 1 1 240px;
  min-width: 0;
  max-width: 320px;
}
@media (max-width: 768px) {
  .in-row {
    flex-wrap: wrap;
  }
  .compare-picker {
    flex-basis: 100%;
    max-width: none;
  }
  .in-row :deep(.n-input) {
    flex: 1;
    min-width: 140px;
  }
}
.opts {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid var(--qv-divider);
  flex-wrap: wrap;
}
.opt {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.table-wrap {
  overflow-x: auto;
}
.cmp-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.cmp-table th,
.cmp-table td {
  padding: 10px 14px;
  text-align: center;
  border-bottom: 1px solid var(--qv-divider);
  white-space: nowrap;
}
.metric-col {
  text-align: left !important;
  opacity: 0.6;
  font-weight: 500;
  position: sticky;
  left: 0;
  background: v-bind('vars.cardColor');
}
.th-name {
  font-size: 14px;
  font-weight: 600;
}
.score-val {
  font-size: 16px;
  font-weight: 700;
  margin-right: 6px;
}
.failed {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 12px;
}
.ai-refusal {
  margin-top: 14px;
}
.ai {
  margin-top: 16px;
  padding: 14px 16px;
  border-radius: 8px;
  background: v-bind('withAlpha(vars.primaryColor, 0.06)');
}
.ai-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 6px;
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  min-width: 0;
}
.ai-model {
  font-size: 12px;
  font-weight: 400;
  opacity: 0.55;
  min-width: 0;
  overflow-wrap: anywhere;
}
.check-chip {
  font-size: 12px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 12px;
  cursor: default;
}
.ai-body {
  margin: 0;
  font-size: 14px;
  line-height: 1.7;
  overflow-wrap: anywhere;
}
</style>
