<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { NAlert, NButton, NDataTable, NSpin, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import {
  getSelectionEval,
  type SelectionBatchView,
  type SelectionBatchDiff,
  type SelectionBootstrapCI,
  type SelectionChallengerEval,
  type SelectionEvalReport,
  type SelectionEvalSection,
  type LLMExperimentProtocol,
  type SelectionMetric,
  type SelectionPairedRow,
  type SelectionPickView,
  type SelectionScoreBlindProtocolStatus,
  type SelectionSliceRow,
} from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const { upColor, downColor } = useUi()

const report = ref<SelectionEvalReport | null>(null)
const loading = ref(false)

async function load(refresh: boolean) {
  loading.value = true
  try {
    report.value = await getSelectionEval(refresh)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

onMounted(() => void load(false))

const REC_TYPE_LABEL: Record<string, string> = {
  short_term: '短线',
  long_term: '长线',
}

const ACTION_LABEL: Record<string, string> = {
  buy: 'buy',
  watch: 'watch',
  reject: '未选 / veto',
}

const EXCLUSION_LABEL: Record<string, string> = {
  missing: '缺结果',
  pending: '待成熟',
  skipped: '不可成交',
  no_data: '无数据',
  forced: 'forced',
}

function sectionTitle(sec: SelectionEvalSection): string {
  return `${REC_TYPE_LABEL[sec.rec_type] || sec.rec_type} · 固定持有 ${sec.horizon_days} 交易日`
}

function mutedDash() {
  return h('span', { style: 'opacity:0.4' }, '—')
}

function pctNode(value: number, colored = false, suffix = '%', available = true) {
  if (!available) return mutedDash()
  const color = colored ? (value > 0 ? upColor.value : value < 0 ? downColor.value : undefined) : undefined
  return h('span', { class: 'qv-tnum', style: color ? `color:${color}` : undefined }, `${value.toFixed(2)}${suffix}`)
}

function ciNode(ci: SelectionBootstrapCI, colored = false, suffix = '%') {
  if (!ci?.sample_batches) return mutedDash()
  const color = colored ? (ci.estimate > 0 ? upColor.value : ci.estimate < 0 ? downColor.value : undefined) : undefined
  return h(
    'span',
    {
      class: 'qv-tnum se-ci',
      style: color ? `color:${color}` : undefined,
      title: `95% CI [${ci.low_95.toFixed(2)}, ${ci.high_95.toFixed(2)}]，${ci.sample_batches} 个配对批次`,
    },
    `${ci.estimate.toFixed(2)}${suffix} [${ci.low_95.toFixed(2)}, ${ci.high_95.toFixed(2)}]`,
  )
}

function metricLabel(row: SelectionMetric) {
  return h('span', { class: 'se-label-cell' }, [
    h('span', null, row.label),
    !row.evaluated
      ? h(NTag, { size: 'tiny', type: 'warning', bordered: false }, { default: () => '无可比样本' })
      : null,
  ])
}

const metricColumns = computed<DataTableColumns<SelectionMetric>>(() => [
  { title: '组别', key: 'label', width: 190, fixed: 'left', render: metricLabel },
  { title: '批次', key: 'batches', width: 66, render: (r) => h('span', { class: 'qv-tnum' }, String(r.batches)) },
  {
    title: '标的(有效/选择)',
    key: 'symbols',
    width: 118,
    render: (r) => h('span', { class: 'qv-tnum' }, `${r.sample_symbols} / ${r.selected_symbols}`),
  },
  {
    title: '覆盖率',
    key: 'coverage',
    width: 82,
    render: (r) => pctNode(r.coverage_pct, false, '%', r.selected_symbols > 0),
  },
  { title: '毛收益均值', key: 'gross', width: 98, render: (r) => pctNode(r.avg_gross_pct, true, '%', r.evaluated) },
  { title: '净收益均值', key: 'avg_net', width: 98, render: (r) => pctNode(r.avg_net_pct, true, '%', r.evaluated) },
  { title: '净收益中位', key: 'median_net', width: 98, render: (r) => pctNode(r.median_net_pct, true, '%', r.evaluated) },
  { title: '净收益 P10', key: 'p10_net', width: 92, render: (r) => pctNode(r.p10_net_pct, true, '%', r.evaluated) },
  { title: 'net>0', key: 'positive', width: 78, render: (r) => pctNode(r.net_positive_pct, false, '%', r.evaluated) },
  {
    title: 'Alpha 均值/中位',
    key: 'alpha',
    width: 138,
    render: (r) =>
      r.evaluated && r.alpha_sample > 0
        ? h('span', { class: 'qv-tnum' }, [pctNode(r.avg_alpha_pct, true), ' / ', pctNode(r.median_alpha_pct, true)])
        : mutedDash(),
  },
  { title: 'alpha>0', key: 'alpha_positive', width: 82, render: (r) => pctNode(r.alpha_positive_pct, false, '%', r.evaluated && r.alpha_sample > 0) },
  {
    title: 'net<-5%',
    key: 'severe',
    width: 82,
    render: (r) =>
      r.evaluated
        ? h('span', { class: 'qv-tnum', style: r.severe_loss_pct > 0 ? `color:${downColor.value}` : undefined }, `${r.severe_loss_pct.toFixed(2)}%`)
        : mutedDash(),
  },
  {
    title: 'MFE 均值/中位',
    key: 'mfe',
    width: 126,
    render: (r) => (r.evaluated ? h('span', { class: 'qv-tnum' }, `${r.avg_mfe_pct.toFixed(2)}% / ${r.median_mfe_pct.toFixed(2)}%`) : mutedDash()),
  },
  {
    title: 'MAE 均值/中位',
    key: 'mae',
    width: 126,
    render: (r) => (r.evaluated ? h('span', { class: 'qv-tnum' }, `${r.avg_mae_pct.toFixed(2)}% / ${r.median_mae_pct.toFixed(2)}%`) : mutedDash()),
  },
])

const batchDiffColumns = computed<DataTableColumns<SelectionBatchDiff>>(() => [
  { title: '批次', key: 'batch_id', width: 76, fixed: 'left', render: (r) => h('span', { class: 'qv-tnum' }, `#${r.batch_id}`) },
  { title: '信号日', key: 'signal_date', width: 104, render: (r) => h('span', { class: 'qv-tnum' }, r.signal_date) },
  { title: 'K', key: 'k', width: 50, render: (r) => h('span', { class: 'qv-tnum' }, String(r.k)) },
  {
    title: '左组标的',
    key: 'left_symbols',
    width: 230,
    ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'qv-mono se-symbols' }, (r.left_symbols || []).join(', ') || '—'),
  },
  {
    title: '右组标的',
    key: 'right_symbols',
    width: 230,
    ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'qv-mono se-symbols' }, (r.right_symbols || []).join(', ') || '—'),
  },
  { title: '净收益均值差', key: 'avg_net', width: 112, render: (r) => pctNode(r.avg_net_diff_pct, true) },
  { title: '中位差', key: 'median_net', width: 86, render: (r) => pctNode(r.median_net_diff_pct, true) },
  { title: 'P10 差', key: 'p10_net', width: 82, render: (r) => pctNode(r.p10_net_diff_pct, true) },
  { title: 'net>0 差', key: 'positive', width: 88, render: (r) => pctNode(r.net_positive_diff_pct, false, 'pt') },
  { title: 'Alpha 差', key: 'alpha', width: 88, render: (r) => pctNode(r.avg_alpha_diff_pct, true, '%', r.has_alpha) },
  { title: 'net<-5% 差', key: 'severe', width: 94, render: (r) => pctNode(r.severe_loss_diff_pct, false, 'pt') },
  { title: 'MFE 差', key: 'mfe', width: 78, render: (r) => pctNode(r.avg_mfe_diff_pct) },
  { title: 'MAE 差', key: 'mae', width: 78, render: (r) => pctNode(r.avg_mae_diff_pct) },
])

