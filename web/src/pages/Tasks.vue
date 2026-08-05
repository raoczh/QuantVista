<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { storeToRefs } from 'pinia'
import { NAlert, NButton, NEmpty, NSelect, NSpin, NSwitch, NTag } from 'naive-ui'
import {
  TASK_SOURCE_LABELS,
  TASK_STATUS_LABELS,
  formatTaskTime,
  listTasks,
  taskActionLabel,
  taskDurationText,
  taskKindLabel,
  taskProgressText,
  taskRecoveryAdvice,
  taskResultRoute,
  taskSourceLabel,
  taskStageLabel,
  type TaskCenterItem,
  type TaskSource,
  type TaskStatus,
} from '@/api/taskCenter'
import { isAbortError } from '@/api/client'
import { useVisibleTaskPolling } from '@/composables/useVisibleTaskPolling'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAuthStore } from '@/stores/auth'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const router = useRouter()
const authStore = useAuthStore()
const { isAdmin } = storeToRefs(authStore)
const { vars } = useUi()

const tasks = ref<TaskCenterItem[]>([])
const loading = ref(false)
const refreshing = ref(false)
const loadError = ref('')
const source = ref<TaskSource | ''>('')
const kind = ref('')
const status = ref<TaskStatus | ''>('')
const includeSystem = ref(false)
let requestController: AbortController | null = null

const knownKindsBySource: Record<TaskSource, string[]> = {
  analysis: ['market', 'sector', 'stock', 'watchlist', 'position'],
  recommendation: ['short_term', 'long_term'],
  daily_report: ['daily_report'],
  llm: ['qa', 'compare', 'position_advice', 'screener_parse'],
  data_sync: ['sync_daily_bars', 'backfill_calendar', 'snapshot_market', 'sync_market_wide', 'init_market_history'],
}

const styleVars = computed(() => ({
  '--task-border': vars.value.borderColor,
  '--task-divider': vars.value.dividerColor,
  '--task-card': vars.value.cardColor,
  '--task-primary': vars.value.primaryColor,
  '--task-primary-soft': withAlpha(vars.value.primaryColor, 0.1),
  '--task-muted': vars.value.textColor3,
}))

const sourceOptions = computed(() => {
  const values: TaskSource[] = ['analysis', 'recommendation', 'daily_report', 'llm']
  if (isAdmin.value && includeSystem.value) values.push('data_sync')
  return [
    { label: '全部来源', value: '' },
    ...values.map((value) => ({ label: TASK_SOURCE_LABELS[value], value })),
  ]
})

const accessibleTasks = computed(() =>
  tasks.value.filter((task) => task.source !== 'data_sync' || (isAdmin.value && includeSystem.value)),
)
const sourceFilteredTasks = computed(() =>
  source.value ? accessibleTasks.value.filter((task) => task.source === source.value) : accessibleTasks.value,
)

const kindOptions = computed(() => {
  const sources = source.value
    ? [source.value]
    : (Object.keys(knownKindsBySource) as TaskSource[]).filter(
        (value) => value !== 'data_sync' || (isAdmin.value && includeSystem.value),
      )
  const values = [
    ...new Set([
      ...sources.flatMap((value) => knownKindsBySource[value]),
      ...sourceFilteredTasks.value.map((task) => task.kind).filter(Boolean),
    ]),
  ]
  values.sort((a, b) => taskKindLabel(a).localeCompare(taskKindLabel(b), 'zh-CN'))
  return [
    { label: '全部类型', value: '' },
    ...values.map((value) => ({ label: taskKindLabel(value), value })),
  ]
})

const statusOptions = [
  { label: '全部状态', value: '' },
  ...(['processing', 'success', 'degraded', 'failed'] as TaskStatus[]).map((value) => ({
    label: TASK_STATUS_LABELS[value],
    value,
  })),
]

