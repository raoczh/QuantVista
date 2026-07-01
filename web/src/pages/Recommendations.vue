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
  type Strategy,
  type RecommendRequest,
  type RecommendationView,
  type RecommendationBatch,
  type RecommendationItem,
  type PerformanceStats,
} from '@/api/recommendation'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
  { label: '美股', value: 'us' },
  { label: '港股', value: 'hk' },
]

// ---------- 表单 ----------
const form = ref<RecommendRequest>({
  type: 'short_term',
  market: 'cn',
  strategy: '',
  llm_config_id: undefined,
  count: 5,
})
const isShort = computed(() => form.value.type === 'short_term')

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
  running.value = true
  try {
    current.value = await generateRecommendations({ ...form.value })
    if (current.value.status === 'degraded') {
      message.warning('模型未给出候选池内的有效推荐，请调整策略或稍后重试')
    } else if (current.value.items.length) {
      message.success(`已生成 ${current.value.items.length} 个${isShort.value ? '短线' : '长线'}标的`)
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
    await loadHistory()
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

// 一键建仓：跳持仓页预填。
function buildPosition(item: RecommendationItem) {
  router.push({
    name: 'positions',
    query: { add: '1', symbol: item.symbol, market: item.market, name: item.name },
  })
}

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

// ---------- 展示辅助 ----------
function typeLabel(t: string) {
  return t === 'short_term' ? '短线' : '长线'
}
function strategyName(key: string) {
  return strategies.value.find((s) => s.key === key)?.name || key
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
  await Promise.all([loadStrategies(), loadLLM(), loadHistory(), loadPerformance()])
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
            <n-form-item label="LLM 配置">
              <n-select v-model:value="form.llm_config_id" :options="llmOptions" placeholder="选择模型配置" />
            </n-form-item>
            <n-button type="primary" block :loading="running" :disabled="running" @click="generate">
              {{ running ? '生成中…' : '生成推荐' }}
            </n-button>
            <div class="hint">
              候选池 = 自选股 ∪ 涨幅榜 ∪ 活跃榜；AI 只能从池中选，不会编造池外标的。
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
                    <span class="hist-name">{{ strategyName(h.strategy) }}</span>
                  </div>
                  <div class="hist-sub">{{ fmtTime(h.created_at) }} · 候选 {{ h.candidate_count }}</div>
                </div>
                <div class="hist-side">
                  <n-tag
                    v-if="h.status !== 'success'"
                    size="tiny"
                    :type="h.status === 'failed' ? 'error' : 'warning'"
                    :bordered="false"
                    >{{ h.status === 'failed' ? '失败' : '降级' }}</n-tag
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
            <div class="perf-row">
              <span class="perf-label">胜率</span>
              <span class="perf-val">{{ performance.win_rate.toFixed(1) }}%</span>
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
            <div class="perf-tags">
              <n-tag size="tiny" :bordered="false" round :color="{ color: withAlpha(upColor, 0.14), textColor: upColor }"
                >止盈 {{ performance.take_profit }}</n-tag
              >
              <n-tag size="tiny" :bordered="false" round :color="{ color: withAlpha(downColor, 0.14), textColor: downColor }"
                >止损 {{ performance.stop_loss }}</n-tag
              >
              <n-tag size="tiny" :bordered="false" round>过期 {{ performance.expired }}</n-tag>
              <n-tag size="tiny" :bordered="false" round>进行 {{ performance.active }}</n-tag>
            </div>
            <div class="perf-note">仅统计有价格数据的推荐；超额收益以上证指数为基准。</div>
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
                <span class="res-strategy">{{ strategyName(current.strategy) }}</span>
                <span class="res-meta">候选池 {{ current.candidate_count }} · {{ fmtTime(current.created_at) }}</span>
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

              <n-alert v-if="current.status === 'degraded'" type="warning" :bordered="false" style="margin-bottom: 12px">
                {{ current.error || '模型未给出候选池内的有效推荐' }}
              </n-alert>

              <n-empty v-if="!current.items.length && current.status !== 'degraded'" description="本批无有效标的" />

              <div class="cards">
                <div v-for="it in current.items" :key="it.id" class="card">
                  <!-- 头 -->
                  <div class="card-head">
                    <div class="card-title">
                      <span class="ct-name">{{ it.name || it.symbol }}</span>
                      <span class="ct-symbol qv-mono">{{ it.symbol }}</span>
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
                  </div>

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
                    <p class="disclaimer">{{ it.detail.disclaimer }}</p>
                  </template>

                  <div class="card-actions">
                    <n-button size="small" type="primary" ghost @click="buildPosition(it)">一键建仓</n-button>
                  </div>
                </div>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>
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
</style>