const pairColumns = computed<DataTableColumns<SelectionPairedRow>>(() => [
  {
    type: 'expand',
    width: 42,
    renderExpand: (row) =>
      h('div', { class: 'se-diff-expand' }, [
        h('div', { class: 'se-expand-title' }, `逐批差：${row.left_group} - ${row.right_group}`),
        h(NDataTable, {
          columns: batchDiffColumns.value,
          data: row.batch_diffs || [],
          rowKey: (r: SelectionBatchDiff) => r.batch_id,
          size: 'small',
          scrollX: 1400,
          maxHeight: 360,
        }),
      ]),
  },
  { title: '配对', key: 'label', width: 190, fixed: 'left' },
  { title: '批次', key: 'batches', width: 66, render: (r) => h('span', { class: 'qv-tnum' }, String(r.batches)) },
  {
    title: '左胜/平/右胜',
    key: 'wins',
    width: 108,
    render: (r) => h('span', { class: 'qv-tnum' }, `${r.left_wins} / ${r.ties} / ${r.right_wins}`),
  },
  { title: '净收益均值差 [95% CI]', key: 'avg_net', width: 190, render: (r) => ciNode(r.avg_net_pct, true) },
  { title: '中位差 [95% CI]', key: 'median_net', width: 178, render: (r) => ciNode(r.median_net_pct, true) },
  { title: 'P10 差 [95% CI]', key: 'p10_net', width: 178, render: (r) => ciNode(r.p10_net_pct, true) },
  { title: 'net>0 差 [95% CI]', key: 'positive', width: 180, render: (r) => ciNode(r.net_positive_pct, false, 'pt') },
  { title: 'Alpha 差 [95% CI]', key: 'alpha', width: 178, render: (r) => ciNode(r.avg_alpha_pct, true) },
  { title: 'net<-5% 差 [95% CI]', key: 'severe', width: 188, render: (r) => ciNode(r.severe_loss_pct, false, 'pt') },
  { title: 'MFE 差 [95% CI]', key: 'mfe', width: 172, render: (r) => ciNode(r.avg_mfe_pct) },
  { title: 'MAE 差 [95% CI]', key: 'mae', width: 172, render: (r) => ciNode(r.avg_mae_pct) },
])

