<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NEmpty,
  NForm,
  NFormItem,
  NModal,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NStep,
  NSteps,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  confirmDataImport,
  downloadDataImportTemplate,
  getDataImport,
  listDataImports,
  previewDataImport,
  rollbackDataImport,
  uploadDataImport,
  type DataImportBatch,
  type DataImportKind,
  type DataImportBatchSummary,
  type DataImportRollbackConflict,
  type DataImportRow,
  type DataImportNormalized,
} from '@/api/dataImport'
import { listWatchlists, type WatchlistGroup } from '@/api/watchlist'
import { listPortfolios, type PortfolioAccount } from '@/api/portfolio'
import { getSessionEpoch } from '@/api/token'

const props = withDefaults(
  defineProps<{
    show: boolean
    initialKind?: DataImportKind
    accountId?: number
  }>(),
  { initialKind: 'position' },
)

const emit = defineEmits<{
  (event: 'update:show', value: boolean): void
  (event: 'confirmed', kind: DataImportKind): void
  (event: 'rolled-back', kind: DataImportKind): void
}>()

const message = useMessage()
const step = ref(1)
const kind = ref<DataImportKind>(props.initialKind)
const file = ref<File | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const batch = ref<DataImportBatch | null>(null)
const mapping = ref<Record<string, string>>({})
const targetGroupID = ref<number | null>(null)
const groups = ref<WatchlistGroup[]>([])
const accounts = ref<PortfolioAccount[]>([])
const recentBatches = ref<DataImportBatchSummary[]>([])
const loading = ref(false)
const requestKind = ref<'read' | 'write'>('read')
const auxiliaryError = ref('')
let requestSeq = 0
let dialogSeq = 0
let requestSession = getSessionEpoch()
let disposed = false
const requestError = ref('')
const rollbackConflicts = ref<DataImportRollbackConflict[]>([])
const rowPage = ref(1)
const rowPageSize = 20
const rowList = ref<HTMLElement | null>(null)
const stepLabels = ['选择文件', '列映射', '核对预检', '导入结果']

const kindOptions: Array<{ label: string; value: DataImportKind }> = [
  { label: '自选', value: 'watchlist' },
  { label: '持仓', value: 'position' },
  { label: '成交流水', value: 'trade' },
]
const headerOptions = computed(() => (batch.value?.headers || []).map((header) => ({ label: header, value: header })))
const groupOptions = computed(() => {
  const options = groups.value.map((group) => ({ label: group.name, value: group.id }))
  if (targetGroupID.value && !options.some(option => option.value === targetGroupID.value)) {
    options.push({ label: `原自选分组 #${targetGroupID.value}（当前不可用）`, value: targetGroupID.value })
  }
  return options
})
const hasProblems = computed(() => Boolean(batch.value && (batch.value.error_rows > 0 || batch.value.conflict_rows > 0)))
const previewRows = computed(() => {
  const rows = batch.value?.rows || []
  const problems = rows.filter((row) => row.status === 'error' || row.status === 'conflict')
  return problems.length ? problems : rows
})
const rowPages = computed(() => Math.max(1, Math.ceil(previewRows.value.length / rowPageSize)))
const displayRows = computed(() => previewRows.value.slice((rowPage.value - 1) * rowPageSize, rowPage.value * rowPageSize))
const missingPreview = computed(() => Boolean(batch.value && (batch.value.rows.length !== batch.value.total_rows || batch.value.rows.some(row => row.status === 'valid' && !row.normalized))))
const targetLabel = computed(() => {
  if (!batch.value) return ''
  if (batch.value.kind === 'watchlist') {
    const id = step.value === 2 ? targetGroupID.value : batch.value.target_group_id
    return id ? `自选分组：${groups.value.find(group => group.id === id)?.name || `分组 #${id}（当前不可用）`}` : '请选择目标自选分组'
  }
  const id = batch.value.account_id
  return `真实账户：${accounts.value.find(account => account.id === id)?.name || (id ? `账户 #${id}` : '历史账户待核对')}`
})

