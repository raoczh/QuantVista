<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { NAlert, NButton, NCollapse, NCollapseItem, NDataTable, NInput, NInputNumber, NSelect, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import SectionCard from '@/components/SectionCard.vue'
import { SCORE_PROFILE_LABEL } from '@/api/screener'
import { getSessionEpoch } from '@/api/token'
import { isAbortError } from '@/api/client'
import {
  captureRankingArtifact, getRankingPolicyState, RANKING_ALGORITHM_LABEL, runRankingResearch, updateRankingPolicy,
  type RankingAlgorithm, type RankingPolicyState, type RankingResearchMetric, type RankingResearchReport, type RankingResearchRequest,
} from '@/api/rankingResearch'

const message = useMessage()
const form = reactive<RankingResearchRequest>({ source: 'recommendations', rec_type: 'short_term', profile: 'momentum', horizon: 10, target: 'alpha', max_dates: 480, max_symbols: 200, top_k: 5 })
const report = ref<RankingResearchReport | null>(null)
const state = ref<RankingPolicyState | null>(null)
const error = ref('')
const policyError = ref('')
const running = ref(false)
const saving = ref(false)
const policyLoading = ref(false)
const updating = ref(false)
const algorithm = ref<RankingAlgorithm>('qr1')
const artifactID = ref<number | null>(null)
const session = getSessionEpoch()
const controller = new AbortController()
let disposed = false
const active = () => !disposed && getSessionEpoch() === session
onBeforeUnmount(() => { disposed = true; controller.abort() })
const busy = computed(() => running.value || saving.value || updating.value)
const policyKey = computed(() => `${form.rec_type}:${form.profile}`)
const policy = computed(() => state.value?.policies.find(item => item.key === policyKey.value))
const actualAlgorithm = computed(() => policy.value?.algorithm || state.value?.default_algorithm || 'qr1')
const profileOptions = Object.entries(SCORE_PROFILE_LABEL).map(([value, label]) => ({ value, label }))
const horizonOptions = computed(() => (form.rec_type === 'short_term' ? [5, 10] : [20, 60]).map(value => ({ value, label: `${value} 日` })))
const algorithmOptions = Object.entries(RANKING_ALGORITHM_LABEL).map(([value, label]) => ({ value, label }))
const artifactOptions = computed(() => (state.value?.artifacts || []).filter(item => item.rec_type === form.rec_type && item.profile === form.profile).map(item => ({
  value: item.id, label: `#${item.id} · ${item.horizon} 日 · ${item.target === 'alpha' ? '超额' : '扣费'} · 截至 ${item.as_of}${item.eligible ? '' : ' · 仅研究'}`, disabled: !item.eligible,
})))
watch([policyKey, policy, () => state.value?.default_algorithm], () => {
  algorithm.value = actualAlgorithm.value
  artifactID.value = policy.value?.artifact_id || null
})
watch(() => form.rec_type, value => { form.horizon = value === 'short_term' ? 10 : 20 })

async function loadPolicy() {
  if (!active() || policyLoading.value) return
  policyLoading.value = true
  try {
    const result = await getRankingPolicyState(controller.signal)
    if (active()) { state.value = result; policyError.value = '' }
  } catch (e) { if (active() && !isAbortError(e)) policyError.value = e instanceof Error ? e.message : '评分版本读取失败' }
  finally { if (active()) policyLoading.value = false }
}
onMounted(loadPolicy)

async function run() {
  if (!active() || busy.value) return
  running.value = true
  error.value = ''
  try {
    const result = await runRankingResearch({ ...form, as_of: form.as_of?.trim() || undefined }, controller.signal)
    if (active()) report.value = result
  } catch (e) { if (active() && !isAbortError(e)) error.value = e instanceof Error ? e.message : '排序研究失败' }
  finally { if (active()) running.value = false }
}
async function saveArtifact() {
  if (!active() || busy.value || !report.value) return
  saving.value = true
  try {
    const result = await captureRankingArtifact(report.value, controller.signal)
    if (!active()) return
    message.success(`已保存模型 #${result.id}，${result.eligible ? '可在下方选择启用' : '当前仅作研究，尚不能启用'}`)
    await loadPolicy()
  } catch (e) { if (active() && !isAbortError(e)) message.error(e instanceof Error ? e.message : '模型保存失败') }
  finally { if (active()) saving.value = false }
}
async function applyPolicy() {
  if (!active() || busy.value || !state.value || policyLoading.value || policyError.value) return
  updating.value = true
  try {
    const result = await updateRankingPolicy({ rec_type: form.rec_type, profile: form.profile, algorithm: algorithm.value, artifact_id: algorithm.value === 'ridge1' ? artifactID.value || 0 : 0, base_revision: policy.value?.revision || 0 }, controller.signal)
    if (!active()) return
    state.value.policies = [...state.value.policies.filter(item => item.key !== result.key), result]
    message.success('评分版本已更新，之后提交的推荐使用该版本')
    await loadPolicy()
  } catch (e) { if (active() && !isAbortError(e)) message.error(e instanceof Error ? e.message : '版本更新失败') }
  finally { if (active()) updating.value = false }
}
const canApply = computed(() => !!state.value && !policyError.value && !policyLoading.value && !busy.value &&
  (algorithm.value !== 'ridge1' || artifactOptions.value.some(item => item.value === artifactID.value && !item.disabled)) &&
  (algorithm.value !== actualAlgorithm.value || (algorithm.value === 'ridge1' && artifactID.value !== policy.value?.artifact_id)))
const lastModel = computed(() => report.value?.folds?.at(-1)?.model)
const weights = computed(() => [...(lastModel.value?.weights || [])].sort((a, b) => Math.abs(b.weight) - Math.abs(a.weight)).slice(0, 8))
function label(value: string) { return RANKING_ALGORITHM_LABEL[value as RankingAlgorithm] || value }
function pct(value?: number | null) { return value != null && Number.isFinite(value) ? `${value.toFixed(2)}%` : '—' }
const columns: DataTableColumns<RankingResearchMetric> = [
  { title: '排序方式', key: 'algorithm', width: 150, render: row => label(row.algorithm) },
  { title: '选择 / 成交', key: 'traded', width: 100, render: row => `${row.selected} / ${row.traded}` },
  { title: '完整日期', key: 'complete_dates', width: 90 },
  { title: '目标覆盖', key: 'coverage_pct', width: 90, render: row => row.selected ? pct(row.coverage_pct) : '—' },
  { title: '可成交率', key: 'fill_rate_pct', width: 90, render: row => row.selected ? pct(row.fill_rate_pct) : '—' },
  { title: '含现金拨款均值', key: 'allocation_mean_pct', width: 130, render: row => pct(row.allocation_mean_pct) },
  { title: '拨款扣费 / 超额', key: 'allocation_net_mean_pct', width: 180, render: row => `${pct(row.allocation_net_mean_pct)} / ${pct(row.allocation_alpha_mean_pct)}` },
  { title: '成交扣费均值', key: 'mean_net_pct', width: 115, render: row => row.traded ? pct(row.mean_net_pct) : '—' },
  { title: '成交超额均值', key: 'mean_alpha_pct', width: 115, render: row => pct(row.mean_alpha_pct) },
  { title: '后 10% 分位', key: 'p10_net_pct', width: 110, render: row => row.traded ? pct(row.p10_net_pct) : '—' },
  { title: '亏损超过 5%', key: 'severe_loss_pct', width: 110, render: row => row.traded ? pct(row.severe_loss_pct) : '—' },
  { title: '未成交 / 待成熟 / 缺数据 / 强制退出', key: 'pending', width: 240, render: row => `${row.skipped} / ${row.pending} / ${row.no_data} / ${row.forced}` },
]
const reasonLabels: Record<string, string> = {
  calendar_missing: '缺交易日历', candidate_events_missing: '缺候选事实表', pit_sources_missing: '缺历史宇宙或因子快照',
  batch_budget_truncated: '批次读取达到上限', duplicate_event: '重复候选事件', oversized_fact: '事实体积异常',
  incomplete_or_invalid_fact: '缺失或损坏的新特征', other_profile: '其他评分侧重', known_filtered: '当时已被筛除',
  fact_time_or_owner_mismatch: '事实时点或归属不符', incomplete_opportunity_batch: '机会集不完整的批次',
  batch_version_or_market_mismatch: '批次算法版本或市场不一致',
  cohort_not_sampled: '未进入固定代码抽样', universe_snapshot_missing: '缺当日宇宙状态',
  cohort_no_point_in_time_universe: '缺少当日已知宇宙，无法固定历史群组',
  pit_feature_or_eligibility_unavailable: '历史特征或资格不可核验', quality_core_missing: '缺核心质量特征',
}
const featureLabels: Record<string, string> = {
  trend: '趋势', momentum: '动量', position: '位置', volume: '量能', risk: '风险', ma20_distance_atr: '距均线',
  breakout_distance_atr: '突破延伸', compression: '整理振幅', volume_contraction: '整理量能', close_location: '收盘承接',
  efficiency_20: '趋势效率', demand_5: '量价承接', pe_ttm: 'PE 对数', roe: 'ROE', revenue_growth: '营收增速', profit_growth: '净利润增速',
}
function featureLabel(value: string) { return value.startsWith('missing:') ? `${featureLabels[value.slice(8)] || value.slice(8)}缺失` : featureLabels[value] || value }
</script>

<template>
  <SectionCard title="推荐排序研究与版本">
    <p class="research-note">比较同一机会集的原加法评分、质量规则与正则线性排序。研究不会调用 AI 或补拉行情；保存模型、启用版本分别操作。</p>
    <div class="research-form">
      <label>样本来源<n-select v-model:value="form.source" :disabled="busy" :options="[{ label: '真实推荐候选', value: 'recommendations' }, { label: '历史收盘快照', value: 'snapshots' }]" /></label>
      <label>推荐周期<n-select v-model:value="form.rec_type" :disabled="busy" :options="[{ label: '短线', value: 'short_term' }, { label: '长线', value: 'long_term' }]" /></label>
      <label>评分侧重<n-select v-model:value="form.profile" :disabled="busy" :options="profileOptions" /></label>
      <label>持有交易日<n-select v-model:value="form.horizon" :disabled="busy" :options="horizonOptions" /></label>
      <label>学习与比较目标<n-select v-model:value="form.target" :disabled="busy" :options="[{ label: '扣费后超额收益', value: 'alpha' }, { label: '扣费后收益', value: 'net' }]" /></label>
    </div>
    <n-collapse class="research-advanced">
      <n-collapse-item title="日期与样本预算" name="budget">
        <div class="research-form">
          <label>截止日<n-input v-model:value="form.as_of" :disabled="busy" placeholder="YYYY-MM-DD，留空取最新" clearable /></label>
          <label>最多交易日<n-input-number v-model:value="form.max_dates" :disabled="busy" :min="120" :max="1080" :precision="0" :update-value-on-input="false" /></label>
          <label>历史抽样标的上限<n-input-number v-model:value="form.max_symbols" :disabled="busy || form.source !== 'snapshots'" :min="10" :max="500" :precision="0" :update-value-on-input="false" /></label>
          <label>每组选择数<n-input-number v-model:value="form.top_k" :disabled="busy" :min="1" :max="10" :precision="0" :update-value-on-input="false" /></label>
        </div>
      </n-collapse-item>
    </n-collapse>
    <div class="research-actions"><n-button type="primary" :loading="running" :disabled="saving || updating" @click="run">运行只读研究</n-button><span>先选择 TopK，再检查结果；未成交不会补选下一只。</span></div>
    <n-alert v-if="error" type="error" :bordered="false">{{ error }}<template v-if="report">；下方保留上次成功的结果。</template></n-alert>
    <template v-if="report">
      <div class="research-result-head">
        <b>本次结果：{{ SCORE_PROFILE_LABEL[report.request.profile] }} · {{ report.request.horizon }} 日 · {{ report.request.target === 'alpha' ? '超额收益' : '扣费收益' }} · 截至 {{ report.request.as_of }}</b>
        <n-tag size="small" :type="report.promotion_ready ? 'success' : 'warning'" :bordered="false">{{ report.promotion_ready ? '满足学习模型启用条件' : '学习模型尚不能启用' }}</n-tag>
      </div>
      <p class="research-note">输入 {{ report.coverage.input_rows }} · 特征完整 {{ report.coverage.feature_rows }} · 成熟成交 {{ report.coverage.matured }} · 基准齐全 {{ report.coverage.benchmark_rows }} · 财务完整 {{ report.coverage.finance_rows }} · {{ report.coverage.trade_dates }} 个信号日期。{{ report.coverage.sampling }}</p>
      <n-alert v-if="report.reason" type="info" :bordered="false">{{ report.reason }}</n-alert>
      <div class="research-table">
        <h4>时间外测试</h4>
        <n-data-table :columns="columns" :data="report.out_of_time || []" :row-key="row => row.algorithm" :scroll-x="1720" size="small" :bordered="false" />
      </div>
      <p class="research-note">含现金拨款均值按所选目标、信号日期等权汇总；只统计结果完整的日期。成交收益单列，缺行情与强制退出不当作零收益，也不将重叠持仓复利成年化收益。</p>
      <div v-if="report.comparisons?.length" class="research-comparisons">
        <div v-for="item in report.comparisons" :key="`${item.challenger}:${item.baseline}:${item.target}`">
          <b>{{ label(item.challenger) }} 相对 {{ label(item.baseline) }} · {{ item.target === 'alpha' ? '超额' : '扣费' }}</b>
          <span v-if="item.dates">增量 {{ pct(item.delta_pct) }} · 95% 区间 [{{ pct(item.low_95) }}, {{ pct(item.high_95) }}] · {{ item.dates }} 个完整配对日期</span>
          <span v-else>尚无完整配对日期，不能计算增量。</span>
        </div>
      </div>
      <ul v-if="report.promotion_reasons?.length" class="research-reasons"><li v-for="reason in report.promotion_reasons" :key="reason">{{ reason }}</li></ul>
      <n-collapse>
        <n-collapse-item title="全样本描述统计（包含训练期，不能作为增益证据）" name="descriptive"><n-data-table :columns="columns" :data="report.descriptive || []" :row-key="row => row.algorithm" :scroll-x="1720" size="small" :bordered="false" /></n-collapse-item>
        <n-collapse-item title="时间切分、模型与缺失记录" name="method">
          <p class="research-note">训练 / 验证 / 测试 {{ report.split.train }} / {{ report.split.val }} / {{ report.split.test }} 个交易日；purge {{ report.split.purge }} 日，embargo {{ report.split.embargo }} 日。{{ report.adapted ? '窗口已按可用跨度缩小。' : '' }}</p>
          <div v-for="fold in report.folds || []" :key="fold.test_from" class="research-fold">
            <b>测试 {{ fold.test_from }} ～ {{ fold.test_to }} · {{ fold.status === 'evaluated' ? `已验证，λ=${fold.lambda}` : fold.reason || '样本不足' }}</b>
            <span>训练 {{ fold.train_from }} ～ {{ fold.train_to }}；验证 {{ fold.validate_from }} ～ {{ fold.validate_to }}；训练 {{ fold.training_rows }} 条，跨界或未成熟排除 {{ fold.purged_rows }} 条。</span>
          </div>
          <p v-if="lastModel" class="research-note">最近一折训练 {{ lastModel.samples }} 条、{{ lastModel.dates }} 个日期。以下为标准化特征的主要系数；它们描述模型关系，不代表因果。</p>
          <div class="research-weights"><span v-for="item in weights" :key="item.feature">{{ featureLabel(item.feature) }} {{ item.weight.toFixed(4) }}</span></div>
          <ul class="research-reasons"><li v-for="(count, reason) in report.coverage.reasons" :key="reason">{{ reasonLabels[reason] || reason }}：{{ count }}</li></ul>
          <ul class="research-reasons"><li v-for="note in report.notes" :key="note">{{ note }}</li></ul>
          <p class="research-hash">样本摘要 {{ report.dataset_hash }} · {{ report.version }} / {{ report.feature_version }} / {{ report.outcome_version }}</p>
        </n-collapse-item>
      </n-collapse>
      <div class="research-actions"><n-button :loading="saving" :disabled="busy || !lastModel" @click="saveArtifact">保存本次研究模型</n-button><span>保存后仍使用下方已生效版本；样本不足时不能保存空模型。</span></div>
    </template>
    <div class="research-policy">
      <h4>新推荐采用的评分版本</h4>
      <p class="research-note">适用 {{ form.rec_type === 'short_term' ? '短线' : '长线' }} · {{ SCORE_PROFILE_LABEL[form.profile] }}。已排队任务保留提交时的完整策略和算法，历史推荐保持原版本。</p>
      <n-alert v-if="policyError" type="error" :bordered="false">{{ policyError }}</n-alert>
      <p>当前：{{ state ? label(actualAlgorithm) : '尚未读取' }}<template v-if="policy?.artifact_id"> · 模型 #{{ policy.artifact_id }}</template></p>
      <div class="research-policy-controls">
        <label>评分方式<n-select v-model:value="algorithm" :disabled="busy || !state || policyLoading" :options="algorithmOptions" /></label>
        <label v-if="algorithm === 'ridge1'">已验证模型<n-select v-model:value="artifactID" :disabled="busy || policyLoading" :options="artifactOptions" placeholder="选择适用且通过门槛的模型" /></label>
        <n-button :loading="updating" :disabled="!canApply" @click="applyPolicy">应用评分版本</n-button>
        <n-button :loading="policyLoading" :disabled="busy" @click="loadPolicy">刷新版本</n-button>
      </div>
      <p class="research-note">选择质量规则或原加法评分即可回退。加法对照沿用当前候选机会集与执行检查；它不是旧版全流程重放。学习模型不会自动启用。</p>
      <p v-if="policy?.updated_at" class="research-note">本配置第 {{ policy.revision }} 次变更 · {{ new Date(policy.updated_at).toLocaleString('zh-CN', { hour12: false }) }}</p>
    </div>
  </SectionCard>
</template>

<style scoped>
.research-form { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 12px; min-width: 0; }
.research-form label, .research-policy-controls label { display: grid; gap: 5px; min-width: 0; font-size: 12px; }
.research-advanced { margin-top: 12px; }
.research-note, .research-actions span, .research-hash { font-size: 12px; line-height: 1.7; opacity: .72; overflow-wrap: anywhere; }
.research-actions, .research-result-head { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin: 16px 0; }
.research-result-head { justify-content: space-between; }
.research-table { min-width: 0; overflow: hidden; }
h4 { margin: 16px 0 10px; font-size: 14px; }
.research-comparisons { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; }
.research-comparisons > div, .research-fold { display: grid; gap: 5px; min-width: 0; font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
.research-fold { margin: 10px 0; }
.research-reasons { padding-left: 20px; font-size: 12px; line-height: 1.8; overflow-wrap: anywhere; }
.research-weights { display: flex; flex-wrap: wrap; gap: 8px 16px; font-size: 12px; }
.research-policy { border-top: 1px solid var(--qv-divider); margin-top: 20px; padding-top: 6px; }
.research-policy-controls { display: flex; flex-wrap: wrap; align-items: end; gap: 10px; }
.research-policy-controls label { flex: 1 1 210px; }
@media (max-width: 600px) {
  .research-form { grid-template-columns: 1fr; }
  .research-actions { align-items: flex-start; }
  .research-policy-controls label { flex-basis: 100%; }
}
</style>