function pickSummary(picks: SelectionPickView[] | null, showAction: boolean): string {
  return (picks || [])
    .map((pick) => {
      const name = pick.name ? `${pick.name} ` : ''
      const action = showAction && pick.action ? ` ${pick.action}` : ''
      return `#${pick.order} ${name}${pick.symbol}${action} · rank ${pick.score_rank}`
    })
    .join('；')
}

const batchColumns = computed<DataTableColumns<SelectionBatchView>>(() => [
  { title: '批次', key: 'batch_id', width: 76, fixed: 'left', render: (r) => h('span', { class: 'qv-tnum' }, `#${r.batch_id}`) },
  { title: '信号日', key: 'signal_date', width: 104, render: (r) => h('span', { class: 'qv-tnum' }, r.signal_date) },
  { title: 'N', key: 'n', width: 50, render: (r) => h('span', { class: 'qv-tnum' }, String(r.n)) },
  {
    title: '状态',
    key: 'comparable',
    width: 86,
    render: (r) =>
      h(
        NTag,
        { size: 'tiny', type: r.comparable ? 'success' : 'warning', bordered: false },
        { default: () => (r.comparable ? '可比' : EXCLUSION_LABEL[r.exclusion || ''] || r.exclusion || '剔除') },
      ),
  },
  {
    title: 'AI 最终 picks',
    key: 'ai',
    width: 420,
    ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'se-picks' }, pickSummary(r.ai, true) || '—'),
  },
  {
    title: 'Quant Top-N',
    key: 'quant',
    width: 420,
    ellipsis: { tooltip: true },
    render: (r) => h('span', { class: 'se-picks' }, pickSummary(r.quant, false) || '—'),
  },
])

const sliceColumns = computed<DataTableColumns<SelectionSliceRow>>(() => [
  { title: '取值', key: 'key', width: 230, ellipsis: { tooltip: true } },
  { title: '可比批次', key: 'batches', width: 86, render: (r) => h('span', { class: 'qv-tnum' }, String(r.batches)) },
  {
    title: '结论状态',
    key: 'evaluated',
    width: 96,
    render: (r) =>
      h(
        NTag,
        { size: 'tiny', type: r.evaluated ? 'success' : 'warning', bordered: false },
        { default: () => (r.evaluated ? '已评估' : '统计不确定') },
      ),
  },
  {
    title: 'AI - Quant 净收益均值差 [95% CI]',
    key: 'avg_net_pct',
    width: 245,
    render: (r) => (r.evaluated && r.avg_net_pct ? ciNode(r.avg_net_pct, true) : mutedDash()),
  },
  { title: '说明', key: 'note', minWidth: 190, render: (r) => r.note || '—' },
])

function comparablePairs(rows: SelectionPairedRow[] | null | undefined): SelectionPairedRow[] {
  return (rows || []).filter((row) => row.batches > 0)
}

function hasEvaluatedMetric(rows: SelectionMetric[] | null | undefined): boolean {
  return (rows || []).some((row) => row.evaluated)
}

function actionMetrics(sec: SelectionEvalSection): SelectionMetric[] {
  return (sec.action_veto || []).map((row) => ({ ...row.metric, label: row.label }))
}

function planMetrics(sec: SelectionEvalSection): SelectionMetric[] {
  return [sec.plan.fixed_hold, sec.plan.plan_l2]
}

function promptChallengers(sec: SelectionEvalSection): SelectionChallengerEval[] {
  return (sec.challengers || []).filter((item) => !item.experiment_type || item.experiment_type === 'prompt')
}

function scoreBlindExperiments(sec: SelectionEvalSection): SelectionChallengerEval[] {
  return (sec.challengers || []).filter((item) => item.experiment_type === 'score_blind')
}

function unknownChallengers(sec: SelectionEvalSection): SelectionChallengerEval[] {
  return (sec.challengers || []).filter((item) =>
    !!item.experiment_type && item.experiment_type !== 'prompt' && item.experiment_type !== 'score_blind')
}

function protocolLockComplete(challenger: SelectionChallengerEval): boolean {
  return !!challenger.protocol && !!challenger.protocol_hash && challenger.input_schema_version === 'sb1'
}

function protocolReadinessType(status: SelectionScoreBlindProtocolStatus): 'info' | 'warning' {
  return status.ready ? 'info' : 'warning'
}