function normalizedFields(row: DataImportRow) {
  const normalized = row.normalized
  if (!normalized) return []
  return (batch.value?.columns || []).map(column => {
    let value: unknown = normalized[column.key as keyof DataImportNormalized]
    if (column.key === 'market') value = { cn: 'A 股', hk: '港股', us: '美股' }[normalized.market] || value
    if (column.key === 'position_type') value = { long_term: '长线', short_term: '短线' }[normalized.position_type || ''] || value
    if (column.key === 'side') value = { buy: '买入', sell: '卖出' }[normalized.side || ''] || value
    if (row.status === 'valid') {
      if (column.key === 'position_id' && normalized.virtual_key) value = '本批新建持仓'
      if (column.key === 'fee' || column.key === 'tax') value ??= 0
      if (column.key === 'name' && !value) value = normalized.symbol
    }
    return { key: column.key, label: column.label, value: typeof value === 'number' ? value.toLocaleString('zh-CN', { useGrouping: false, maximumFractionDigits: 4 }) : String(value ?? '') || '空' }
  })
}

function changeRowPage(page: number) {
  rowPage.value = Math.max(1, Math.min(page, rowPages.value))
  if (rowList.value) rowList.value.scrollTop = 0
}

function acceptBatch(result: DataImportBatch) {
  batch.value = result
  kind.value = result.kind
  mapping.value = result.status === 'uploaded' ? { ...result.suggestions, ...result.mapping } : { ...result.mapping }
  targetGroupID.value = result.target_group_id || groups.value[0]?.id || null
  rowPage.value = 1
  rollbackConflicts.value = []
  if (result.status === 'confirmed' || result.status === 'rolled_back') step.value = 4
  else if (result.status === 'previewed') step.value = 3
  else step.value = 2
}

function reset() {
  requestSeq++
  loading.value = false
  step.value = 1
  kind.value = props.initialKind
  file.value = null
  if (fileInput.value) fileInput.value.value = ''
  batch.value = null
  mapping.value = {}
  targetGroupID.value = null
  requestError.value = ''
  rollbackConflicts.value = []
  auxiliaryError.value = ''
  groups.value = []
  accounts.value = []
  recentBatches.value = []
  rowPage.value = 1
}

function beginRequest(kind: 'read' | 'write') {
  requestKind.value = kind
  loading.value = true
  requestError.value = ''
  requestSession = getSessionEpoch()
  return ++requestSeq
}
function isCurrent(seq: number) {
  return !disposed && props.show && seq === requestSeq && requestSession === getSessionEpoch()
}

watch(
  () => [props.show, props.accountId] as const,
  async ([show]) => {
    const seq = ++dialogSeq
    requestSeq++
    if (!show) return
    reset()
    const session = getSessionEpoch()
    const [watchlists, recent, portfolios] = await Promise.allSettled([
      listWatchlists(),
      listDataImports(10, props.accountId),
      listPortfolios(),
    ])
    if (disposed || !props.show || seq !== dialogSeq || session !== getSessionEpoch()) return
    const errors: string[] = []
    if (watchlists.status === 'fulfilled') groups.value = watchlists.value
    else errors.push('自选分组读取失败：' + (watchlists.reason as Error).message)
    if (recent.status === 'fulfilled') recentBatches.value = recent.value
    else errors.push('导入历史读取失败：' + (recent.reason as Error).message)
    if (portfolios.status === 'fulfilled') accounts.value = portfolios.value
    else errors.push('账户名称读取失败：' + (portfolios.reason as Error).message)
    auxiliaryError.value = errors.join('；')
    if (!targetGroupID.value) targetGroupID.value = groups.value[0]?.id || null
  },
)
onBeforeUnmount(() => {
  disposed = true
  requestSeq++
  dialogSeq++
})

function setShow(value: boolean) {
  if (!value && loading.value && requestKind.value === 'write') return
  emit('update:show', value)
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  file.value = input.files?.[0] || null
  requestError.value = ''
}

