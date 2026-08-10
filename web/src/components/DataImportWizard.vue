<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
} from '@/api/dataImport'
import { listWatchlists, type WatchlistGroup } from '@/api/watchlist'

const props = withDefaults(
  defineProps<{
    show: boolean
    initialKind?: DataImportKind
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
const batch = ref<DataImportBatch | null>(null)
const mapping = ref<Record<string, string>>({})
const targetGroupID = ref<number | null>(null)
const groups = ref<WatchlistGroup[]>([])
const recentBatches = ref<DataImportBatchSummary[]>([])
const loading = ref(false)
const requestError = ref('')
const rollbackConflicts = ref<DataImportRollbackConflict[]>([])

const kindOptions: Array<{ label: string; value: DataImportKind }> = [
  { label: '自选', value: 'watchlist' },
  { label: '持仓', value: 'position' },
  { label: '成交流水', value: 'trade' },
]
const headerOptions = computed(() => (batch.value?.headers || []).map((header) => ({ label: header, value: header })))
const groupOptions = computed(() => groups.value.map((group) => ({ label: group.name, value: group.id })))
const hasProblems = computed(() => Boolean(batch.value && (batch.value.error_rows > 0 || batch.value.conflict_rows > 0)))
const displayRows = computed(() => {
  const rows = batch.value?.rows || []
  const problems = rows.filter((row) => row.status === 'error' || row.status === 'conflict')
  return problems.length ? problems : rows.slice(0, 100)
})

function reset() {
  step.value = 1
  kind.value = props.initialKind
  file.value = null
  batch.value = null
  mapping.value = {}
  targetGroupID.value = null
  requestError.value = ''
  rollbackConflicts.value = []
}

watch(
  () => props.show,
  async (show) => {
    if (!show) return
    reset()
    const [watchlists, recent] = await Promise.all([
      listWatchlists().catch(() => [] as WatchlistGroup[]),
      listDataImports(10).catch(() => [] as DataImportBatchSummary[]),
    ])
    groups.value = watchlists
    recentBatches.value = recent
    targetGroupID.value = groups.value[0]?.id || null
  },
)

function setShow(value: boolean) {
  emit('update:show', value)
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  file.value = input.files?.[0] || null
  requestError.value = ''
}

async function downloadTemplate() {
  try {
    await downloadDataImportTemplate(kind.value)
  } catch (error) {
    message.error((error as Error).message)
  }
}

async function upload() {
  if (!file.value) {
    requestError.value = '请选择 CSV 文件'
    return
  }
  loading.value = true
  requestError.value = ''
  try {
    batch.value = await uploadDataImport(kind.value, file.value)
    mapping.value = { ...batch.value.suggestions, ...batch.value.mapping }
    if (batch.value.status === 'confirmed' || batch.value.status === 'rolled_back') {
      step.value = 4
    } else if (batch.value.status === 'previewed') {
      step.value = 3
    } else {
      step.value = 2
    }
  } catch (error) {
    requestError.value = (error as Error).message
  } finally {
    loading.value = false
  }
}

async function openBatch(id: string) {
  loading.value = true
  requestError.value = ''
  try {
    batch.value = await getDataImport(id)
    kind.value = batch.value.kind
    mapping.value = { ...batch.value.suggestions, ...batch.value.mapping }
    targetGroupID.value = batch.value.target_group_id || groups.value[0]?.id || null
    if (batch.value.status === 'confirmed' || batch.value.status === 'rolled_back') step.value = 4
    else if (batch.value.status === 'previewed') step.value = 3
    else step.value = 2
  } catch (error) {
    requestError.value = (error as Error).message
  } finally {
    loading.value = false
  }
}

async function preview() {
  if (!batch.value) return
  if (kind.value === 'watchlist' && !targetGroupID.value) {
    requestError.value = '请选择自选分组'
    return
  }
  loading.value = true
  requestError.value = ''
  try {
    batch.value = await previewDataImport(batch.value.id, {
      version: batch.value.version,
      mapping: mapping.value,
      target_group_id: targetGroupID.value || undefined,
    })
    mapping.value = { ...batch.value.mapping }
    step.value = 3
  } catch (error) {
    requestError.value = (error as Error).message
  } finally {
    loading.value = false
  }
}

async function confirm() {
  if (!batch.value || hasProblems.value) return
  loading.value = true
  requestError.value = ''
  try {
    batch.value = await confirmDataImport(batch.value.id, batch.value.version)
    step.value = 4
    emit('confirmed', batch.value.kind)
  } catch (error) {
    requestError.value = (error as Error).message
  } finally {
    loading.value = false
  }
}

async function rollback() {
  if (!batch.value) return
  loading.value = true
  requestError.value = ''
  rollbackConflicts.value = []
  try {
    const result = await rollbackDataImport(batch.value.id)
    if (result.status === 'conflict') {
      rollbackConflicts.value = result.conflicts
      return
    }
    batch.value.status = 'rolled_back'
    emit('rolled-back', batch.value.kind)
  } catch (error) {
    requestError.value = (error as Error).message
  } finally {
    loading.value = false
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
    :mask-closable="!loading"
    @update:show="setShow"
  >
    <div class="import-wizard">
      <n-steps :current="step" size="small">
        <n-step title="选择文件" />
        <n-step title="列映射" />
        <n-step title="只读预检" />
        <n-step title="结果" />
      </n-steps>

      <n-alert v-if="requestError" type="error" :bordered="false">
        {{ requestError }}
      </n-alert>

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
            <input class="file-input" type="file" accept=".csv,text/csv" :disabled="loading" @change="onFileChange" />
          </n-form-item>
        </n-form>
        <div class="step-actions split-actions">
          <n-button quaternary @click="downloadTemplate">下载当前类型模板</n-button>
          <n-button type="primary" :loading="loading" :disabled="!file" @click="upload">上传并识别列</n-button>
        </div>
        <div v-if="recentBatches.length" class="recent-batches">
          <div class="recent-title">最近导入批次</div>
          <button v-for="item in recentBatches" :key="item.id" type="button" class="recent-row" @click="openBatch(item.id)">
            <span>{{ kindOptions.find((option) => option.value === item.kind)?.label }} · {{ item.file_name }} · 第 {{ item.attempt }} 次</span>
            <n-tag size="small" :type="item.status === 'confirmed' ? 'success' : item.status === 'rolled_back' ? 'warning' : 'default'" :bordered="false">
              {{ { uploaded: '待映射', previewed: '已预检', confirmed: '已确认', rolled_back: '已回滚' }[item.status] }}
            </n-tag>
          </button>
        </div>
      </template>

      <template v-else-if="step === 2 && batch">
        <n-alert type="info" :bordered="false">
          已读取 {{ batch.total_rows }} 行。请核对每个业务字段对应的 CSV 列；未映射的可选字段会留空。
        </n-alert>
        <n-form label-placement="left" label-width="110" class="mapping-form">
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
        <div class="step-actions">
          <n-button @click="step = 1">重新选择文件</n-button>
          <n-button type="primary" :loading="loading" @click="preview">生成只读预检</n-button>
        </div>
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
        <n-alert v-else type="success" :bordered="false">
          预检通过。确认时服务端只使用当前冻结的预检事实，并以单个事务写入。
        </n-alert>
        <div v-if="displayRows.length" class="row-list">
          <div v-for="row in displayRows" :key="row.row" class="row-fact">
            <div class="row-head">
              <strong>第 {{ row.row }} 行</strong>
              <n-tag size="small" :type="statusType(row.status)" :bordered="false">{{ statusLabel(row.status) }}</n-tag>
            </div>
            <p v-if="row.message" class="row-message">{{ row.message }}</p>
            <div class="row-values">
              <span v-for="(value, key) in row.raw" :key="key"><b>{{ key }}</b>{{ value || '空' }}</span>
            </div>
          </div>
        </div>
        <n-empty v-else description="没有可显示的行" />
        <div class="step-actions">
          <n-button @click="step = 2">返回列映射</n-button>
          <n-button v-if="hasProblems" type="primary" @click="step = 1">修正后重新上传</n-button>
          <n-button v-else type="primary" :loading="loading" @click="confirm">确认原子写入</n-button>
        </div>
      </template>

      <template v-else-if="step === 4 && batch">
        <n-alert :type="batch.status === 'rolled_back' ? 'warning' : 'success'" :bordered="false">
          <template v-if="batch.status === 'rolled_back'">本批已回滚，审计事实仍保留。</template>
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
          自动回滚已拒绝。以下记录存在后续交易、人工编辑或业务依赖：
          <ul>
            <li v-for="conflict in rollbackConflicts" :key="`${conflict.record_kind}-${conflict.record_id}`">
              {{ conflict.message }}（{{ conflict.record_kind }} #{{ conflict.record_id }}）
            </li>
          </ul>
        </n-alert>
        <div class="step-actions split-actions">
          <n-button @click="setShow(false)">关闭</n-button>
          <n-button
            v-if="batch.status === 'confirmed'"
            type="error"
            secondary
            :loading="loading"
            @click="rollback"
          >
            检查并回滚本批
          </n-button>
        </div>
      </template>
    </div>
  </n-modal>
</template>

<style scoped>
.import-wizard-modal {
  width: min(860px, calc(100vw - 24px));
  max-height: calc(100vh - 32px);
}
.import-wizard-modal :deep(.n-card__content) {
  overflow-y: auto;
}
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
  .import-wizard-modal {
    max-height: calc(100dvh - 16px);
  }
  .import-wizard :deep(.n-steps) {
    overflow-x: auto;
  }
  .mapping-form,
  .summary-strip {
    grid-template-columns: minmax(0, 1fr);
  }
  .mapping-form :deep(.n-form-item-label) {
    width: 96px;
  }
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
</style>