function protocolGuardrailType(status: SelectionScoreBlindProtocolStatus): 'default' | 'info' | 'success' | 'error' {
  if (!status.ready) return 'default'
  if (!status.guardrails_passed) return 'error'
  return status.multiple_testing_applied ? 'success' : 'info'
}

function protocolGuardrailLabel(status: SelectionScoreBlindProtocolStatus): string {
  if (!status.ready) return '数值护栏待评估'
  if (!status.guardrails_passed) return '数值护栏未通过'
  return status.multiple_testing_applied ? '协议护栏通过' : '数值护栏通过，显著性未检验'
}

const MULTIPLE_TESTING_LABEL: Record<string, string> = {
  holm_bonferroni: 'Holm-Bonferroni',
  bonferroni: 'Bonferroni',
  benjamini_hochberg: 'Benjamini-Hochberg',
}

function multipleTestingLabel(method: string): string {
  return MULTIPLE_TESTING_LABEL[method] || method || '—'
}

function protocolSummary(protocol: LLMExperimentProtocol): string {
  const severeLossDefinition = protocol.severe_loss_definition_pct ?? -5
  return [
    `短线 ${protocol.short_horizons.join('/')} 日`,
    `长线 ${protocol.long_horizons.join('/')} 日`,
    `最小有效批次 ${protocol.min_effective_batches}`,
    `覆盖率下降 ≤ ${protocol.max_coverage_drop_pct}%`,
    `严重亏损率 ≤ ${protocol.max_severe_loss_rate_pct}%（net<${severeLossDefinition}%）`,
    `多重检验 ${multipleTestingLabel(protocol.multiple_testing_method)}`,
  ].join(' · ')
}

function transitionLabel(from: string, to: string): string {
  return `${ACTION_LABEL[from] || from} → ${ACTION_LABEL[to] || to}`
}
</script>

