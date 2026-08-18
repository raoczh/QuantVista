<script setup lang="ts">
import { computed } from 'vue'
import {
  NAlert,
  NButton,
  NEmpty,
  NRadioButton,
  NRadioGroup,
  NSpin,
  NTag,
  useThemeVars,
} from 'naive-ui'
import type {
  PortfolioOverview,
  Position,
  PositionAdviceResult,
  PositionExitAssessment,
} from '@/api/position'
import { POSITION_VERDICT_LABEL } from '@/api/position'
import { useDisplayMode } from '@/composables/useDisplayMode'
import { useUi, withAlpha } from '@/composables/useUi'
import {
  buildPositionDecisionRows,
  latestDecisionTime,
  positionDataStatusText,
  positionExitLevelLabel,
} from '@/composables/usePositionDecisionCenter'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'

const props = defineProps<{
  positions: Position[]
  overview: PortfolioOverview | null
  loading: boolean
  error: string
  focusedPositionId: number | null
  focusedAssessment: PositionExitAssessment | null
  advice: PositionAdviceResult | null
  adviceLoading: boolean
  adviceError: string
  adviceTargetPositionId: number | null
}>()

const emit = defineEmits<{
  refresh: []
  review: [position: Position]
}>()

const themeVars = useThemeVars()
const { pctColor } = useUi()
const { displayMode, setMode } = useDisplayMode()
const rows = computed(() =>
  buildPositionDecisionRows(props.positions, props.focusedPositionId, props.focusedAssessment),
)
const urgentCount = computed(() => rows.value.filter((row) => row.assessment.level === 'urgent').length)
const reviewCount = computed(() => rows.value.filter((row) => row.assessment.level === 'review').length)
const lastEvaluatedAt = computed(() => latestDecisionTime(rows.value))
const styleVars = computed(() => ({
  '--decision-border': themeVars.value.dividerColor,
  '--decision-muted': themeVars.value.textColor3,
  '--decision-surface': themeVars.value.cardColor,
  '--decision-error': themeVars.value.errorColor,
  '--decision-warning': themeVars.value.warningColor,
  '--decision-focus': withAlpha(themeVars.value.primaryColor, 0.16),
  '--decision-focus-line': themeVars.value.primaryColor,
  '--decision-evidence': withAlpha(themeVars.value.textColor3, 0.06),
}))

function fmtMoney(value: number | undefined) {
  if (value == null) return '—'
  const sign = value >= 0 ? '' : '-'
  return `${sign}${Math.abs(value).toLocaleString('zh-CN', { maximumFractionDigits: 2 })}`
}

function fmtTime(value: string) {
  if (!value) return '尚未生成'
  const time = new Date(value)
  return Number.isNaN(time.getTime()) ? value : time.toLocaleString('zh-CN', { hour12: false })
}

function dataStatusType(status: PositionExitAssessment['data_status']) {
  return status === 'ready' ? 'success' : status === 'partial' ? 'warning' : 'default'
}

function levelType(level: PositionExitAssessment['level']) {
  return level === 'urgent' ? 'error' : 'warning'
}
</script>