const kindFilteredTasks = computed(() =>
  kind.value ? sourceFilteredTasks.value.filter((task) => task.kind === kind.value) : sourceFilteredTasks.value,
)
const visibleTasks = computed(() =>
  status.value
    ? kindFilteredTasks.value.filter((task) => task.status === status.value)
    : kindFilteredTasks.value,
)
const hasProcessing = computed(() => sourceFilteredTasks.value.some((task) => task.status === 'processing'))

const statusStats = computed(() => {
  const colors: Record<TaskStatus, string> = {
    processing: vars.value.infoColor,
    success: vars.value.successColor,
    degraded: vars.value.warningColor,
    failed: vars.value.errorColor,
  }
  return (['processing', 'success', 'degraded', 'failed'] as TaskStatus[]).map((value) => ({
    value,
    label: TASK_STATUS_LABELS[value],
    count: kindFilteredTasks.value.filter((task) => task.status === value).length,
    color: colors[value],
  }))
})

async function loadTaskRows() {
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  const initial = tasks.value.length === 0
  if (initial) loading.value = true
  refreshing.value = true
  loadError.value = ''
  try {
    const rows = await listTasks(
      {
        source: source.value,
        kind: kind.value,
        limit: 100,
        include_system: isAdmin.value && includeSystem.value,
      },
      controller.signal,
    )
    if (requestController === controller) tasks.value = rows
  } catch (error) {
    if (!isAbortError(error) && requestController === controller) loadError.value = (error as Error).message
  } finally {
    if (requestController === controller) {
      requestController = null
      loading.value = false
      refreshing.value = false
    }
  }
}

const { refreshNow } = useVisibleTaskPolling(loadTaskRows, () => hasProcessing.value, {
  activeIntervalMs: 4000,
})

watch(source, () => {
  const hadKind = kind.value !== ''
  kind.value = ''
  if (!hadKind) void refreshNow()
})

watch(kind, () => void refreshNow())

function resetSystemOnlyFilters(): boolean {
  if (source.value === 'data_sync') {
    source.value = ''
    return true
  }
  if (knownKindsBySource.data_sync.includes(kind.value)) {
    kind.value = ''
    return true
  }
  return false
}

watch(includeSystem, (enabled) => {
  if (!enabled && resetSystemOnlyFilters()) return
  void refreshNow()
})

watch(isAdmin, (admin) => {
  if (admin) return
  if (includeSystem.value) includeSystem.value = false
  else resetSystemOnlyFilters()
})

onBeforeUnmount(() => requestController?.abort())

function statusTagType(value: TaskStatus): 'info' | 'success' | 'warning' | 'error' {
  if (value === 'processing') return 'info'
  if (value === 'success') return 'success'
  if (value === 'degraded') return 'warning'
  return 'error'
}

function toggleStatus(value: TaskStatus) {
  status.value = status.value === value ? '' : value
}

function providerModel(task: TaskCenterItem) {
  if (task.provider && task.model) return `${task.provider} / ${task.model}`
  return task.model || task.provider || '-'
}

function traceText(traceID: string) {
  return traceID.length > 16 ? `${traceID.slice(0, 16)}…` : traceID
}

function showTaskError(task: TaskCenterItem) {
  return task.status === 'failed' || task.status === 'degraded'
}

function openTask(task: TaskCenterItem) {
  const target = taskResultRoute(task)
  if (target) void router.push(target)
}
</script>