<template>
  <PageContainer
    title="选股配对评估"
    subtitle="S3-6B/C：so1 fixed-hold 同批配对；Prompt challenger 与 score-blind 输入实验独立分组，重算零 LLM 调用"
  >
    <div class="se-wrap">
      <SectionCard title="评估概览">
        <template #extra>
          <div class="se-toolbar">
            <span v-if="report" class="se-meta">
              {{ report.generated_at }} · Bootstrap seed {{ report.bootstrap.seed }} × {{ report.bootstrap.iterations }} · 耗时
              {{ report.elapsed_ms }}ms
            </span>
            <n-button size="small" :loading="loading" @click="load(true)">重新计算</n-button>
          </div>
        </template>

        <n-spin :show="loading">
          <template v-if="report">
            <div class="se-version-row">
              <n-tag size="small" type="success" :bordered="false">outcome {{ report.outcome_version }}</n-tag>
              <n-tag size="small" :bordered="false">schema {{ report.schema_version }}</n-tag>
              <n-tag size="small" :bordered="false">ranking {{ report.ranking_version }}</n-tag>
              <n-tag size="small" :bordered="false">shadow output {{ report.challenger_schema_version }}</n-tag>
            </div>

            <div class="se-coverage-grid">
              <div class="se-coverage-line">
                <span class="se-coverage-label">批次事实</span>
                <span class="qv-tnum">
                  总批次 {{ report.coverage.batches }} · success {{ report.coverage.success_batches }} · 事实完整
                  {{ report.coverage.facts_ready_batches }} · 机会集 {{ report.coverage.opportunity_symbols }} 标的 · AI picks
                  {{ report.coverage.ai_picks }}
                </span>
              </div>
              <div class="se-coverage-line">
                <span class="se-coverage-label">拒选覆盖</span>
                <span class="qv-tnum">
                  AI 零 picks {{ report.coverage.zero_pick_batches }} 批（{{ report.coverage.zero_pick_rate_pct.toFixed(2) }}%）
                </span>
              </div>
              <div class="se-coverage-line">
                <span class="se-coverage-label">事实剔除</span>
                <span class="qv-tnum">
                  degraded {{ report.coverage.degraded_excluded }} · facts 缺失 {{ report.coverage.facts_missing_excluded }} · events 缺失
                  {{ report.coverage.events_missing_excluded }} · 旧排名/顺序 {{ report.coverage.ranking_excluded }} · 重复事实
                  {{ report.coverage.duplicate_facts_excluded }} · picks 不一致 {{ report.coverage.pick_mismatch_excluded }}
                </span>
              </div>
              <div class="se-coverage-line">
                <span class="se-coverage-label">Outcome</span>
                <span class="qv-tnum">
                  共 {{ report.coverage.outcome_rows }} 行 · 成熟 {{ report.coverage.outcome_matured }} · pending
                  {{ report.coverage.outcome_pending }} · skipped {{ report.coverage.outcome_skipped }} · no_data
                  {{ report.coverage.outcome_no_data }} · forced {{ report.coverage.outcome_forced }}
                </span>
              </div>
              <div class="se-coverage-line">
                <span class="se-coverage-label">影子实验</span>
                <span class="qv-tnum">
                  runs {{ report.coverage.challenger_runs }} · valid ep1 输出 {{ report.coverage.challenger_valid_runs }} · 无效/不完整
                  {{ report.coverage.challenger_invalid_runs }} · 零 picks {{ report.coverage.challenger_zero_pick_runs }}
                </span>
              </div>
            </div>

            <div class="se-notes">
              <div v-for="(note, i) in report.notes || []" :key="i">{{ note }}</div>
            </div>
          </template>
          <div v-else-if="!loading" class="se-empty">
            暂无缓存报表。点「重新计算」推进 fixed-hold outcome 并生成统计；该过程不会调用 LLM。
          </div>
        </n-spin>
      </SectionCard>

      <template v-if="report">
        <SectionCard
          v-for="sec in report.sections || []"
          :key="`${sec.rec_type}-${sec.horizon_days}`"
          :title="sectionTitle(sec)"
        >
          <div class="se-section-coverage">
            <n-tag size="small" :type="sec.coverage.comparable_batches ? 'success' : 'warning'" :bordered="false">
              可比 {{ sec.coverage.comparable_batches }} / {{ sec.coverage.candidate_batches }} 批（{{ sec.coverage.coverage_pct.toFixed(2) }}%）
            </n-tag>
            <span class="qv-tnum">
              剔除：缺结果 {{ sec.coverage.missing_excluded }} · pending {{ sec.coverage.pending_excluded }} · skipped
              {{ sec.coverage.skipped_excluded }} · no_data {{ sec.coverage.no_data_excluded }} · forced
              {{ sec.coverage.forced_excluded }}
            </span>
          </div>

          <div class="se-sub">Selection：AI 最终 picks vs 同批 Quant Top-N</div>
          <n-data-table
            :columns="metricColumns"
            :data="sec.groups || []"
            :row-key="(r: SelectionMetric) => r.group"
            size="small"
            :scroll-x="1580"
          />
          <div v-if="!hasEvaluatedMetric(sec.groups)" class="se-inline-empty">暂无成熟且非 forced 的同批可比样本。</div>

          <div class="se-sub">Selection 逐批配对差与 95% CI（左组 - 右组）</div>
          <n-data-table
            v-if="comparablePairs(sec.pairs).length"
            :columns="pairColumns"
            :data="comparablePairs(sec.pairs)"
            :row-key="(r: SelectionPairedRow) => r.pair"
            size="small"
            :scroll-x="1890"
          />
          <div v-else class="se-inline-empty">暂无可用于 paired bootstrap 的批次。</div>

          <div class="se-sub">批次选择与剔除核对</div>
          <n-data-table
            v-if="sec.batches?.length"
            :columns="batchColumns"
            :data="sec.batches"
            :row-key="(r: SelectionBatchView) => r.batch_id"
            size="small"
            :scroll-x="1160"
            :max-height="360"
          />
          <div v-else class="se-inline-empty">暂无 AI 非零 picks 的完整事实批次。</div>

          <div class="se-layer">
            <div class="se-sub">Action / Veto（独立 fixed-hold 统计）</div>
            <div v-if="sec.action_transitions?.length" class="se-transition-row">
              <span class="se-transition-label">复核动作迁移</span>
              <n-tag
                v-for="t in sec.action_transitions"
                :key="`${t.from}-${t.to}`"
                size="small"
                :bordered="false"
              >
                {{ transitionLabel(t.from, t.to) }} · <span class="qv-tnum">{{ t.count }}</span>
              </n-tag>
            </div>
            <n-data-table
              :columns="metricColumns"
              :data="actionMetrics(sec)"
              :row-key="(r: SelectionMetric) => r.group"
              size="small"
              :scroll-x="1580"
            />
            <div v-if="!hasEvaluatedMetric(actionMetrics(sec))" class="se-inline-empty">暂无可评估的 action / veto outcome。</div>
          </div>

          <div class="se-layer">
            <div class="se-sub">Plan 辅助面板：同一 AI picks 的 l2 vs so1 fixed-hold</div>
            <div class="se-coverage-inline qv-tnum">
              同批交集 {{ sec.plan.coverage.comparable_batches }} / {{ sec.plan.coverage.candidate_batches }}（{{ sec.plan.coverage.coverage_pct.toFixed(2) }}%）·
              缺标签 {{ sec.plan.coverage.missing_excluded }} · pending {{ sec.plan.coverage.pending_excluded }} · skipped
              {{ sec.plan.coverage.skipped_excluded }} · no_data {{ sec.plan.coverage.no_data_excluded }} · forced
              {{ sec.plan.coverage.forced_excluded }}
            </div>
            <n-data-table
              :columns="metricColumns"
              :data="planMetrics(sec)"
              :row-key="(r: SelectionMetric) => r.group"
              size="small"
              :scroll-x="1580"
            />
            <n-data-table
              v-if="sec.plan.pair.batches > 0"
              class="se-pair-table"
              :columns="pairColumns"
              :data="[sec.plan.pair]"
              :row-key="(r: SelectionPairedRow) => r.pair"
              size="small"
              :scroll-x="1890"
            />
            <div v-else class="se-inline-empty">暂无 l2 与 so1 同时成熟的同一 picks 样本。</div>
            <div class="se-notes se-notes-compact">
              <div v-for="(note, i) in sec.plan.notes || []" :key="i">{{ note }}</div>
            </div>
          </div>

          <div class="se-layer">
            <div class="se-sub">Prompt challenger（旧记录缺 experiment_type 时按此兼容）</div>
            <template v-if="promptChallengers(sec).length">
              <div v-for="challenger in promptChallengers(sec)" :key="challenger.experiment_id" class="se-challenger">
                <div class="se-challenger-head">
                  <span class="se-challenger-title">{{ challenger.name }}</span>
                  <n-tag size="small" :bordered="false">实验 #{{ challenger.experiment_id }}</n-tag>
                  <n-tag size="small" :bordered="false">ep1 输出</n-tag>
                </div>
                <div class="se-coverage-inline qv-tnum">
                  runs {{ challenger.coverage.runs }} · 原生 K {{ challenger.coverage.native_k_min }} ~
                  {{ challenger.coverage.native_k_max }}（均值 {{ challenger.coverage.native_k_avg.toFixed(2) }}）· 原生可评
                  {{ challenger.coverage.native_eligible }} · matched-K 可评 {{ challenger.coverage.matched_eligible }} · outcome 剔除
                  {{ challenger.coverage.outcome_excluded }} · 无 matched 样本 {{ challenger.coverage.zero_matched }}
                </div>
                <n-data-table
                  :columns="metricColumns"
                  :data="challenger.groups || []"
                  :row-key="(r: SelectionMetric) => r.group"
                  size="small"
                  :scroll-x="1580"
                />
                <n-data-table
                  v-if="comparablePairs(challenger.pairs).length"
                  class="se-pair-table"
                  :columns="pairColumns"
                  :data="comparablePairs(challenger.pairs)"
                  :row-key="(r: SelectionPairedRow) => r.pair"
                  size="small"
                  :scroll-x="1890"
                />
                <div v-else class="se-inline-empty">该实验暂无 matched-K 配对批次。</div>
                <div class="se-notes se-notes-compact">
                  <div v-for="(note, i) in challenger.notes || []" :key="i">{{ note }}</div>
                </div>
              </div>
            </template>
            <div v-else class="se-inline-empty">暂无 valid 且逐标的 ep1 JSON 完整的 prompt challenger run。</div>
          </div>

          <div class="se-layer se-score-blind-layer">
            <div class="se-sub">Score-blind 输入实验</div>
            <div class="se-shadow-note">
              <n-tag size="small" type="warning" :bordered="false">纯影子、不影响推荐</n-tag>
              <span>独立输入实验分组；不属于 prompt challenger 晋级或发布审计路径。</span>
            </div>
            <template v-if="scoreBlindExperiments(sec).length">
              <div v-for="challenger in scoreBlindExperiments(sec)" :key="challenger.experiment_id" class="se-challenger">
                <div class="se-challenger-head">
                  <span class="se-challenger-title">{{ challenger.name }}</span>
                  <n-tag size="small" type="warning" :bordered="false">Score-blind</n-tag>
                  <n-tag size="small" :bordered="false">{{ challenger.input_schema_version || 'schema 缺失' }} 输入</n-tag>
                  <n-tag size="small" :bordered="false">ep1 输出</n-tag>
                  <n-tag size="small" :bordered="false">实验 #{{ challenger.experiment_id }}</n-tag>
                </div>
                <div v-if="challenger.protocol" class="se-protocol-block">
                  <div class="se-protocol-head">
                    <n-tag size="small" :type="protocolLockComplete(challenger) ? 'info' : 'error'" :bordered="false">
                      {{ protocolLockComplete(challenger) ? '锁定协议工件完整' : '协议工件不完整' }}
                    </n-tag>
                    <span>{{ protocolSummary(challenger.protocol) }}</span>
                  </div>
                  <div class="se-protocol-hash qv-mono">protocol hash {{ challenger.protocol_hash || '—' }}</div>
                </div>
                <n-alert v-else type="error" :show-icon="false" :bordered="false" class="se-protocol-alert">
                  锁定评价协议缺失。该 score-blind 分组不得据此形成实验结论。
                </n-alert>
                <div v-if="challenger.protocol_status" class="se-protocol-status">
                  <div class="se-protocol-head">
                    <n-tag size="small" :type="protocolReadinessType(challenger.protocol_status)" :bordered="false">
                      {{ challenger.protocol_status.ready ? '达到最小有效批次' : '样本积累中' }}
                    </n-tag>
                    <n-tag
                      size="small"
                      :type="protocolGuardrailType(challenger.protocol_status)"
                      :bordered="false"
                    >
                      {{ protocolGuardrailLabel(challenger.protocol_status) }}
                    </n-tag>
                    <span>
                      {{ challenger.protocol_status.window_group }} {{ challenger.protocol_status.horizon_days }} 日 ·
                      有效批次 {{ challenger.protocol_status.effective_batches }}/{{ challenger.protocol_status.min_effective_batches }}
                    </span>
                  </div>
                  <div class="se-protocol-metrics qv-tnum">
                    覆盖率 champion {{ challenger.protocol_status.champion_coverage_pct.toFixed(2) }}% / score-blind
                    {{ challenger.protocol_status.score_blind_coverage_pct.toFixed(2) }}% · 下降
                    {{ challenger.protocol_status.coverage_drop_pct.toFixed(2) }} / 上限
                    {{ challenger.protocol_status.max_coverage_drop_pct.toFixed(2) }} pt · 严重亏损率
                    {{ challenger.protocol_status.severe_loss_rate_pct.toFixed(2) }}% / 上限
                    {{ challenger.protocol_status.max_severe_loss_rate_pct.toFixed(2) }}% ·
                    预注册 {{ multipleTestingLabel(challenger.protocol_status.multiple_testing_method) }} · 检验族
                    {{ challenger.protocol_status.multiple_testing_family }} 个窗口 ·
                    {{ challenger.protocol_status.multiple_testing_applied ? '已执行校正' : '未作显著性检验' }}
                  </div>
                  <div v-if="challenger.protocol_status.note" class="se-protocol-note">{{ challenger.protocol_status.note }}</div>
                </div>
                <n-alert v-else type="error" :show-icon="false" :bordered="false" class="se-protocol-alert">
                  协议评估状态缺失。管理员记录的 improved 不是协议通过结论，本页不作正向判定。
                </n-alert>
                <div class="se-coverage-inline qv-tnum">
                  runs {{ challenger.coverage.runs }} · 原生 K {{ challenger.coverage.native_k_min }} ~
                  {{ challenger.coverage.native_k_max }}（均值 {{ challenger.coverage.native_k_avg.toFixed(2) }}）· 原生可评
                  {{ challenger.coverage.native_eligible }} · matched-K 可评 {{ challenger.coverage.matched_eligible }} · outcome 剔除
                  {{ challenger.coverage.outcome_excluded }} · 无 matched 样本 {{ challenger.coverage.zero_matched }}
                </div>
                <div class="se-attempt-coverage">
                  <span>协议终态尝试 {{ challenger.coverage.runs }} 批：</span>
                  <n-tag size="small" type="error" :bordered="false">调用失败 {{ challenger.coverage.failed_runs }}</n-tag>
                  <n-tag size="small" type="error" :bordered="false">越池 {{ challenger.coverage.out_of_pool_runs }}</n-tag>
                  <n-tag size="small" type="warning" :bordered="false">收益指标剔除 {{ challenger.coverage.metric_excluded }}</n-tag>
                  <span>失败与越池保留在协议覆盖率分母，不进入收益指标。</span>
                </div>
                <n-data-table
                  :columns="metricColumns"
                  :data="challenger.groups || []"
                  :row-key="(r: SelectionMetric) => r.group"
                  size="small"
                  :scroll-x="1580"
                />
                <n-data-table
                  v-if="comparablePairs(challenger.pairs).length"
                  class="se-pair-table"
                  :columns="pairColumns"
                  :data="comparablePairs(challenger.pairs)"
                  :row-key="(r: SelectionPairedRow) => r.pair"
                  size="small"
                  :scroll-x="1890"
                />
                <div v-else class="se-inline-empty">该 score-blind 实验暂无 matched-K 配对批次。</div>
                <div class="se-notes se-notes-compact">
                  <div v-for="(note, i) in challenger.notes || []" :key="i">{{ note }}</div>
                </div>
              </div>
            </template>
            <div v-else class="se-inline-empty">暂无可评估的 score-blind 影子事实。</div>
          </div>

          <n-alert
            v-if="unknownChallengers(sec).length"
            type="error"
            :show-icon="false"
            :bordered="false"
            class="se-unknown-alert"
          >
            检测到当前前端不认识的实验类型：
            <span v-for="(challenger, i) in unknownChallengers(sec)" :key="challenger.experiment_id">
              {{ i ? '；' : '' }}#{{ challenger.experiment_id }} {{ challenger.experiment_type }}
            </span>。
            已 fail-closed：不归入 Prompt challenger 或 Score-blind 分组，也不展示为可采信的配对结论。
          </n-alert>

          <div class="se-layer">
            <div class="se-sub">AI - Quant 分层检查</div>
            <template v-if="sec.slices?.some((slice) => slice.rows?.length)">
              <div v-for="slice in sec.slices" :key="slice.dim" class="se-slice">
                <div class="se-slice-title">{{ slice.label }}</div>
                <n-data-table
                  v-if="slice.rows?.length"
                  :columns="sliceColumns"
                  :data="slice.rows"
                  :row-key="(r: SelectionSliceRow) => r.key"
                  size="small"
                  :scroll-x="850"
                />
                <div v-else class="se-inline-empty">该维度暂无样本。</div>
              </div>
            </template>
            <div v-else class="se-inline-empty">暂无可分层的配对批次；样本不足时不输出方向性结论。</div>
          </div>

          <div class="se-notes">
            <div v-for="(note, i) in sec.notes || []" :key="i">{{ note }}</div>
          </div>
        </SectionCard>
      </template>
    </div>
  </PageContainer>
