<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NAlert, NEmpty, NModal, NSelect, NSpin, NTag } from 'naive-ui'
import {
  getAttribution,
  getDailyAuditReport,
  getRecallReport,
  getShadowReport,
  type AttributionReport,
  type CandidateAuditUserReport,
  type RecallReport,
  type RecType,
  type ShadowReport,
} from '@/api/recommendation'
import TermHelp from '@/components/TermHelp.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import { useUi } from '@/composables/useUi'

type AuditMode = '' | 'attribution' | 'shadow' | 'recall'
const mode = defineModel<AuditMode>({ default: '' })
const props = defineProps<{ type: RecType }>()
const { pctColor, vars } = useUi()
const loading = ref(false)
const error = ref('')
const horizon = ref(10)
const k = ref(50)
const attribution = ref<AttributionReport | null>(null)
const shadow = ref<ShadowReport | null>(null)
const recall = ref<RecallReport | null>(null)
const daily = ref<CandidateAuditUserReport | null>(null)
const horizonOptions = [5, 10, 20, 60].map((value) => ({ label: `${value} 交易日`, value }))
const kOptions = [20, 50, 100].map((value) => ({ label: `Top ${value}`, value }))
const title = computed(() => mode.value === 'attribution' ? '错误归因报表' : mode.value === 'shadow' ? '影子门控对照' : '候选召回与每日复盘')

async function load() {
  if (!mode.value) return
  loading.value = true
  error.value = ''
  try {
    if (mode.value === 'attribution') attribution.value = await getAttribution(props.type, horizon.value)
    else if (mode.value === 'shadow') shadow.value = await getShadowReport(props.type, horizon.value)
    else {
      const [recallResult, dailyResult] = await Promise.allSettled([
        getRecallReport(props.type, horizon.value, k.value),
        getDailyAuditReport(props.type, 30),
      ])
      if (recallResult.status === 'fulfilled') recall.value = recallResult.value
      if (dailyResult.status === 'fulfilled') daily.value = dailyResult.value
      if (recallResult.status === 'rejected' && dailyResult.status === 'rejected') throw recallResult.reason
      if (recallResult.status === 'rejected') error.value = (recallResult.reason as Error).message
    }
  } catch (reason) {
    error.value = (reason as Error).message || '审计数据读取失败'
  } finally {
    loading.value = false
  }
}
watch([mode, horizon, k], () => void load())
function signed(value: number) { return `${value > 0 ? '+' : ''}${value.toFixed(2)}%` }
</script>