async function downloadTemplate() {
  const seq = dialogSeq
  const session = getSessionEpoch()
  try {
    await downloadDataImportTemplate(kind.value)
  } catch (error) {
    if (!disposed && props.show && seq === dialogSeq && session === getSessionEpoch()) message.error((error as Error).message)
  }
}

async function upload() {
  if (loading.value) return
  if (!file.value) {
    requestError.value = '请选择 CSV 文件'
    return
  }
  const seq = beginRequest('write')
  try {
    const result = await uploadDataImport(kind.value, file.value, props.accountId)
    if (!isCurrent(seq)) return
    acceptBatch(result)
  } catch (error) {
    if (isCurrent(seq)) requestError.value = (error as Error).message
  } finally {
    if (isCurrent(seq)) loading.value = false
  }
}

async function openBatch(id: string) {
  if (loading.value && requestKind.value === 'write') return
  const seq = beginRequest('read')
  try {
    const result = await getDataImport(id)
    if (!isCurrent(seq)) return
    acceptBatch(result)
  } catch (error) {
    if (isCurrent(seq)) requestError.value = (error as Error).message
  } finally {
    if (isCurrent(seq)) loading.value = false
  }
}

async function preview() {
  if (!batch.value || loading.value) return
  if (kind.value === 'watchlist' && !targetGroupID.value) {
    requestError.value = '请选择自选分组'
    return
  }
  const seq = beginRequest('write')
  try {
    const result = await previewDataImport(batch.value.id, {
      version: batch.value.version,
      mapping: { ...mapping.value },
      target_group_id: targetGroupID.value || undefined,
    })
    if (!isCurrent(seq)) return
    acceptBatch(result)
  } catch (error) {
    if (isCurrent(seq)) requestError.value = (error as Error).message
  } finally {
    if (isCurrent(seq)) loading.value = false
  }
}

async function confirm() {
  if (!batch.value || hasProblems.value || missingPreview.value || loading.value) return
  const seq = beginRequest('write')
  try {
    const result = await confirmDataImport(batch.value.id, batch.value.version)
    if (!isCurrent(seq)) return
    acceptBatch(result)
    emit('confirmed', result.kind)
  } catch (error) {
    if (isCurrent(seq)) requestError.value = (error as Error).message
  } finally {
    if (isCurrent(seq)) loading.value = false
  }
}

async function rollback() {
  if (!batch.value || loading.value) return
  const seq = beginRequest('write')
  rollbackConflicts.value = []
  try {
    const result = await rollbackDataImport(batch.value.id)
    if (!isCurrent(seq)) return
    if (result.status === 'conflict') {
      rollbackConflicts.value = result.conflicts
      return
    }
    batch.value.status = 'rolled_back'
    emit('rolled-back', batch.value.kind)
  } catch (error) {
    if (isCurrent(seq)) requestError.value = (error as Error).message
  } finally {
    if (isCurrent(seq)) loading.value = false
  }
}

function statusType(status: string) {
  if (status === 'valid') return 'success'
  if (status === 'conflict') return 'warning'
  if (status === 'error') return 'error'
  return 'default'
}

function statusLabel(status: string) {
  return { valid: '可导入', conflict: '冲突', error: '错误', uploaded: '待预检' }[status] || status
}
</script>

