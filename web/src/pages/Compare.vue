<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NSwitch,
  NSpin,
  NEmpty,
  NTag,
  useMessage,
} from 'naive-ui'
import { compareStocks, type CompareResult } from '@/api/compare'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, upColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

// ---------- 输入 ----------
const inputs = ref<{ symbol: string; market: string }[]>([
  { symbol: '', market: 'cn' },
  { symbol: '', market: 'cn' },
])
function addRow() {
  if (inputs.value.length < 6) inputs.value.push({ symbol: '', market: 'cn' })
}
function removeRow(i: number) {
  if (inputs.value.length > 2) inputs.value.splice(i, 1)
}

const withAI = ref(false)

// 支持 ?symbols=600519,000001 预填（全局速查/首页速查跳转入口），填完即清 query。
onMounted(() => {
  const q = String(route.query.symbols || '')
  if (!q) return
  const syms = q
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .slice(0, 6)
  if (!syms.length) return
  while (inputs.value.length < Math.max(2, syms.length)) inputs.value.push({ symbol: '', market: 'cn' })
  syms.forEach((s, i) => {
    inputs.value[i].symbol = s
  })
  router.replace({ name: 'compare' })
})

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
async function run() {
  const symbols = inputs.value.map((r) => ({ symbol: r.symbol.trim(), market: r.market })).filter((r) => r.symbol)
  if (symbols.length < 2) {
    message.warning('请至少填写两只股票代码')
    return
  }
  if (withAI.value && !llmConfigs.value.length) {
    message.warning('未配置 LLM，将仅做指标对比')
  }
  running.value = true
  try {
    result.value = await compareStocks({ symbols, with_ai: withAI.value, llm_config_id: llmId.value })
    if (result.value.note) message.info(result.value.note)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    running.value = false
  }
}

// ---------- 展示辅助 ----------
const rows = computed(() => result.value?.rows.filter((r) => r.quote_ok) || [])
const failed = computed(() => result.value?.rows.filter((r) => !r.quote_ok) || [])

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
function pctText(n: number) {
  return (n > 0 ? '+' : '') + n.toFixed(2) + '%'
}
// 每个指标里的最优值（用于高亮）：涨跌类取最大。
function bestIndex(key: 'change_pct' | 'change_pct_5d' | 'change_pct_20d') {
  let idx = -1
  let best = -Infinity
  rows.value.forEach((r, i) => {
    if (r[key] > best) {
      best = r[key]
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
    if (r.score > best) {
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
</script>

<template>
  <PageContainer title="个股横向对比" subtitle="多股并排 · 综合评分 + 行情与技术指标 · 可选 AI 一句话点评">
    <div class="cmp" :style="styleVars">
      <SectionCard title="选择标的">
        <div class="inputs">
          <div v-for="(row, i) in inputs" :key="i" class="in-row">
            <n-input v-model:value="row.symbol" :placeholder="`股票 ${i + 1}，如 600000`" style="max-width: 200px" />
            <n-select v-model:value="row.market" :options="marketOptions" style="width: 100px" />
            <n-button v-if="inputs.length > 2" size="small" quaternary type="error" @click="removeRow(i)">移除</n-button>
          </div>
          <n-button v-if="inputs.length < 6" size="small" dashed @click="addRow">＋ 增加一只（最多 6）</n-button>
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

      <SectionCard v-if="result" title="对比结果">
        <n-spin :show="running">
          <n-empty v-if="!rows.length" description="没有取到有效行情" />
          <div v-else class="table-wrap">
            <table class="cmp-table">
              <thead>
                <tr>
                  <th class="metric-col">指标</th>
                  <th v-for="r in rows" :key="r.symbol">
                    <div class="th-name">
                      {{ r.name || r.symbol }}
                      <n-tag v-if="r.is_st" size="tiny" type="warning" :bordered="false">ST</n-tag>
                    </div>
                    <div class="th-symbol qv-mono">{{ r.symbol }}</div>
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
                    <span class="score-val">{{ r.score.toFixed(0) }}</span>
                    <n-tag size="tiny" :bordered="false" round :color="scoreTagColor(r.score)">{{ r.score_label }}</n-tag>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">现价</td>
                  <td v-for="r in rows" :key="r.symbol">{{ fmt(r.price) }}</td>
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
                    :style="{ color: pctColor(r.change_pct_5d), background: i === bestIndex('change_pct_5d') ? withAlpha(upColor, 0.1) : '' }"
                  >
                    {{ pctText(r.change_pct_5d) }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">近 20 日</td>
                  <td
                    v-for="(r, i) in rows"
                    :key="r.symbol"
                    :style="{ color: pctColor(r.change_pct_20d), background: i === bestIndex('change_pct_20d') ? withAlpha(upColor, 0.1) : '' }"
                  >
                    {{ pctText(r.change_pct_20d) }}
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">MA5 / 10 / 20</td>
                  <td v-for="r in rows" :key="r.symbol">{{ fmt(r.ma5) }} / {{ fmt(r.ma10) }} / {{ fmt(r.ma20) }}</td>
                </tr>
                <tr>
                  <td class="metric-col">均线位置</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <n-tag size="tiny" :bordered="false" round :type="r.above_ma20 ? 'error' : 'success'">{{
                      r.above_ma20 ? '站上 MA20' : 'MA20 下方'
                    }}</n-tag>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">区间高 / 低</td>
                  <td v-for="r in rows" :key="r.symbol">{{ fmt(r.period_high) }} / {{ fmt(r.period_low) }}</td>
                </tr>
                <tr>
                  <td class="metric-col">PE-TTM / PB</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <template v-if="r.valuation_ok">{{ fmtPE(r.pe_ttm) }} / {{ fmt(r.pb) }}</template>
                    <template v-else>—</template>
                  </td>
                </tr>
                <tr>
                  <td class="metric-col">总市值</td>
                  <td v-for="r in rows" :key="r.symbol">{{ r.valuation_ok ? fmtCap(r.total_cap) : '—' }}</td>
                </tr>
                <tr>
                  <td class="metric-col">换手 / 量比</td>
                  <td v-for="r in rows" :key="r.symbol">
                    <template v-if="r.valuation_ok">{{ fmt(r.turnover_rate) }}% / {{ fmt(r.volume_ratio) }}</template>
                    <template v-else>—</template>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-if="failed.length" class="failed">
            未取到行情：{{ failed.map((f) => `${f.symbol}（${f.error}）`).join('、') }}
          </div>

          <div v-if="result.ai_comment" class="ai">
            <div class="ai-title">AI 点评</div>
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
.th-symbol {
  font-size: 11px;
  opacity: 0.5;
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
.ai {
  margin-top: 16px;
  padding: 14px 16px;
  border-radius: 10px;
  background: v-bind('withAlpha(vars.primaryColor, 0.06)');
}
.ai-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 6px;
}
.ai-body {
  margin: 0;
  font-size: 14px;
  line-height: 1.7;
}
</style>