<template>
  <n-modal :show="!!mode" preset="card" :title="title" class="audit-modal" :style="{ width: 'min(980px, calc(100vw - 24px))' }" @update:show="(show) => { if (!show) mode = '' }">
    <div class="toolbar">
      <n-select v-model:value="horizon" :options="horizonOptions" size="small" />
      <n-select v-if="mode === 'recall'" v-model:value="k" :options="kOptions" size="small" />
      <span>读取历史事实和程序统计，不会调用 AI，也不会改写推荐或结算标签。</span>
    </div>
    <n-alert v-if="error" type="warning" :bordered="false">{{ error }}；已成功读取的部分仍保留。</n-alert>
    <n-spin :show="loading">
      <template v-if="mode === 'attribution'">
        <n-empty v-if="attribution && !attribution.sample" description="暂无成熟样本" />
        <div v-else-if="attribution" class="audit-body">
          <div class="summary">成熟 {{ attribution.sample }} · 买入 {{ attribution.sample_buy }} · 尚未成熟 {{ attribution.pending }} · 无法成交 {{ attribution.skipped }}</div>
          <div v-for="group in attribution.groups || []" :key="`${group.dim}:${group.key}`" class="audit-row">
            <b>{{ group.dim }} · {{ group.key }}</b><span>n={{ group.sample }}</span><span>胜率 {{ group.win_rate.toFixed(1) }}%</span><span :style="{ color: pctColor(group.avg_net_pct) }">均值 {{ signed(group.avg_net_pct) }}</span><span :style="{ color: pctColor(group.p10_net_pct) }">P10 {{ signed(group.p10_net_pct) }}</span>
          </div>
        </div>
      </template>
      <template v-else-if="mode === 'shadow'">
        <n-empty v-if="shadow && !shadow.groups?.length" description="暂无影子门控成熟样本" />
        <div v-else-if="shadow" class="audit-body">
          <div class="summary">入选 buy {{ shadow.picked_buy }} · 已成熟 {{ shadow.picked_buy_matured }}</div>
          <section v-for="group in shadow.groups || []" :key="group.gate_type">
            <h4>{{ group.gate_label }} · 标记 {{ group.marked }} · 若转正会改写 {{ group.would_rewrite }}</h4>
            <div v-for="cell in [group.gated, group.ungated]" :key="cell.key" class="audit-row">
              <b>{{ cell.key === 'gated' ? '被标记' : '未标记对照' }}</b><span>n={{ cell.sample }}</span><span>胜率 {{ cell.win_rate.toFixed(1) }}%</span><span :style="{ color: pctColor(cell.avg_net_pct) }">均值 {{ signed(cell.avg_net_pct) }}</span>
            </div>
          </section>
        </div>
      </template>
      <template v-else>
        <div v-if="recall" class="recall-summary">
          <div><span>候选池召回</span><b>{{ recall.recall_pool_pct.toFixed(1) }}%</b></div>
          <div><span>AI 名单召回</span><b>{{ recall.recall_llm_pct.toFixed(1) }}%</b></div>
          <div><span>最终入选</span><b>{{ recall.recall_picked_pct.toFixed(1) }}%</b></div>
          <div><span>错失机会</span><b>{{ recall.missed_rate_pct.toFixed(1) }}%</b></div>
        </div>
        <section v-if="daily" class="daily-section">
          <header><h4>每日漏选 / 误选复盘</h4><n-tag :type="daily.outcome.evaluated ? 'success' : 'warning'" :bordered="false">{{ daily.outcome.evaluated ? '已评估' : '样本不足，未评估' }}</n-tag></header>
          <div v-for="item in daily.items || []" :key="item.id" class="daily-row">
            <StockIdentity :symbol="item.symbol" market="cn" :name="item.name" density="table" clickable />
            <span>{{ item.signal_date }} → {{ item.outcome_date }}</span>
            <span>{{ item.conclusion_code }}</span>
            <span><TermHelp term="mfe" /> {{ signed(item.mfe_pct) }} · <TermHelp term="mae" /> {{ signed(item.mae_pct) }}</span>
          </div>
        </section>
        <n-empty v-if="!loading && !recall && !daily" description="暂无召回或每日审计事实" />
      </template>
    </n-spin>
  </n-modal>
</template>

<style scoped>
.toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; }
.toolbar :deep(.n-select) { width: 130px; }
.toolbar span { font-size: 12px; opacity: .65; }
.audit-body { display: grid; gap: 8px; }
.summary { padding: 8px 0; font-size: 12px; opacity: .7; }
.audit-row,
.daily-row { display: grid; grid-template-columns: minmax(150px, 1fr) repeat(4, auto); gap: 8px 14px; padding: 9px 0; border-bottom: 1px solid v-bind('vars.dividerColor'); font-size: 12px; }
.recall-summary { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
.recall-summary > div { display: grid; gap: 3px; padding: 10px 0; }
.recall-summary span { font-size: 11px; opacity: .62; }
.recall-summary b { font-size: 20px; }
.daily-section header { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.daily-row { grid-template-columns: minmax(160px, 1fr) auto auto minmax(180px, 1fr); }
@media (max-width: 700px) {
  .audit-row,
  .daily-row { grid-template-columns: 1fr; gap: 3px; }
  .recall-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