<template>
  <n-modal
    :show="show"
    preset="card"
    title="统一数据导入"
    class="import-wizard-modal"
    :mask-closable="!loading || requestKind === 'read'"
    :close-on-esc="!loading || requestKind === 'read'"
    :closable="!loading || requestKind === 'read'"
    @update:show="setShow"
  >
    <div class="import-wizard">
      <n-steps :current="step" size="small">
        <n-step title="选择文件" />
        <n-step title="列映射" />
        <n-step title="核对预检" />
        <n-step title="结果" />
      </n-steps>
      <div class="mobile-step">步骤 {{ step }} / 4 · {{ stepLabels[step - 1] }}</div>
      <div v-if="batch && step > 1" class="batch-context">
        <strong>{{ targetLabel }}</strong>
        <span>{{ batch.file_name }}</span>
      </div>

      <n-alert v-if="requestError" type="error" :bordered="false">
        {{ requestError }}
      </n-alert>
      <n-alert v-if="auxiliaryError" type="warning" :bordered="false">{{ auxiliaryError }}</n-alert>

      <template v-if="step === 1">
        <n-form label-placement="top">
          <n-form-item label="导入类型">
            <n-radio-group v-model:value="kind" :disabled="loading">
              <n-radio-button v-for="option in kindOptions" :key="option.value" :value="option.value">
                {{ option.label }}
              </n-radio-button>
            </n-radio-group>
          </n-form-item>
          <n-form-item label="CSV 文件">
            <input ref="fileInput" class="file-input" type="file" accept=".csv,text/csv" :disabled="loading" @change="onFileChange" />
          </n-form-item>
        </n-form>
        <div v-if="recentBatches.length" class="recent-batches">
          <div class="recent-title">最近导入批次</div>
          <button v-for="item in recentBatches" :key="item.id" type="button" class="recent-row" :disabled="loading && requestKind === 'write'" @click="openBatch(item.id)">
            <span>{{ kindOptions.find((option) => option.value === item.kind)?.label }} · {{ item.file_name }} · 第 {{ item.attempt }} 次</span>
            <n-tag size="small" :type="item.status === 'confirmed' ? 'success' : item.status === 'rolled_back' ? 'warning' : 'default'" :bordered="false">
              {{ { uploaded: '待映射', previewed: '已预检', confirmed: '已确认', rolled_back: '已撤销' }[item.status] }}
            </n-tag>
          </button>
        </div>
      </template>

      <template v-else-if="step === 2 && batch">
        <n-alert type="info" :bordered="false">
          已读取 {{ batch.total_rows }} 行。请选择每个字段对应的 CSV 列；未映射的可选字段将留空或使用默认值，下一步可核对实际导入内容。
        </n-alert>
        <n-form label-placement="top" :disabled="loading" class="mapping-form">
          <n-form-item v-if="kind === 'watchlist'" label="目标分组" required>
            <n-select v-model:value="targetGroupID" :options="groupOptions" placeholder="选择本人自选分组" />
          </n-form-item>
          <n-form-item v-for="column in batch.columns" :key="column.key" :label="column.label" :required="column.required">
            <n-select
              v-model:value="mapping[column.key]"
              :options="headerOptions"
              :clearable="!column.required"
              :placeholder="column.required ? '请选择 CSV 列' : '可不映射'"
            />
          </n-form-item>
        </n-form>
      </template>

      <template v-else-if="step === 3 && batch">
        <div class="summary-strip">
          <span>总行数 <strong>{{ batch.total_rows }}</strong></span>
          <span>可导入 <strong>{{ batch.valid_rows }}</strong></span>
          <span>错误 <strong>{{ batch.error_rows }}</strong></span>
          <span>冲突 <strong>{{ batch.conflict_rows }}</strong></span>
        </div>
        <n-alert v-if="hasProblems" type="warning" :bordered="false">
          当前批次不会写入任何业务数据。请根据行号修正原 CSV 后重新上传，或调整列映射后再次预检。
        </n-alert>
        <n-alert v-else-if="missingPreview" type="error" :bordered="false">
          预检详情不完整，请返回列映射重新预检后再确认。
        </n-alert>
        <n-alert v-else type="success" :bordered="false">
          请核对下方实际导入内容。确认后整批导入；若记录发生冲突，本批不会部分写入。
        </n-alert>
        <p v-if="hasProblems" class="row-message">下方仅列出需要修正的错误或冲突行。</p>
        <div v-if="displayRows.length" ref="rowList" class="row-list">
          <div v-for="row in displayRows" :key="row.row" class="row-fact">
            <div class="row-head">
              <strong>第 {{ row.row }} 行</strong>
              <n-tag size="small" :type="statusType(row.status)" :bordered="false">{{ statusLabel(row.status) }}</n-tag>
            </div>
            <p v-if="row.message" class="row-message">{{ row.message }}</p>
            <div v-if="row.normalized" class="normalized-values">
              <div class="values-title">{{ row.status === 'valid' ? '实际导入内容' : '已识别内容（仍须修正）' }}</div>
              <dl>
                <div v-for="field in normalizedFields(row)" :key="field.key"><dt>{{ field.label }}</dt><dd>{{ field.value }}</dd></div>
              </dl>
            </div>
            <details class="raw-values" :open="row.status !== 'valid'">
              <summary>CSV 原始值</summary>
              <div class="row-values">
                <span v-for="(value, key) in row.raw" :key="key"><b>{{ key }}</b>{{ value || '空' }}</span>
              </div>
            </details>
          </div>
        </div>
        <n-empty v-else description="没有可显示的行" />
        <div v-if="rowPages > 1" class="row-pagination" aria-label="预检记录翻页">
          <span>第 {{ rowPage }} / {{ rowPages }} 页 · 共 {{ previewRows.length }} 行</span>
          <n-button size="small" :disabled="rowPage <= 1" @click="changeRowPage(rowPage - 1)">上一页</n-button>
          <n-button size="small" :disabled="rowPage >= rowPages" @click="changeRowPage(rowPage + 1)">下一页</n-button>
        </div>
      </template>

      <template v-else-if="step === 4 && batch">
        <n-alert :type="batch.status === 'rolled_back' ? 'warning' : 'success'" :bordered="false">
          <template v-if="batch.status === 'rolled_back'">本批已撤销，导入历史仍保留，可重新上传文件。</template>
          <template v-else>
            导入完成：新建 {{ batch.created_rows }} 项，更新 {{ batch.updated_rows }} 项，共处理 {{ batch.total_rows }} 行。
          </template>
        </n-alert>
        <div class="batch-audit">
          <span>批次 ID</span><code>{{ batch.id }}</code>
          <span>文件尝试</span><code>第 {{ batch.attempt }} 次</code>
          <span>文件摘要</span><code>{{ batch.file_digest.slice(0, 16) }}</code>
        </div>
        <n-alert v-if="rollbackConflicts.length" type="error" :bordered="false">
          暂时无法自动撤销。以下记录存在后续交易、人工编辑或关联记录：
          <ul class="conflict-list">
            <li v-for="conflict in rollbackConflicts" :key="`${conflict.record_kind}-${conflict.record_id}`">
              {{ conflict.message }}（{{ conflict.record_kind }} #{{ conflict.record_id }}）
            </li>
          </ul>
        </n-alert>
      </template>
    </div>
    <template #footer>
        <div v-if="step === 1" class="step-actions split-actions">
          <n-button quaternary @click="downloadTemplate">下载当前类型模板</n-button>
          <n-button type="primary" :loading="loading" :disabled="!file" @click="upload">上传并识别列</n-button>
        </div>
        <div v-else-if="step === 2 && batch" class="step-actions">
          <n-button :disabled="loading" @click="step = 1">重新选择文件</n-button>
          <n-button type="primary" :loading="loading" @click="preview">生成只读预检</n-button>
        </div>
        <div v-else-if="step === 3 && batch" class="step-actions">
          <n-button :disabled="loading" @click="step = 2">返回列映射</n-button>
          <n-button v-if="hasProblems" type="primary" :disabled="loading" @click="step = 1">修正后重新上传</n-button>
          <n-button v-else type="primary" :loading="loading" :disabled="missingPreview" @click="confirm">确认导入</n-button>
        </div>
        <div v-else-if="step === 4 && batch" class="step-actions split-actions">
          <n-button :disabled="loading && requestKind === 'write'" @click="setShow(false)">关闭</n-button>
          <n-button
            v-if="batch.status === 'confirmed'"
            type="error"
            secondary
            :loading="loading"
            @click="rollback"
          >
            检查并撤销本批
          </n-button>
        </div>
    </template>
  </n-modal>
