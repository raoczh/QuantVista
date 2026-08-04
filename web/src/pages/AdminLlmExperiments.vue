<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import {
  NAlert, NButton, NCollapse, NCollapseItem, NForm, NFormItem, NInput, NInputNumber,
  NModal, NRadioButton, NRadioGroup, NSelect, NSpin, NTag, useDialog, useMessage,
} from 'naive-ui'
import {
  actLLMExperiment, auditLLMExperiment, createLLMExperiment, getLLMExperiment, listLLMExperiments,
  type LLMExperiment, type LLMExperimentActual, type LLMExperimentInput, type LLMExperimentRun,
  type LLMExperimentProtocol, type LLMExperimentType, type LLMReleaseAudit, type LLMReleaseAuditFinding,
} from '@/api/admin'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'

const message = useMessage()
const dialog = useDialog()
const { vars } = useUi()

const rows = ref<LLMExperiment[]>([])
const loading = ref(false)

const STATUS_LABEL: Record<string, string> = {
  draft: '草稿', running: '采样中', completed: '已完成', promoted: '已晋级', abandoned: '已废弃', rolled_back: '已回滚',
}
const STATUS_TYPE: Record<string, 'default' | 'info' | 'success' | 'warning' | 'error'> = {
  draft: 'default', running: 'info', completed: 'warning', promoted: 'success', abandoned: 'error', rolled_back: 'error',
}
const CONCLUDE_LABEL: Record<string, string> = {
  improved: '有增量', no_improvement: '无增量', worse: '劣化',
}
const RUN_STATUS_LABEL: Record<string, string> = {
  running: '运行中', success: '成功', failed: '失败', empty_picks: '空 picks', out_of_pool: '越池', invalid: '无效',
}
const MULTIPLE_TESTING_OPTIONS = [
  { label: 'Holm-Bonferroni', value: 'holm_bonferroni' },
  { label: 'Bonferroni', value: 'bonferroni' },
  { label: 'Benjamini-Hochberg', value: 'benjamini_hochberg' },
]

function experimentType(exp: Pick<LLMExperiment, 'experiment_type'>): string {
  return exp.experiment_type || 'prompt'
}

function isScoreBlind(exp: Pick<LLMExperiment, 'experiment_type'>): boolean {
  return experimentType(exp) === 'score_blind'
}

function isPromptExperiment(exp: Pick<LLMExperiment, 'experiment_type'>): boolean {
  return experimentType(exp) === 'prompt'
}

function isKnownExperiment(exp: Pick<LLMExperiment, 'experiment_type'>): boolean {
  return isPromptExperiment(exp) || isScoreBlind(exp)
}

function scoreBlindProtocolIssue(exp: LLMExperiment): string {
  if (!isScoreBlind(exp)) return ''
  if (exp.champion_custom) return 'score-blind 仅支持默认推荐任务段，当前 champion 为自定义任务段'
  if (exp.input_schema_version !== 'sb1') return `不支持的输入 schema：${exp.input_schema_version || '缺失'}`
  if (!exp.protocol_hash) return 'protocol hash 缺失'
  if (!exp.protocol_locked_at) return '协议尚未锁定'
  const protocol = parseProtocol(exp)
  if (!protocol) return '协议工件不可解析或字段不完整'
  if (protocol.short_horizons.join(',') !== '5,10' || protocol.long_horizons.join(',') !== '20,60') {
    return '评价窗口不是锁定的 5/10 与 20/60 日'
  }
  if (protocol.min_effective_batches <= 0 || protocol.max_coverage_drop_pct <= 0 ||
    protocol.max_coverage_drop_pct > 100 || protocol.max_severe_loss_rate_pct <= 0 ||
    protocol.max_severe_loss_rate_pct > 100 ||
    !MULTIPLE_TESTING_OPTIONS.some((item) => item.value === protocol.multiple_testing_method)) {
    return '协议阈值或多重检验方法非法'
  }
  if (exp.sample_target < protocol.min_effective_batches * 2) {
    return `采样总目标至少须为每类最小有效批次数的 2 倍（至少 ${protocol.min_effective_batches * 2}）`
  }
  return ''
}

function conclusionLabel(exp: LLMExperiment): string {
  const label = CONCLUDE_LABEL[exp.conclusion] || exp.conclusion
  if (isScoreBlind(exp) && exp.conclusion) {
    return `管理员记录：${exp.conclusion === 'improved' ? '倾向有增量' : label}（非协议判定）`
  }
  if (!isKnownExperiment(exp) && exp.conclusion) return `未知类型记录：${label}`
  return label
}

