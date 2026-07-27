<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import {
  NButton, NCollapse, NCollapseItem, NForm, NFormItem, NInput, NInputNumber,
  NModal, NSelect, NSpin, NTag, useDialog, useMessage,
} from 'naive-ui'
import {
  actLLMExperiment, auditLLMExperiment, createLLMExperiment, getLLMExperiment, listLLMExperiments,
  type LLMExperiment, type LLMExperimentActual, type LLMExperimentInput, type LLMExperimentRun,
  type LLMReleaseAudit, type LLMReleaseAuditFinding,
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

async function load() {
  loading.value = true
  try {
    rows.value = await listLLMExperiments()
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
  dialog.warning({
    title: '晋级为 champion',
    content: '将把 challenger 内容落为该模块启用中的自定义模板（生成新的不可变 revision 快照并切换指针）。发布质量门（样本量/结构化有效率/结论/内容 hash/发布审计 PASS）不通过会被拒绝。回滚 = 本页「一键切回 champion」。',
    positiveText: '晋级',
    negativeText: '取消',
    onPositiveClick: () => act(exp, 'promote'),
  })
}

function confirmRollback(exp: LLMExperiment) {
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
async function submitComplete() {
  if (!completeTarget.value) return
  acting.value = true
  try {
    await actLLMExperiment(completeTarget.value.id, 'complete', {
      conclusion: completeForm.conclusion,
      failure_reason: completeForm.failure_reason.trim(),
    })
    message.success('实验已完成（聚合报表见卡片）')
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
const createForm = reactive<LLMExperimentInput>({
  module: 'recommendation', name: '', hypothesis: '', expected_improvement: '',
  challenger_content: '', sample_target: 20, parent_id: 0,
})
const parentOptions = computed(() =>
  rows.value.map((r) => ({ label: `#${r.id} ${r.name}`, value: r.id })))
async function submitCreate() {
  creating.value = true
  try {
    const res = await createLLMExperiment({ ...createForm, parent_id: createForm.parent_id || 0 })
    res.warnings?.forEach((w) => message.warning(w))
    message.success(`实验 #${res.experiment.id} 已创建（draft）`)
    showCreate.value = false
    createForm.name = ''
    createForm.hypothesis = ''
    createForm.expected_improvement = ''
    createForm.challenger_content = ''
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
    title="LLM 实验（champion / challenger）"
    subtitle="P2-1/P2-2 实验飞轮：challenger 只影子运行（不影响业务结果）；假设→实验→反馈闭环，无增量不晋级；晋级须过发布质量门（P1-9 硬检 + P2-6 发布审计 PASS），晋级后可一键切回 champion"
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
                  <n-tag size="tiny" :bordered="false" round :type="STATUS_TYPE[r.status]">{{ STATUS_LABEL[r.status] || r.status }}</n-tag>
                  <n-tag size="tiny" :bordered="false" round>{{ r.module }}</n-tag>
                  <span class="exp-sub">采样 {{ r.sample_count }}/{{ r.sample_target }}</span>
                  <n-tag v-if="r.conclusion" size="tiny" :bordered="false" round :type="r.conclusion === 'improved' ? 'success' : 'warning'">
                    {{ CONCLUDE_LABEL[r.conclusion] || r.conclusion }}
                  </n-tag>
                  <span v-if="r.parent_id" class="exp-sub">← 父实验 #{{ r.parent_id }}</span>
                </span>
              </template>
              <div class="exp-body">
                <div class="exp-row"><span class="exp-k">假设</span><span>{{ r.hypothesis }}</span></div>
                <div class="exp-row"><span class="exp-k">预期改善</span><span>{{ r.expected_improvement }}</span></div>
                <div class="exp-row">
                  <span class="exp-k">对照锚</span>
                  <span>champion={{ r.champion_version }} · challenger hash {{ r.challenger_hash.slice(0, 8) }}<template v-if="r.promoted_revision"> · 晋级 revision {{ r.promoted_revision }}</template></span>
                </div>
                <div class="exp-row"><span class="exp-k">challenger 任务段</span><span class="exp-content">{{ r.challenger_content }}</span></div>
                <div v-if="r.failure_reason" class="exp-row"><span class="exp-k">失败原因</span><span>{{ r.failure_reason }}</span></div>
                <div v-if="r.baseline_stale" class="exp-row exp-row-warning">
                  <span class="exp-k">基线已失效</span>
                  <span>{{ r.baseline_stale }}（该实验不可再启动、审计或晋级，请基于当前 champion 新建实验）</span>
                </div>
                <div v-if="r.rollback_stale" class="exp-row"><span class="exp-k">回滚不可用</span><span>{{ r.rollback_stale }}（如需恢复历史内容请在提示词页按 revision 快照操作）</span></div>
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
                  <n-button v-if="r.status === 'draft'" size="tiny" type="primary" :loading="acting" :disabled="!!r.baseline_stale" :title="r.baseline_stale || undefined" @click="act(r, 'start')">启动采样</n-button>
                  <n-button v-if="r.status === 'running'" size="tiny" type="warning" :loading="acting" @click="completeTarget = r">完成实验</n-button>
                  <n-button v-if="r.status === 'completed'" size="tiny" type="info" :loading="auditing" :disabled="!!r.baseline_stale" :title="r.baseline_stale || undefined" @click="runAudit(r)">发布审计</n-button>
                  <n-button v-if="r.status === 'completed'" size="tiny" type="success" :loading="acting" :disabled="!!r.baseline_stale" :title="r.baseline_stale || undefined" @click="confirmPromote(r)">晋级 champion</n-button>
                  <n-button v-if="r.status === 'promoted'" size="tiny" type="error" :loading="acting" :disabled="!!r.rollback_stale" @click="confirmRollback(r)">一键切回 champion</n-button>
                  <n-button v-if="r.status !== 'promoted' && r.status !== 'abandoned' && r.status !== 'rolled_back'" size="tiny" :loading="acting" @click="act(r, 'abandon')">废弃</n-button>
                </div>
                <div v-if="detailAudits[r.id]?.length" class="exp-runs">
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
                  <div class="exp-runs-title">影子样本（{{ detailRuns[r.id].length }} 条）</div>
                  <div v-for="run in detailRuns[r.id]" :key="run.id" class="exp-run-line">
                    批次 {{ run.batch_id }} · {{ run.valid ? '有效' : '失败' }} ·
                    picks {{ run.picks_count }}/{{ run.champion_picks }} 重合 {{ run.overlap_count }} ·
                    token {{ run.champion_tokens }}/{{ run.challenger_tokens }}
                    <template v-if="run.error"> · {{ run.error }}</template>
                  </div>
                </div>
              </div>
            </n-collapse-item>
          </n-collapse>
          <div v-else class="exp-empty">暂无实验。新建一个：写下假设与预期改善（P2-2），draft → 启动后由「Prompt 实验影子采样」开关控制是否实际采样。</div>
        </n-spin>
      </SectionCard>
    </div>

    <n-modal v-model:show="showCreate" preset="card" title="新建 challenger 实验" class="exp-modal">
      <n-form label-placement="top">
        <n-form-item label="名称"><n-input v-model:value="createForm.name" maxlength="60" /></n-form-item>
        <n-form-item label="假设（想验证什么）"><n-input v-model:value="createForm.hypothesis" type="textarea" :rows="2" maxlength="300" /></n-form-item>
        <n-form-item label="预期改善（对照什么指标）"><n-input v-model:value="createForm.expected_improvement" type="textarea" :rows="2" maxlength="300" /></n-form-item>
        <n-form-item label="challenger 任务段（推荐模块 L3；系统契约段恒由程序追加）">
          <n-input v-model:value="createForm.challenger_content" type="textarea" :rows="6" maxlength="4000" />
        </n-form-item>
        <n-form-item label="采样目标（5~100）"><n-input-number v-model:value="createForm.sample_target" :min="5" :max="100" /></n-form-item>
        <n-form-item label="父实验（可选，P2-2 版本谱系）">
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

    <n-modal :show="!!completeTarget" preset="card" title="完成实验（P2-2 反馈）" class="exp-modal" @update:show="(v: boolean) => { if (!v) completeTarget = null }">
      <n-form label-placement="top">
        <n-form-item label="结论">
          <n-select v-model:value="completeForm.conclusion" :options="[
            { label: '有增量（improved，可申请晋级）', value: 'improved' },
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
</style>