<template>
  <section class="decision-center" :style="styleVars" aria-labelledby="decision-center-title">
    <header class="decision-header">
      <div>
        <h2 id="decision-center-title">需要处理</h2>
        <p>程序风险等级来自已落库的持仓卖出评估；这里不执行任何交易。</p>
      </div>
      <div class="decision-tools">
        <n-radio-group
          :value="displayMode"
          size="small"
          aria-label="风险依据显示方式"
          @update:value="setMode"
        >
          <n-radio-button value="plain">简明</n-radio-button>
          <n-radio-button value="professional">专业</n-radio-button>
        </n-radio-group>
        <n-button size="small" :loading="loading" @click="emit('refresh')">刷新评估</n-button>
      </div>
    </header>

    <div class="decision-summary" aria-label="持仓风险摘要">
      <div class="summary-item is-urgent">
        <span>紧急处理</span>
        <strong class="qv-tnum">{{ urgentCount }}</strong>
      </div>
      <div class="summary-item is-review">
        <span>需要复核</span>
        <strong class="qv-tnum">{{ reviewCount }}</strong>
      </div>
      <div class="summary-item">
        <span>持仓总盈亏</span>
        <strong class="qv-tnum" :style="{ color: pctColor(overview?.total_profit || 0) }">
          {{ overview ? fmtMoney(overview.total_profit) : '—' }}
        </strong>
      </div>
      <div class="summary-item">
        <span>最后评估</span>
        <strong>{{ fmtTime(lastEvaluatedAt) }}</strong>
      </div>
    </div>

    <n-alert v-if="error" type="error" :bordered="false" title="持仓风险读取失败">
      {{ error }}
    </n-alert>

    <n-spin :show="loading && !positions.length">
      <n-empty
        v-if="!loading && !error && !rows.length"
        description="当前没有需要处理的持仓"
      />
      <div v-else class="decision-list">
        <article
          v-for="row in rows"
          :id="`position-item-${row.position.id}`"
          :key="`${row.position.id}-${row.assessment.id}`"
          class="decision-card"
          :class="[
            `is-${row.assessment.level}`,
            { 'is-focused': row.focused },
          ]"
        >
          <div class="card-heading">
            <StockIdentity
              :symbol="row.position.symbol"
              :market="row.position.market"
              :name="row.position.name"
              clickable
              actions
            />
            <div class="heading-tags">
              <n-tag :type="levelType(row.assessment.level)" size="small" :bordered="false">
                {{ positionExitLevelLabel[row.assessment.level] }}
              </n-tag>
              <n-tag v-if="row.historical" size="small" :bordered="false">通知对应评估</n-tag>
            </div>
          </div>

          <div class="decision-facts">
            <div>
              <span>当前盈亏</span>
              <strong class="qv-tnum" :style="{ color: pctColor(row.position.profit_amount) }">
                {{ row.position.quote_ok ? fmtMoney(row.position.profit_amount) : '暂时未知' }}
              </strong>
              <small v-if="row.position.quote_ok" class="qv-tnum">
                {{ row.position.profit_pct.toFixed(2) }}%
              </small>
            </div>
            <div>
              <span>数据完整性</span>
              <n-tag :type="dataStatusType(row.assessment.data_status)" size="small" :bordered="false">
                {{ positionDataStatusText(row.assessment) }}
              </n-tag>
            </div>
          </div>

          <div class="reason-block">
            <span class="field-label">最主要原因</span>
            <p>{{ row.assessment.primary_reason || '当前评估没有提供主因' }}</p>
          </div>
          <div class="asof-line">
            <TermHelp term="as_of" />：行情 {{ row.assessment.quote_as_of || '未知' }} · 日线
            {{ row.assessment.bars_as_of || '未知' }}
          </div>
          <div v-if="row.assessment.data_gaps.length" class="data-gaps">
            <TermHelp :term="row.assessment.data_status === 'partial' ? 'partial' : 'unknown'" />：
            {{ row.assessment.data_gaps.join('；') }}
          </div>
          <div class="next-action">
            <div>
              <span class="field-label">建议下一步</span>
              <p>{{ row.assessment.next_action || '先核对行情与持仓事实，再决定是否操作。' }}</p>
            </div>
            <n-button
              type="primary"
              size="small"
              :loading="adviceLoading && adviceTargetPositionId === row.position.id"
              :disabled="adviceLoading && adviceTargetPositionId !== row.position.id"
              @click="emit('review', row.position)"
            >AI 复核</n-button>
          </div>

          <details class="evidence-details">
            <summary>查看依据</summary>
            <div class="evidence-content">
              <p v-for="(item, index) in row.assessment.evidence" :key="`e-${index}`">{{ item }}</p>
              <template v-if="displayMode === 'professional'">
                <p><TermHelp term="atr" />：{{ row.assessment.atr14 || '未知' }}；保护线 {{ row.assessment.atr_line || '未知' }}</p>
                <p>MA20：{{ row.assessment.ma20 || '未知' }}；MA60：{{ row.assessment.ma60 || '未知' }}</p>
                <p>趋势状态：{{ row.assessment.trend }}；评估版本：{{ row.assessment.version || '未知' }}</p>
              </template>
              <p v-if="!row.assessment.evidence.length">本次没有额外原始证据。</p>
            </div>
          </details>
        </article>
      </div>
    </n-spin>

    <n-alert class="ai-boundary" type="info" :bordered="false" title="AI 复核边界">
      AI 复核不会自动卖出，也不会修改程序风险等级。是否交易始终由你确认。
    </n-alert>
    <n-alert v-if="adviceError" type="error" :bordered="false" title="AI 复核失败">
      {{ adviceError }}
    </n-alert>
    <section v-if="advice" class="advice-result" aria-label="AI 复核结果">
      <div v-for="item in advice.advices" :key="item.position_id" class="advice-row">
        <StockIdentity :symbol="item.symbol" market="cn" :name="item.name" density="table" />
        <n-tag size="small" :bordered="false">{{ POSITION_VERDICT_LABEL[item.verdict] }}</n-tag>
        <p>{{ item.reason }}</p>
        <small v-if="item.invalidation">失效条件：{{ item.invalidation }}</small>
      </div>
    </section>
  </section>