function conclusionType(exp: LLMExperiment): 'default' | 'info' | 'success' | 'warning' | 'error' {
  if (!isKnownExperiment(exp)) return 'error'
  if (isScoreBlind(exp)) return 'info'
  return exp.conclusion === 'improved' ? 'success' : 'warning'
}

async function load() {
  loading.value = true
  try {
    rows.value = await listLLMExperiments()
    const loadedIDs = Object.keys(detailRuns).map(Number)
    await Promise.all(loadedIDs.map((id) => loadDetail(id, true)))
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function parseActual(exp: LLMExperiment): LLMExperimentActual | null {
  if (!exp.actual_json) return null
  try {
    return JSON.parse(exp.actual_json) as LLMExperimentActual
  } catch {
    return null
  }
}

function parseProtocol(exp: LLMExperiment): LLMExperimentProtocol | null {
  if (!exp.protocol_json) return null
  try {
    const value = JSON.parse(exp.protocol_json) as Partial<LLMExperimentProtocol> | null
    if (!value || !Array.isArray(value.short_horizons) || !Array.isArray(value.long_horizons) ||
      !value.short_horizons.every((item) => typeof item === 'number' && Number.isFinite(item)) ||
      !value.long_horizons.every((item) => typeof item === 'number' && Number.isFinite(item)) ||
      typeof value.min_effective_batches !== 'number' || !Number.isFinite(value.min_effective_batches) ||
      typeof value.max_coverage_drop_pct !== 'number' || !Number.isFinite(value.max_coverage_drop_pct) ||
      typeof value.max_severe_loss_rate_pct !== 'number' || !Number.isFinite(value.max_severe_loss_rate_pct) ||
      typeof value.multiple_testing_method !== 'string') {
      return null
    }
    return value as LLMExperimentProtocol
  } catch {
    return null
  }
}

function formatProtocol(exp: LLMExperiment): string {
  const protocol = parseProtocol(exp)
  if (!protocol) return '协议工件不可解析'
  const severeLossDefinition = protocol.severe_loss_definition_pct ?? -5
  return [
    `短线 ${protocol.short_horizons.join('/')} 日`,
    `长线 ${protocol.long_horizons.join('/')} 日`,
    `每类最小有效批次 ${protocol.min_effective_batches}`,
    `覆盖率下降上限 ${protocol.max_coverage_drop_pct}%`,
    `严重亏损率上限 ${protocol.max_severe_loss_rate_pct}%（net<${severeLossDefinition}%）`,
    `多重检验 ${protocol.multiple_testing_method}`,
  ].join(' · ')
}

function runStatusType(run: LLMExperimentRun): 'default' | 'info' | 'success' | 'warning' | 'error' {
  switch (run.run_status) {
    case 'running': return 'info'
    case 'success': return 'success'
    case 'empty_picks': return 'warning'
    case 'failed':
    case 'out_of_pool': return 'error'
    default: return run.valid ? 'success' : 'error'
  }
}

function runStatusLabel(run: LLMExperimentRun): string {
  return RUN_STATUS_LABEL[run.run_status || ''] || run.run_status || (run.valid ? '成功' : '失败')
}

function runTypeLabel(run: LLMExperimentRun, exp: LLMExperiment): string {
  if (run.experiment_type) return run.experiment_type
  return isPromptExperiment(exp) ? 'prompt（旧 ep1）' : '类型缺失'
}

function runTypeMatches(run: LLMExperimentRun, exp: LLMExperiment): boolean {
  if (!isKnownExperiment(exp)) return false
  if (!run.experiment_type) return isPromptExperiment(exp)
  if (run.experiment_type !== 'prompt' && run.experiment_type !== 'score_blind') return false
  return run.experiment_type === experimentType(exp)
}

function runStatusSummary(runs: LLMExperimentRun[]): string {
  const counts = new Map<string, number>()
  for (const run of runs) {
    const status = run.run_status || (run.valid ? 'success' : 'failed')
    counts.set(status, (counts.get(status) || 0) + 1)
  }
  return ['running', 'success', 'empty_picks', 'failed', 'out_of_pool']
    .filter((status) => counts.has(status))
    .map((status) => `${RUN_STATUS_LABEL[status] || status} ${counts.get(status)}`)
    .join(' · ')
}

function formatInputOrder(run: LLMExperimentRun): string {
  if (!run.input_order_json) return '—'
  try {
    const order = JSON.parse(run.input_order_json) as unknown
    if (Array.isArray(order)) return order.map(String).join(', ')
  } catch {
    // 旧记录或异常工件保留原文展示，不能事后重建。
  }
  return run.input_order_json
}

/* 明细（影子样本 + 发布审计工件） */
const detailRuns = reactive<Record<number, LLMExperimentRun[]>>({})
const detailAudits = reactive<Record<number, LLMReleaseAudit[]>>({})
async function loadDetail(id: number, force = false) {
  if (detailRuns[id] && !force) return
  try {
    const res = await getLLMExperiment(id)
    detailRuns[id] = res.runs
    detailAudits[id] = res.audits ?? []
  } catch (e) {
    message.error((e as Error).message)
  }
}

function auditFindings(a: LLMReleaseAudit): LLMReleaseAuditFinding[] {
  if (!a.findings_json) return []
  try {
    return (JSON.parse(a.findings_json) as LLMReleaseAuditFinding[]) ?? []
  } catch {
    return []
  }
}
const AUDIT_TYPE: Record<string, 'success' | 'error' | 'warning'> = { pass: 'success', fail: 'error', error: 'warning' }
const AUDIT_LABEL: Record<string, string> = { pass: 'PASS', fail: 'FAIL', error: '未判定' }

/* P2-6 发布审计（LLM 只复核程序硬检覆盖不了的缺口；一次真实 LLM 调用） */
const auditing = ref(false)
async function runAudit(exp: LLMExperiment) {
  if (!isPromptExperiment(exp)) {
    message.error('仅已识别的 prompt challenger 可以执行发布审计')
    return
  }
  auditing.value = true
  try {
    const a = await auditLLMExperiment(exp.id)
    message.success(`发布审计完成：${AUDIT_LABEL[a.verdict] || a.verdict}`)
    await loadDetail(exp.id, true)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    auditing.value = false
  }
}

function onHeaderClick(data: { name: string | number }) {
  void loadDetail(Number(data.name))
}

/* 动作 */
const acting = ref(false)
async function act(exp: LLMExperiment, action: 'start' | 'promote' | 'rollback' | 'abandon') {
  if (!isKnownExperiment(exp)) {
    message.error(`未知实验类型 ${experimentType(exp)}，前端已拒绝执行状态变更`)
    return
  }
  if ((action === 'promote' || action === 'rollback') && !isPromptExperiment(exp)) {
    message.error('score-blind 输入实验不能进入 prompt 晋级或回滚路径')
    return
  }
  if (action === 'start') {
    const protocolIssue = scoreBlindProtocolIssue(exp)
    if (protocolIssue) {
      message.error(`score-blind 启动条件不完整：${protocolIssue}`)
      return
    }
  }
  acting.value = true
  try {
    await actLLMExperiment(exp.id, action)
    message.success('已执行')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    acting.value = false
  }
}

function confirmPromote(exp: LLMExperiment) {
  if (!isPromptExperiment(exp)) {
    message.error('仅已识别的 prompt challenger 可以晋级 champion')
    return
  }
  dialog.warning({
    title: '晋级为 champion',
    content: '将把 challenger 内容落为该模块启用中的自定义模板（生成新的不可变 revision 快照并切换指针）。发布质量门（样本量/结构化有效率/结论/内容 hash/发布审计 PASS）不通过会被拒绝。回滚 = 本页「一键切回 champion」。',
    positiveText: '晋级',
    negativeText: '取消',
    onPositiveClick: () => act(exp, 'promote'),
  })
}

function confirmRollback(exp: LLMExperiment) {
  if (!isPromptExperiment(exp)) {
    message.error('仅已识别的 prompt challenger 可以回滚 champion')
    return
  }
  dialog.warning({
    title: '一键切回 champion',
    content: '将恢复晋级前的模板状态（晋级前有自定义模板则恢复其内容并生成新 revision；晋级前为默认模板则停用当前自定义模板）。实验进入「已回滚」终态，工件全部保留。',
    positiveText: '回滚',
    negativeText: '取消',
    onPositiveClick: () => act(exp, 'rollback'),
  })
}

/* 完成（P2-2 结论与失败原因） */
const completeTarget = ref<LLMExperiment | null>(null)
const completeForm = reactive({ conclusion: 'no_improvement', failure_reason: '' })
function openComplete(exp: LLMExperiment) {
  if (!isKnownExperiment(exp)) {
    message.error(`未知实验类型 ${experimentType(exp)}，前端已拒绝完成操作`)
    return
  }
  completeForm.conclusion = 'no_improvement'
  completeForm.failure_reason = ''
  completeTarget.value = exp
}

async function submitComplete() {
  const target = completeTarget.value
  if (!target) return
  if (!isKnownExperiment(target)) {
    message.error(`未知实验类型 ${experimentType(target)}，前端已拒绝完成操作`)
    completeTarget.value = null
    return
  }
  acting.value = true
  try {
    await actLLMExperiment(target.id, 'complete', {
      conclusion: completeForm.conclusion,
      failure_reason: completeForm.failure_reason.trim(),
    })
    message.success(isScoreBlind(target)
      ? 'score-blind 管理员结论已记录（非协议判定）'
      : '实验已完成（聚合报表见卡片）')
    completeTarget.value = null
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    acting.value = false
  }
}

/* 创建 */
const showCreate = ref(false)
const creating = ref(false)
const createForm = reactive({
  module: 'recommendation', experiment_type: 'prompt' as LLMExperimentType,
  name: '', hypothesis: '', expected_improvement: '', challenger_content: '',
  sample_target: 20, parent_id: 0,
  min_effective_batches: null as number | null,
  max_coverage_drop_pct: null as number | null,
  max_severe_loss_rate_pct: null as number | null,
  multiple_testing_method: '',
})
const parentOptions = computed(() =>
	rows.value.filter(isPromptExperiment).map((r) => ({ label: `#${r.id} ${r.name}`, value: r.id })))
const scoreBlindSampleTargetMin = computed(() => Math.max(5, (createForm.min_effective_batches ?? 1) * 2))
async function submitCreate() {
  const scoreBlind = createForm.experiment_type === 'score_blind'
  if (scoreBlind && (
    createForm.min_effective_batches === null || createForm.max_coverage_drop_pct === null ||
    createForm.max_severe_loss_rate_pct === null || createForm.max_coverage_drop_pct <= 0 ||
    createForm.max_coverage_drop_pct > 100 || createForm.max_severe_loss_rate_pct <= 0 ||
    createForm.max_severe_loss_rate_pct > 100 || !createForm.multiple_testing_method.trim() ||
    createForm.sample_target < createForm.min_effective_batches * 2
  )) {
    message.warning('score-blind 必须完整填写协议，且采样总目标至少为每类最小有效批次数的 2 倍')
    return
  }
  const input: LLMExperimentInput = {
    module: createForm.module,
    experiment_type: createForm.experiment_type,
    name: createForm.name,
    hypothesis: createForm.hypothesis,
    expected_improvement: createForm.expected_improvement,
    sample_target: createForm.sample_target,
  }
  if (scoreBlind) {
    input.protocol = {
      short_horizons: [5, 10],
      long_horizons: [20, 60],
      min_effective_batches: createForm.min_effective_batches!,
      max_coverage_drop_pct: createForm.max_coverage_drop_pct!,
      max_severe_loss_rate_pct: createForm.max_severe_loss_rate_pct!,
      multiple_testing_method: createForm.multiple_testing_method.trim(),
    }
  } else {
    input.challenger_content = createForm.challenger_content
    input.parent_id = createForm.parent_id || 0
  }
  creating.value = true
  try {
    const res = await createLLMExperiment(input)
    res.warnings?.forEach((w) => message.warning(w))
    message.success(`实验 #${res.experiment.id} 已创建（draft）`)
    showCreate.value = false
    createForm.name = ''
    createForm.hypothesis = ''
    createForm.expected_improvement = ''
    createForm.challenger_content = ''
    createForm.parent_id = 0
    createForm.min_effective_batches = null
    createForm.max_coverage_drop_pct = null
    createForm.max_severe_loss_rate_pct = null
    createForm.multiple_testing_method = ''
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <PageContainer
    title="推荐影子实验"
    subtitle="Prompt challenger 与 S3-6C score-blind 输入实验统一调度；每批最多一次额外调用，影子输出不改业务推荐"
  >
    <div class="exp-wrap">
      <SectionCard title="实验列表">
        <template #extra>
          <div class="exp-toolbar">
            <n-button size="small" @click="load">刷新</n-button>
            <n-button size="small" type="primary" @click="showCreate = true">新建实验</n-button>
          </div>
        </template>
        <n-spin :show="loading">
          <n-collapse v-if="rows.length" display-directive="show" @item-header-click="onHeaderClick">
            <n-collapse-item v-for="r in rows" :key="r.id" :name="r.id">
              <template #header>
                <span class="exp-head">
                  <code class="exp-id">#{{ r.id }}</code>
                  <span class="exp-name">{{ r.name }}</span>
                  <n-tag size="tiny" :bordered="false" round :type="STATUS_TYPE[r.status] || 'default'">{{ STATUS_LABEL[r.status] || r.status }}</n-tag>
                  <n-tag size="tiny" :bordered="false" round :type="isScoreBlind(r) ? 'warning' : (isPromptExperiment(r) ? 'default' : 'error')">
                    {{ isScoreBlind(r) ? `Score-blind ${r.input_schema_version || 'schema 缺失'}` : (isPromptExperiment(r) ? 'Prompt challenger' : `未知类型 ${experimentType(r)}`) }}
                  </n-tag>
                  <n-tag size="tiny" :bordered="false" round>{{ r.module }}</n-tag>
                  <span class="exp-sub">采样 {{ r.sample_count }}/{{ r.sample_target }}</span>
                  <n-tag v-if="r.conclusion" size="tiny" :bordered="false" round :type="conclusionType(r)">
                    {{ conclusionLabel(r) }}
                  </n-tag>
                  <span v-if="r.parent_id" class="exp-sub">← 父实验 #{{ r.parent_id }}</span>
                </span>
              </template>
              <div class="exp-body">
                <n-alert v-if="isScoreBlind(r)" type="warning" :show-icon="false" :bordered="false" class="exp-shadow-alert">
                  纯影子、不影响推荐。该输入实验不属于 prompt 晋级路径，不能发布审计、晋级或回滚为 champion。
                </n-alert>
                <n-alert v-else-if="!isKnownExperiment(r)" type="error" :show-icon="false" :bordered="false" class="exp-shadow-alert">
                  未知实验类型 {{ experimentType(r) }}。当前管理端已 fail-closed：不执行启动、完成、废弃、发布审计、晋级或回滚，也不将它兼容成 prompt 实验。
                </n-alert>
                <div class="exp-row"><span class="exp-k">假设</span><span>{{ r.hypothesis }}</span></div>
                <div class="exp-row"><span class="exp-k">预期改善</span><span>{{ r.expected_improvement }}</span></div>
                <div v-if="isScoreBlind(r)" class="exp-row">
                  <span class="exp-k">输入工件</span>
                  <span>
                    schema {{ r.input_schema_version || '缺失' }} · protocol hash
                    <span class="qv-mono">{{ r.protocol_hash || '—' }}</span> · 锁定于 {{ r.protocol_locked_at || '—' }}
                  </span>
                </div>
                <div v-if="isScoreBlind(r)" class="exp-row">
                  <span class="exp-k">锁定评价协议</span><span>{{ formatProtocol(r) }}</span>
                </div>
                <div v-if="isScoreBlind(r) && scoreBlindProtocolIssue(r)" class="exp-row exp-row-warning">
                  <span class="exp-k">协议异常</span><span>{{ scoreBlindProtocolIssue(r) }}（启动已禁用）</span>
                </div>
                <div v-if="isPromptExperiment(r)" class="exp-row">
                  <span class="exp-k">对照锚</span>
                  <span>champion={{ r.champion_version }} · challenger hash {{ r.challenger_hash.slice(0, 8) }}<template v-if="r.promoted_revision"> · 晋级 revision {{ r.promoted_revision }}</template></span>
                </div>
                <div v-if="isPromptExperiment(r)" class="exp-row"><span class="exp-k">challenger 任务段</span><span class="exp-content">{{ r.challenger_content }}</span></div>
                <div v-if="r.failure_reason" class="exp-row"><span class="exp-k">失败原因</span><span>{{ r.failure_reason }}</span></div>
                <div v-if="isPromptExperiment(r) && r.baseline_stale" class="exp-row exp-row-warning">
                  <span class="exp-k">基线已失效</span>
                  <span>{{ r.baseline_stale }}（该实验不可再启动、审计或晋级，请基于当前 champion 新建实验）</span>
                </div>
                <div v-if="isPromptExperiment(r) && r.rollback_stale" class="exp-row"><span class="exp-k">回滚不可用</span><span>{{ r.rollback_stale }}（如需恢复历史内容请在提示词页按 revision 快照操作）</span></div>
                <div v-if="parseActual(r)" class="exp-row">
                  <span class="exp-k">实际结果</span>
                  <span>
                    样本 {{ parseActual(r)!.samples }} · 结构化有效率 {{ parseActual(r)!.valid_rate_pct }}% ·
                    picks 均值 champion {{ parseActual(r)!.avg_champion_picks }} / challenger {{ parseActual(r)!.avg_challenger_picks }} ·
                    名单重合 {{ parseActual(r)!.avg_overlap_pct }}% ·
                    token 均值 {{ parseActual(r)!.avg_champion_tokens }} / {{ parseActual(r)!.avg_challenger_tokens }} ·
                    延迟均值 {{ parseActual(r)!.avg_champion_ms }} / {{ parseActual(r)!.avg_challenger_ms }} ms
                    <template v-if="parseActual(r)!.errors?.length"> · 失败样本：{{ parseActual(r)!.errors!.join('；') }}</template>
                  </span>
                </div>
                <div class="exp-actions">
                  <n-button
                    v-if="r.status === 'draft' && isKnownExperiment(r)"
                    size="tiny"
                    type="primary"
                    :loading="acting"
                    :disabled="(isPromptExperiment(r) && !!r.baseline_stale) || !!scoreBlindProtocolIssue(r)"
                    :title="isPromptExperiment(r) ? r.baseline_stale || undefined : scoreBlindProtocolIssue(r) || undefined"
                    @click="act(r, 'start')"
                  >启动采样</n-button>
                  <n-button v-if="isKnownExperiment(r) && r.status === 'running'" size="tiny" type="warning" :loading="acting" @click="openComplete(r)">完成实验</n-button>
                  <n-button v-if="isPromptExperiment(r) && r.status === 'completed'" size="tiny" type="info" :loading="auditing" :disabled="!!r.baseline_stale" :title="r.baseline_stale || undefined" @click="runAudit(r)">发布审计</n-button>
                  <n-button v-if="isPromptExperiment(r) && r.status === 'completed'" size="tiny" type="success" :loading="acting" :disabled="!!r.baseline_stale" :title="r.baseline_stale || undefined" @click="confirmPromote(r)">晋级 champion</n-button>
                  <n-button v-if="isPromptExperiment(r) && r.status === 'promoted'" size="tiny" type="error" :loading="acting" :disabled="!!r.rollback_stale" @click="confirmRollback(r)">一键切回 champion</n-button>
                  <n-button v-if="isKnownExperiment(r) && r.status !== 'promoted' && r.status !== 'abandoned' && r.status !== 'rolled_back'" size="tiny" :loading="acting" @click="act(r, 'abandon')">废弃</n-button>
                </div>
                <div v-if="isPromptExperiment(r) && detailAudits[r.id]?.length" class="exp-runs">
                  <div class="exp-runs-title">发布审计工件（{{ detailAudits[r.id].length }} 次，晋级门只认最新且要求内容 hash 匹配）</div>
                  <div v-for="a in detailAudits[r.id]" :key="a.id" class="exp-run-line">
                    <n-tag size="tiny" :bordered="false" round :type="AUDIT_TYPE[a.verdict] || 'default'">{{ AUDIT_LABEL[a.verdict] || a.verdict }}</n-tag>
                    {{ a.summary }}
                    <template v-if="auditFindings(a).length">
                      · 发现：<template v-for="(f, i) in auditFindings(a)" :key="i">{{ i > 0 ? '；' : '' }}[{{ f.severity }}] {{ f.code }} {{ f.message }}</template>
                    </template>
                    · trace {{ a.trace_id.slice(0, 10) }} · {{ a.tokens_used }} tok
                  </div>
                </div>
                <div v-if="detailRuns[r.id]?.length" class="exp-runs">
                  <div class="exp-runs-title">
                    影子样本（{{ detailRuns[r.id].length }} 条） · {{ runStatusSummary(detailRuns[r.id]) }}
                  </div>
                  <div v-for="run in detailRuns[r.id]" :key="run.id" class="exp-run-line">
                    <template v-if="isScoreBlind(r)">
                      批次 {{ run.batch_id }} ·
                      <n-tag size="tiny" :bordered="false" round :type="runStatusType(run)">{{ runStatusLabel(run) }}</n-tag> ·
                      <n-tag size="tiny" :bordered="false" round :type="runTypeMatches(run, r) ? 'default' : 'error'">{{ runTypeLabel(run, r) }}</n-tag> ·
                      schema {{ run.input_schema_version || '缺失' }} · seed <span class="qv-tnum">{{ run.seed ?? '—' }}</span> ·
                      input hash <span class="qv-mono">{{ run.input_hash || '—' }}</span> · 输入顺序
                      <span class="qv-mono">{{ formatInputOrder(run) }}</span> · ep1 picks score-blind/champion
                      {{ run.picks_count }}/{{ run.champion_picks }} · 重合 {{ run.overlap_count }} ·
                      token champion/score-blind {{ run.champion_tokens }}/{{ run.challenger_tokens }} ·
                      耗时 {{ run.champion_ms }}/{{ run.challenger_ms }} ms · finish {{ run.finish_state || '—' }}
                    </template>
                    <template v-else>
                      批次 {{ run.batch_id }} ·
                      <n-tag size="tiny" :bordered="false" round :type="runStatusType(run)">{{ runStatusLabel(run) }}</n-tag> ·
                      <n-tag size="tiny" :bordered="false" round :type="runTypeMatches(run, r) ? 'default' : 'error'">{{ runTypeLabel(run, r) }}</n-tag> ·
                      {{ run.pick_schema_version || 'ep1' }} · picks {{ run.picks_count }}/{{ run.champion_picks }} 重合
                      {{ run.overlap_count }} · token {{ run.champion_tokens }}/{{ run.challenger_tokens }} ·
                      耗时 {{ run.champion_ms }}/{{ run.challenger_ms }} ms · finish {{ run.finish_state || '—' }}
                    </template>
                    <template v-if="run.error"> · {{ run.error }}</template>
                  </div>
                </div>
              </div>
            </n-collapse-item>
          </n-collapse>
          <div v-else class="exp-empty">暂无实验。新建后由「推荐影子实验采样」总开关控制是否实际采样。</div>
        </n-spin>
      </SectionCard>
    </div>

    <n-modal v-model:show="showCreate" preset="card" title="新建推荐影子实验" class="exp-modal">
      <n-form label-placement="top">
        <n-form-item label="实验类型">
          <n-radio-group v-model:value="createForm.experiment_type">
            <n-radio-button value="prompt">Prompt challenger</n-radio-button>
            <n-radio-button value="score_blind">Score-blind 输入实验</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-alert v-if="createForm.experiment_type === 'score_blind'" type="warning" :show-icon="false" :bordered="false" class="exp-create-alert">
          纯影子、不影响推荐。仅支持默认推荐任务段；创建时锁定评价协议，之后不可修改。该类型不能进入 prompt 发布审计、晋级或回滚路径。
        </n-alert>
        <n-form-item label="名称"><n-input v-model:value="createForm.name" maxlength="60" /></n-form-item>
        <n-form-item label="假设（想验证什么）"><n-input v-model:value="createForm.hypothesis" type="textarea" :rows="2" maxlength="300" /></n-form-item>
        <n-form-item label="预期改善（对照什么指标）"><n-input v-model:value="createForm.expected_improvement" type="textarea" :rows="2" maxlength="300" /></n-form-item>
        <n-form-item v-if="createForm.experiment_type === 'prompt'" label="challenger 任务段（推荐模块 L3；系统契约段恒由程序追加）">
          <n-input v-model:value="createForm.challenger_content" type="textarea" :rows="6" maxlength="4000" />
        </n-form-item>
        <template v-else>
          <n-form-item label="锁定窗口">
            <div class="exp-protocol-fixed">
              <n-tag size="small" :bordered="false">短线 5 / 10 日</n-tag>
              <n-tag size="small" :bordered="false">长线 20 / 60 日</n-tag>
            </div>
          </n-form-item>
          <n-form-item label="每类最小有效批次数（短线/长线分别，必填）">
            <n-input-number v-model:value="createForm.min_effective_batches" :min="1" :max="50" />
          </n-form-item>
          <n-form-item label="覆盖率下降上限（%，必填）">
            <n-input-number v-model:value="createForm.max_coverage_drop_pct" :min="0.01" :max="100" :step="0.1" />
          </n-form-item>
          <n-form-item label="严重亏损率上限（%，net&lt;-5% 的样本占比，必填）">
            <n-input-number v-model:value="createForm.max_severe_loss_rate_pct" :min="0.01" :max="100" :step="0.1" />
          </n-form-item>
          <n-form-item label="多重检验方法（必填）">
            <n-select v-model:value="createForm.multiple_testing_method" :options="MULTIPLE_TESTING_OPTIONS" placeholder="选择锁定方法" />
          </n-form-item>
        </template>
        <n-form-item label="采样总目标（5~100）"><n-input-number v-model:value="createForm.sample_target" :min="createForm.experiment_type === 'score_blind' ? scoreBlindSampleTargetMin : 5" :max="100" /></n-form-item>
        <n-form-item v-if="createForm.experiment_type === 'prompt'" label="父实验（可选，P2-2 版本谱系）">
          <n-select v-model:value="createForm.parent_id" clearable :options="parentOptions" placeholder="无" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="exp-toolbar">
          <n-button @click="showCreate = false">取消</n-button>
          <n-button type="primary" :loading="creating" @click="submitCreate">创建（draft）</n-button>
        </div>
      </template>
    </n-modal>

    <n-modal :show="!!completeTarget" preset="card" title="完成实验" class="exp-modal" @update:show="(v: boolean) => { if (!v) completeTarget = null }">
      <n-form label-placement="top">
        <n-alert v-if="completeTarget && isScoreBlind(completeTarget)" type="warning" :show-icon="false" :bordered="false" class="exp-create-alert">
		  此处仅记录管理员的人工判断，不代表已满足锁定协议，也不会开放 prompt 晋级动作；协议成熟度与护栏以“选股配对评估”页为准。
        </n-alert>
        <n-form-item label="结论">
          <n-select v-model:value="completeForm.conclusion" :options="[
			{ label: completeTarget && isScoreBlind(completeTarget) ? '倾向有增量（管理员记录，非协议判定）' : '有增量（improved，可申请晋级）', value: 'improved' },
            { label: '无增量（no_improvement）', value: 'no_improvement' },
            { label: '劣化（worse）', value: 'worse' },
          ]" />
        </n-form-item>
        <n-form-item label="失败原因（非 improved 必填——失败原因是飞轮资产）">
          <n-input v-model:value="completeForm.failure_reason" type="textarea" :rows="3" maxlength="300" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="exp-toolbar">
          <n-button @click="completeTarget = null">取消</n-button>
          <n-button type="primary" :loading="acting" @click="submitComplete">提交</n-button>
        </div>
      </template>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.exp-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.exp-toolbar {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  justify-content: flex-end;
}
.exp-head {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.exp-id {
  opacity: 0.7;
}
.exp-name {
  font-weight: 600;
}
.exp-sub {
  opacity: 0.6;
  font-size: 12px;
}
.exp-body {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 13px;
}
.exp-row {
  display: flex;
  gap: 8px;
}
.exp-row > :last-child {
  min-width: 0;
  overflow-wrap: anywhere;
}
.exp-row-warning {
  color: v-bind('vars.warningColor');
}
.exp-k {
  flex: none;
  width: 108px;
  opacity: 0.6;
}
.exp-content {
  white-space: pre-wrap;
  word-break: break-all;
}
.exp-actions {
  display: flex;
  gap: 8px;
  margin-top: 4px;
  flex-wrap: wrap;
}
.exp-shadow-alert,
.exp-create-alert {
  margin-bottom: 6px;
}
.exp-protocol-fixed {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.exp-runs {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px dashed rgba(128, 128, 128, 0.3);
}
.exp-runs-title {
  opacity: 0.6;
  margin-bottom: 4px;
}
.exp-run-line {
  font-size: 12px;
  opacity: 0.85;
  overflow-wrap: anywhere;
}
.exp-empty {
  opacity: 0.6;
  font-size: 13px;
  padding: 12px 0;
}
.exp-modal {
  width: min(680px, 92vw);
}

@media (max-width: 600px) {
  .exp-row {
    display: block;
  }
  .exp-k {
    display: block;
    width: auto;
    margin-bottom: 2px;
  }
}
</style>