</template>

<style scoped>
:global(.import-wizard-modal) {
  width: min(860px, calc(100vw - 24px));
  max-height: calc(100dvh - 32px);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:global(.import-wizard-modal > .n-card-content) {
  min-height: 0;
  overflow-y: auto;
}
:global(.import-wizard-modal > .n-card__footer) {
  flex-shrink: 0;
}
.mobile-step { display: none; }
.batch-context { display: grid; gap: 4px; overflow-wrap: anywhere; }
.batch-context span { font-size: 12px; opacity: 0.7; }
.import-wizard {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.file-input {
  width: 100%;
  min-height: 34px;
}
.mapping-form {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 18px;
}
.mapping-form :deep(.n-form-item) {
  min-width: 0;
}
.summary-strip {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 8px;
}
.summary-strip span {
  padding: 8px;
  border: 1px solid rgba(128, 128, 128, 0.24);
  border-radius: 6px;
  text-align: center;
}
.row-list {
  display: flex;
  flex-direction: column;
  max-height: 360px;
  overflow-y: auto;
  border-top: 1px solid rgba(128, 128, 128, 0.24);
}
.row-fact {
  padding: 10px 2px;
  border-bottom: 1px solid rgba(128, 128, 128, 0.2);
}
.row-head,
.step-actions,
.split-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
}
.row-head {
  justify-content: space-between;
}
.split-actions {
  justify-content: space-between;
}
.row-message {
  margin: 6px 0;
  font-size: 13px;
}
.row-values {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  font-size: 12px;
  opacity: 0.78;
}
.row-values span {
  overflow-wrap: anywhere;
}
.row-values b {
  margin-right: 4px;
  font-weight: 500;
}
.values-title { margin: 8px 0; font-size: 12px; font-weight: 600; }
.normalized-values dl { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px 18px; margin: 0 0 12px; }
.normalized-values dl > div { min-width: 0; }
.normalized-values dt { font-size: 12px; opacity: 0.65; }
.normalized-values dd { margin: 2px 0 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.raw-values summary { cursor: pointer; font-size: 12px; }
.raw-values .row-values { margin-top: 8px; }
.row-pagination { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; font-size: 12px; }
.row-pagination > span { margin-right: auto; }
.conflict-list { padding-left: 20px; }
.conflict-list li { overflow-wrap: anywhere; }
.batch-audit {
  display: grid;
  grid-template-columns: 90px minmax(0, 1fr);
  gap: 8px 12px;
  align-items: center;
  font-size: 12px;
}
.recent-batches {
  border-top: 1px solid rgba(128, 128, 128, 0.22);
}
.recent-title {
  padding: 12px 2px 6px;
  font-size: 13px;
  font-weight: 600;
}
.recent-row {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 9px 2px;
  color: inherit;
  background: transparent;
  border: 0;
  border-bottom: 1px solid rgba(128, 128, 128, 0.18);
  text-align: left;
  cursor: pointer;
}
.recent-row span:first-child {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.batch-audit code {
  overflow-wrap: anywhere;
}
@media (max-width: 768px) {
  :global(.import-wizard-modal) {
    max-height: calc(100dvh - 16px);
  }
  .import-wizard :deep(.n-steps) {
    overflow-x: auto;
  }
  .summary-strip { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .step-actions,
  .split-actions {
    flex-wrap: wrap;
  }
  .step-actions .n-button {
    flex: 1;
    min-width: 130px;
  }
  .batch-audit {
    grid-template-columns: minmax(0, 1fr);
  }
}
@media (max-width: 600px) {
  .mapping-form { grid-template-columns: minmax(0, 1fr); }
  .import-wizard :deep(.n-steps) { display: none; }
  .mobile-step { display: block; font-size: 13px; font-weight: 600; }
}
</style>