<template>
  <PageContainer title="任务中心" subtitle="分析、推荐、日报与后台 AI 任务">
    <template #actions>
      <n-button size="small" :loading="refreshing" @click="refreshNow">刷新</n-button>
    </template>

    <div class="task-page" :style="styleVars">
      <div class="status-grid" aria-label="任务状态统计">
        <button
          v-for="item in statusStats"
          :key="item.value"
          type="button"
          class="status-stat"
          :class="{ active: status === item.value }"
          :style="{ '--status-color': item.color }"
          :aria-pressed="status === item.value"
          @click="toggleStatus(item.value)"
        >
          <span class="status-stat-label">{{ item.label }}</span>
          <span class="status-stat-value qv-figure">{{ item.count }}</span>
        </button>
      </div>

      <SectionCard :hoverable="false">
        <div class="filters">
          <n-select v-model:value="source" :options="sourceOptions" size="small" class="filter-control" />
          <n-select v-model:value="kind" :options="kindOptions" size="small" class="filter-control" />
          <n-select v-model:value="status" :options="statusOptions" size="small" class="filter-control" />
          <label v-if="isAdmin" class="system-toggle">
            <n-switch v-model:value="includeSystem" size="small" />
            <span>包含系统任务</span>
          </label>
          <span class="filter-count qv-tnum">{{ visibleTasks.length }} 项</span>
        </div>

        <n-alert v-if="loadError" type="error" :bordered="false" title="任务列表读取失败" class="load-alert">
          {{ loadError }}
        </n-alert>

        <n-spin :show="loading">
          <n-empty v-if="!visibleTasks.length && !loading" description="当前筛选下没有任务" class="task-empty" />

          <div v-else class="desktop-list qv-scroll-x">
            <table class="task-table">
              <thead>
                <tr>
                  <th>任务</th>
                  <th>状态 / 阶段</th>
                  <th>模型</th>
                  <th class="num-cell">Token</th>
                  <th class="num-cell">耗时</th>
                  <th>进度</th>
                  <th>状态说明</th>
                  <th>创建时间</th>
                  <th class="action-cell">操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="task in visibleTasks" :key="task.id">
                  <td class="task-main-cell">
                    <div class="task-title">{{ task.title || taskKindLabel(task.kind) }}</div>
                    <div class="task-meta">
                      <span>{{ taskSourceLabel(task.source) }}</span>
                      <span>{{ taskKindLabel(task.kind) }}</span>
                      <span v-if="task.target">{{ task.target }}</span>
                      <span class="qv-mono">#{{ task.source_id }}</span>
                    </div>
                    <div v-if="task.trace_id" class="task-trace qv-mono" :title="task.trace_id">
                      trace {{ traceText(task.trace_id) }}
                    </div>
                  </td>
                  <td>
                    <div class="status-stack">
                      <n-tag size="small" round :bordered="false" :type="statusTagType(task.status)">
                        {{ TASK_STATUS_LABELS[task.status] }}
                      </n-tag>
                      <span>{{ taskStageLabel(task.stage) }}</span>
                      <span v-if="task.raw_status && task.raw_status !== task.status" class="raw-status">
                        原始 {{ task.raw_status }}
                      </span>
                    </div>
                  </td>
                  <td class="model-cell" :title="providerModel(task)">{{ providerModel(task) }}</td>
                  <td class="num-cell qv-tnum">{{ task.total_tokens ? task.total_tokens.toLocaleString() : '-' }}</td>
                  <td class="num-cell qv-tnum">{{ taskDurationText(task) }}</td>
                  <td class="progress-cell qv-tnum">{{ taskProgressText(task) }}</td>
                  <td class="advice-cell">
                    <div v-if="showTaskError(task) && task.error" class="task-error">{{ task.error }}</div>
                    <code v-if="task.error_code" class="error-code">{{ task.error_code }}</code>
                    <div class="recovery">{{ taskRecoveryAdvice(task) }}</div>
                  </td>
                  <td class="time-cell qv-tnum">{{ formatTaskTime(task.created_at) }}</td>
                  <td class="action-cell">
                    <n-button
                      v-if="taskActionLabel(task)"
                      size="tiny"
                      quaternary
                      type="primary"
                      @click="openTask(task)"
                    >
                      {{ taskActionLabel(task) }}
                    </n-button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-if="visibleTasks.length" class="mobile-list">
            <article v-for="task in visibleTasks" :key="task.id" class="mobile-task">
              <div class="mobile-task-head">
                <div class="mobile-task-title-wrap">
                  <div class="task-title">{{ task.title || taskKindLabel(task.kind) }}</div>
                  <div class="task-meta">
                    <span>{{ taskSourceLabel(task.source) }}</span>
                    <span>{{ taskKindLabel(task.kind) }}</span>
                    <span class="qv-mono">#{{ task.source_id }}</span>
                  </div>
                </div>
                <n-tag size="small" round :bordered="false" :type="statusTagType(task.status)">
                  {{ TASK_STATUS_LABELS[task.status] }}
                </n-tag>
              </div>
              <div v-if="task.target" class="mobile-target">{{ task.target }}</div>
              <dl class="mobile-facts">
                <div>
                  <dt>阶段</dt>
                  <dd>{{ taskStageLabel(task.stage) }}</dd>
                </div>
                <div>
                  <dt>耗时</dt>
                  <dd class="qv-tnum">{{ taskDurationText(task) }}</dd>
                </div>
                <div>
                  <dt>模型</dt>
                  <dd>{{ providerModel(task) }}</dd>
                </div>
                <div>
                  <dt>Token</dt>
                  <dd class="qv-tnum">{{ task.total_tokens ? task.total_tokens.toLocaleString() : '-' }}</dd>
                </div>
                <div v-if="task.total > 0">
                  <dt>进度</dt>
                  <dd class="qv-tnum">{{ taskProgressText(task) }}</dd>
                </div>
                <div>
                  <dt>时间</dt>
                  <dd class="qv-tnum">{{ formatTaskTime(task.created_at) }}</dd>
                </div>
              </dl>
              <div v-if="showTaskError(task) && task.error" class="task-error mobile-error">{{ task.error }}</div>
              <code v-if="task.error_code" class="error-code">{{ task.error_code }}</code>
              <p class="recovery mobile-recovery">{{ taskRecoveryAdvice(task) }}</p>
              <div class="mobile-task-actions">
                <span v-if="task.raw_status && task.raw_status !== task.status" class="raw-status">
                  原始状态 {{ task.raw_status }}
                </span>
                <n-button
                  v-if="taskActionLabel(task)"
                  size="small"
                  tertiary
                  type="primary"
                  @click="openTask(task)"
                >
                  {{ taskActionLabel(task) }}
                </n-button>
              </div>
            </article>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.task-page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.status-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
}