</template>

<style scoped>
.decision-center {
  display: grid;
  gap: 16px;
  min-width: 0;
}
.decision-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}
.decision-header h2 {
  margin: 0;
  font-size: 20px;
  letter-spacing: 0;
}
.decision-header p {
  margin: 5px 0 0;
  color: var(--decision-muted);
  line-height: 1.55;
}
.decision-tools,
.heading-tags {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.decision-summary {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  border-block: 1px solid var(--decision-border);
}
.summary-item {
  display: grid;
  min-width: 0;
  gap: 5px;
  padding: 13px 16px;
  border-right: 1px solid var(--decision-border);
}
.summary-item:last-child {
  border-right: 0;
}
.summary-item span,
.field-label,
.decision-facts span {
  color: var(--decision-muted);
  font-size: 12px;
}
.summary-item strong {
  min-width: 0;
  overflow-wrap: anywhere;
  font-size: 17px;
  letter-spacing: 0;
}
.summary-item.is-urgent strong { color: var(--decision-error); }
.summary-item.is-review strong { color: var(--decision-warning); }
.decision-list {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.decision-card {
  display: grid;
  gap: 13px;
  min-width: 0;
  padding: 16px;
  border: 1px solid var(--decision-border);
  border-left: 3px solid var(--decision-warning);
  border-radius: 6px;
  background: var(--decision-surface);
  scroll-margin-top: 84px;
}
.decision-card.is-urgent { border-left-color: var(--decision-error); }
.decision-card.is-focused {
  box-shadow: 0 0 0 3px var(--decision-focus);
  border-color: var(--decision-focus-line);
}
.card-heading,
.next-action {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  min-width: 0;
}
.decision-facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.decision-facts > div {
  display: flex;
  align-items: baseline;
  gap: 7px;
  min-width: 0;
  flex-wrap: wrap;
}
.decision-facts strong { font-size: 18px; letter-spacing: 0; }
.reason-block p,
.next-action p,
.advice-row p {
  margin: 4px 0 0;
  line-height: 1.65;
  overflow-wrap: anywhere;
}
.asof-line,
.data-gaps {
  color: var(--decision-muted);
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.data-gaps { color: var(--decision-warning); }
.next-action > div { min-width: 0; }
.next-action :deep(.n-button) { flex: 0 0 auto; }
.evidence-details {
  border-top: 1px solid var(--decision-border);
  padding-top: 10px;
}
.evidence-details summary {
  width: fit-content;
  cursor: pointer;
  color: var(--decision-muted);
}
.evidence-content {
  margin-top: 9px;
  padding: 10px 12px;
  background: var(--decision-evidence);
  border-radius: 4px;
}
.evidence-content p { margin: 0; line-height: 1.65; overflow-wrap: anywhere; }
.evidence-content p + p { margin-top: 5px; }
.ai-boundary { margin-top: 2px; }
.advice-result {
  display: grid;
  gap: 10px;
  padding-block: 4px;
}
.advice-row {
  display: grid;
  grid-template-columns: minmax(140px, auto) auto minmax(0, 1fr);
  gap: 10px;
  align-items: center;
  padding-block: 10px;
  border-bottom: 1px solid var(--decision-border);
}
.advice-row small { grid-column: 3; color: var(--decision-muted); overflow-wrap: anywhere; }
@media (max-width: 900px) {
  .decision-list { grid-template-columns: 1fr; }
  .decision-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .summary-item:nth-child(2) { border-right: 0; }
  .summary-item:nth-child(-n + 2) { border-bottom: 1px solid var(--decision-border); }
}
@media (max-width: 560px) {
  .decision-header,
  .card-heading,
  .next-action { flex-direction: column; }
  .decision-tools,
  .next-action :deep(.n-button) { width: 100%; }
  .next-action :deep(.n-button) { min-height: 36px; }
  .decision-facts { grid-template-columns: 1fr; }
  .advice-row { grid-template-columns: minmax(0, 1fr) auto; }
  .advice-row p,
  .advice-row small { grid-column: 1 / -1; }
}
</style>