</template>

<style scoped>
.se-wrap {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.se-toolbar {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  flex-wrap: wrap;
}
.se-meta {
  font-size: 12px;
  opacity: 0.6;
}
.se-version-row,
.se-section-coverage,
.se-transition-row,
.se-challenger-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.se-version-row {
  margin-bottom: 12px;
}
.se-coverage-grid {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.se-coverage-line {
  display: flex;
  align-items: baseline;
  gap: 12px;
  font-size: 12px;
  line-height: 1.7;
}
.se-coverage-label {
  flex: 0 0 76px;
  font-weight: 600;
  opacity: 0.82;
}
.se-section-coverage {
  margin-bottom: 8px;
  font-size: 12px;
  opacity: 0.72;
}
.se-sub {
  margin: 14px 0 8px;
  font-size: 13px;
  font-weight: 600;
}
.se-layer {
  margin-top: 20px;
  padding-top: 2px;
  border-top: 1px dashed var(--qv-border, rgba(128, 128, 128, 0.22));
}
.se-label-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.se-ci,
.se-symbols,
.se-picks {
  white-space: nowrap;
}
.se-diff-expand {
  padding: 6px 0 8px;
}
.se-expand-title {
  margin-bottom: 7px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.72;
}
.se-inline-empty,
.se-empty {
  padding: 14px 0;
  font-size: 12px;
  opacity: 0.58;
}
.se-empty {
  padding: 24px 0;
  font-size: 13px;
}
.se-transition-row {
  margin: 0 0 8px;
}
.se-shadow-note {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
  font-size: 12px;
  opacity: 0.82;
}
.se-score-blind-layer {
  border-top-style: solid;
}
.se-protocol-block,
.se-protocol-status {
  margin: 8px 0;
  padding: 8px 10px;
  border-left: 3px solid var(--qv-border, rgba(128, 128, 128, 0.28));
  background: rgba(128, 128, 128, 0.06);
  font-size: 12px;
  overflow-wrap: anywhere;
}
.se-protocol-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  line-height: 1.6;
}
.se-protocol-hash,
.se-protocol-metrics,
.se-protocol-note {
  margin-top: 5px;
  line-height: 1.6;
}
.se-protocol-note {
  opacity: 0.7;
}
.se-protocol-alert,
.se-unknown-alert {
  margin: 8px 0;
}
.se-attempt-coverage {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin: 8px 0 10px;
  font-size: 12px;
  line-height: 1.7;
}
.se-unknown-alert {
  margin-top: 18px;
  overflow-wrap: anywhere;
}
.se-transition-label {
  font-size: 12px;
  opacity: 0.65;
}
.se-coverage-inline {
  margin: 0 0 8px;
  font-size: 12px;
  line-height: 1.7;
  opacity: 0.68;
}
.se-pair-table {
  margin-top: 10px;
}
.se-challenger + .se-challenger {
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px dashed var(--qv-border, rgba(128, 128, 128, 0.2));
}
.se-challenger-head {
  margin-bottom: 5px;
}
.se-challenger-title {
  font-size: 13px;
  font-weight: 600;
}
.se-slice + .se-slice {
  margin-top: 12px;
}
.se-slice-title {
  margin-bottom: 6px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.76;
}
.se-notes {
  margin-top: 12px;
  font-size: 12px;
  line-height: 1.8;
  opacity: 0.56;
}
.se-notes-compact {
  margin-top: 7px;
}

@media (max-width: 768px) {
  .se-toolbar {
    align-items: flex-start;
    justify-content: flex-start;
  }
  .se-meta {
    width: 100%;
  }
  .se-coverage-line {
    display: block;
  }
  .se-coverage-label {
    display: block;
    margin-bottom: 1px;
  }
  .se-section-coverage {
    align-items: flex-start;
  }
}
</style>