.status-stat {
  position: relative;
  display: flex;
  min-width: 0;
  min-height: 76px;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  box-sizing: border-box;
  padding: 14px 16px;
  overflow: hidden;
  border: 1px solid var(--task-border);
  border-radius: 8px;
  background: var(--task-card);
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: border-color 0.16s ease, background-color 0.16s ease;
}

.status-stat::before {
  position: absolute;
  top: 0;
  bottom: 0;
  left: 0;
  width: 3px;
  background: var(--status-color);
  content: '';
}

.status-stat:hover,
.status-stat.active {
  border-color: var(--status-color);
  background: color-mix(in srgb, var(--status-color) 7%, var(--task-card));
}

.status-stat:focus-visible {
  outline: 2px solid var(--status-color);
  outline-offset: 2px;
}

.status-stat-label {
  min-width: 0;
  overflow-wrap: anywhere;
  font-size: 13px;
  opacity: 0.72;
}

.status-stat-value {
  flex: 0 0 auto;
  color: var(--status-color);
  font-size: 26px;
  font-weight: 700;
  line-height: 1;
}

.filters {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 14px;
}

.filter-control {
  width: 170px;
}

.system-toggle {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  min-height: 28px;
  font-size: 13px;
  cursor: pointer;
}

.filter-count {
  margin-left: auto;
  font-size: 12px;
  color: var(--task-muted);
}

.load-alert {
  margin-bottom: 12px;
}

.task-empty {
  padding: 42px 0;
}

.task-table {
  width: 100%;
  min-width: 1240px;
  border-collapse: collapse;
  table-layout: fixed;
}

