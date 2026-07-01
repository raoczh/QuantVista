<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NForm,
  NFormItem,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NAlert,
  NSpace,
  useMessage,
} from 'naive-ui'
import {
  createAnalysis,
  listAnalysis,
  getAnalysis,
  deleteAnalysis,
  type AnalyzeRequest,
  type AnalysisModule,
  type AnalysisView,
  type AnalysisRecord,
  type AnalysisRating,
} from '@/api/analysis'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
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
  { label: '美股', value: 'us' },
  { label: '港股', value: 'hk' },
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

async function runAnalysis() {
  if (form.value.module === 'stock' && !form.value.symbol?.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (!llmConfigs.value.length) {
    message.warning('请先在「设置」中添加并测试 LLM 配置')
    return
  }
  running.value = true
  try {
    const payload: AnalyzeRequest = {
      module: form.value.module,
      llm_config_id: form.value.llm_config_id,
      question: form.value.question?.trim() || undefined,
    }
    if (needMarket.value) payload.market = form.value.market
    if (needSymbol.value) payload.symbol = form.value.symbol?.trim()
    if (needTarget.value) payload.target = form.value.target?.trim() || undefined
    const view = await createAnalysis(payload)
    current.value = view
    if (view.status === 'degraded') {
      message.warning('模型输出未通过结构化校验，已降级为原文展示')
    } else {
      message.success('分析完成')
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
    // 无论成功/降级/失败都刷新历史（失败记录服务端已落库，便于用户看到）。
    await loadHistory()
  }
}

// ---------- 历史 ----------
const history = ref<AnalysisRecord[]>([])
const historyLoading = ref(false)
async function loadHistory() {
  historyLoading.value = true
  try {
    history.value = await listAnalysis('all', 30)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    historyLoading.value = false
  }
}
async function openRecord(rec: AnalysisRecord) {
  try {
    current.value = await getAnalysis(rec.id)
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
function ratingColor(r: string) {
  return ratingMeta[r as AnalysisRating]?.color() || flatColor.value
}
function moduleText(m: string) {
  return moduleOptions.find((o) => o.value === m)?.label || m
}
function statusText(s: string) {
  return s === 'success' ? '成功' : s === 'degraded' ? '降级' : '失败'
}
function fmtTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

onMounted(async () => {
  // 从个股页/自选跳转带参：预填模块与标的。
  if (route.query.module) form.value.module = String(route.query.module) as AnalysisModule
  if (route.query.symbol) form.value.symbol = String(route.query.symbol)
  if (route.query.market) form.value.market = String(route.query.market)
  await Promise.all([loadLLM(), loadHistory()])
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
            <n-form-item label="LLM 配置">
              <n-select
                v-model:value="form.llm_config_id"
                :options="llmOptions"
                :loading="llmLoading"
                placeholder="选择模型配置"
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
              @click="runAnalysis"
            >
              {{ running ? '分析中…' : '开始分析' }}
            </n-button>
            <div v-if="!llmLoading && !llmConfigs.length" class="hint">
              尚未配置 LLM，请先到「设置」添加并测试连接。
            </div>
          </n-form>
        </SectionCard>

        <SectionCard title="历史记录">
          <template #extra>
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
                  <n-tag v-else size="tiny" :type="h.status === 'failed' ? 'error' : 'warning'" :bordered="false">
                    {{ statusText(h.status) }}
                  </n-tag>
                  <n-popconfirm @positive-click="removeRecord(h)">
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
              <!-- 头部：模块 + 评级 + 置信度 -->
              <div class="res-head">
                <div class="res-title">
                  <n-tag size="small" round :bordered="false">{{ moduleText(current.module) }}</n-tag>
                  <span class="res-target">{{ current.target || current.title }}</span>
                </div>
                <div v-if="current.status === 'success' && current.result" class="res-rating">
                  <span
                    class="rating-badge"
                    :style="{
                      color: ratingColor(current.result.rating),
                      background: withAlpha(ratingColor(current.result.rating), 0.12),
                    }"
                    >{{ ratingText(current.result.rating) }}</span
                  >
                  <span class="confidence">置信度 {{ current.result.confidence }}</span>
                </div>
              </div>

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
                <p class="disclaimer">{{ current.result.disclaimer }}</p>
              </template>

              <!-- 降级原文 -->
              <pre v-else-if="current.status === 'degraded'" class="raw">{{ current.raw }}</pre>

              <!-- 元信息 -->
              <div class="meta">
                <n-space size="small" :wrap="true">
                  <span>模型 {{ current.model || '—' }}</span>
                  <span v-if="current.total_tokens">· token {{ current.total_tokens }}</span>
                  <span v-if="current.latency_ms">· 耗时 {{ (current.latency_ms / 1000).toFixed(1) }}s</span>
                  <span>· 版本 {{ current.prompt_version }}/{{ current.strategy_version }}</span>
                  <span>· {{ fmtTime(current.created_at) }}</span>
                </n-space>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>
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
.res-rating {
  display: flex;
  align-items: center;
  gap: 10px;
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
</style>