.task-table th,
.task-table td {
  padding: 10px 9px;
  border-bottom: 1px solid var(--task-divider);
  text-align: left;
  vertical-align: top;
}

.task-table th {
  color: var(--task-muted);
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}

.task-table td {
  font-size: 12px;
}

.task-table th:nth-child(1) { width: 230px; }
.task-table th:nth-child(2) { width: 104px; }
.task-table th:nth-child(3) { width: 130px; }
.task-table th:nth-child(4) { width: 72px; }
.task-table th:nth-child(5) { width: 72px; }
.task-table th:nth-child(6) { width: 90px; }
.task-table th:nth-child(7) { width: 260px; }
.task-table th:nth-child(8) { width: 150px; }
.task-table th:nth-child(9) { width: 118px; }

.task-table tbody tr:last-child td {
  border-bottom: 0;
}

.task-title {
  overflow: hidden;
  font-size: 13px;
  font-weight: 600;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.task-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 3px 8px;
  margin-top: 4px;
  color: var(--task-muted);
  font-size: 11px;
}

.task-trace {
  margin-top: 4px;
  overflow: hidden;
  color: var(--task-muted);
  font-size: 10px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.status-stack {
  display: flex;
  align-items: flex-start;
  flex-direction: column;
  gap: 4px;
}

.raw-status {
  color: var(--task-muted);
  font-size: 10px;
}

.model-cell {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.num-cell {
  text-align: right !important;
  white-space: nowrap;
}

.progress-cell,
.time-cell {
  white-space: nowrap;
}

.task-error {
  display: -webkit-box;
  overflow: hidden;
  color: var(--task-muted);
  line-height: 1.4;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}

.error-code {
  display: inline-block;
  max-width: 100%;
  margin-top: 4px;
  padding: 1px 4px;
  overflow: hidden;
  border: 1px solid var(--task-divider);
  border-radius: 4px;
  color: inherit;
  font-size: 10px;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.recovery {
  margin-top: 4px;
  color: var(--task-muted);
  font-size: 11px;
  line-height: 1.4;
}

.action-cell {
  text-align: right !important;
}

.mobile-list {
  display: none;
}

@media (max-width: 900px) {
  .status-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (max-width: 768px) {
  .filter-control {
    width: calc(50% - 5px);
  }

  .system-toggle {
    width: calc(50% - 5px);
  }

  .filter-count {
    margin-left: 0;
  }

  .desktop-list {
    display: none;
  }

  .mobile-list {
    display: block;
  }

  .mobile-task {
    padding: 14px 0;
    border-bottom: 1px solid var(--task-divider);
  }

  .mobile-task:first-child {
    padding-top: 2px;
  }

  .mobile-task:last-child {
    padding-bottom: 2px;
    border-bottom: 0;
  }

  .mobile-task-head {
    display: flex;
    min-width: 0;
    align-items: flex-start;
    justify-content: space-between;
    gap: 10px;
  }

  .mobile-task-title-wrap {
    min-width: 0;
  }

  .mobile-target {
    margin-top: 6px;
    overflow-wrap: anywhere;
    color: var(--task-muted);
    font-size: 12px;
  }

  .mobile-facts {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px 14px;
    margin: 12px 0 0;
  }

  .mobile-facts > div {
    min-width: 0;
  }

  .mobile-facts dt {
    margin: 0 0 2px;
    color: var(--task-muted);
    font-size: 10px;
  }

  .mobile-facts dd {
    margin: 0;
    overflow: hidden;
    font-size: 12px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .mobile-error {
    margin-top: 10px;
  }

  .mobile-recovery {
    margin: 8px 0 0;
  }

  .mobile-task-actions {
    display: flex;
    min-height: 28px;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    margin-top: 10px;
  }
}

@media (max-width: 420px) {
  .status-stat {
    min-height: 68px;
    padding: 12px;
  }

  .status-stat-value {
    font-size: 22px;
  }

  .filter-control,
  .system-toggle {
    width: 100%;
  }
}
</style>
