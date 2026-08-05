<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NEmpty, NPopover, NSpin, NTag } from 'naive-ui'
import {
  TASK_STATUS_LABELS,
  formatTaskCompactTime,
  listTasks,
  taskActionLabel,
  taskKindLabel,
  taskResultRoute,
  type TaskCenterItem,
  type TaskStatus,
} from '@/api/taskCenter'
import { isAbortError } from '@/api/client'
import { useVisibleTaskPolling } from '@/composables/useVisibleTaskPolling'
import { useUi, withAlpha } from '@/composables/useUi'

const router = useRouter()
const route = useRoute()
const { vars } = useUi()
const tasks = ref<TaskCenterItem[]>([])
const activeCount = ref(0)
const show = ref(false)
const loading = ref(false)
const refreshing = ref(false)
const loadError = ref('')
let requestController: AbortController | null = null

const recent = computed(() => tasks.value.slice(0, 3))
const hasProcessing = computed(() => activeCount.value > 0)
const badgeText = computed(() => (activeCount.value > 99 ? '99+' : String(activeCount.value)))
const processingSummary = computed(() => {
  if (!activeCount.value) return '当前无进行中任务'
  return activeCount.value >= 100 ? '至少 100 项进行中' : `${activeCount.value} 项进行中`
})
const styleVars = computed(() => ({
  '--recent-border': vars.value.dividerColor,
  '--recent-muted': vars.value.textColor3,
  '--recent-hover': withAlpha(vars.value.primaryColor, 0.08),
  '--recent-primary': vars.value.primaryColor,
}))

async function loadRecentTasks() {
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  if (!tasks.value.length) loading.value = true
  refreshing.value = true
  loadError.value = ''
  try {
    // queued/running 分开读取，避免较新的终态记录把较早的活跃作业挤出窗口。
    // 两个轻量摘要查询都不加载步骤或结果正文。
    const [rows, running, queued] = await Promise.all([
      listTasks({ limit: 3 }, controller.signal),
      listTasks({ status: 'running', limit: 100 }, controller.signal),
      listTasks({ status: 'queued', limit: 100 }, controller.signal),
    ])
    if (requestController === controller) {
      tasks.value = rows
      activeCount.value = running.length + queued.length
    }
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

const { refreshNow } = useVisibleTaskPolling(loadRecentTasks, () => hasProcessing.value, {
  activeIntervalMs: 4000,
  idleIntervalMs: 30_000,
  enabled: () => route.name !== 'tasks',
})

watch(show, (visible) => {
  if (visible) void refreshNow()
})

onBeforeUnmount(() => requestController?.abort())

function statusTagType(value: TaskStatus): 'info' | 'success' | 'warning' | 'error' {
  if (value === 'queued' || value === 'running') return 'info'
  if (value === 'success') return 'success'
  if (value === 'degraded' || value === 'canceled') return 'warning'
  return 'error'
}

function openTask(task: TaskCenterItem) {
  const target = taskResultRoute(task)
  if (!target) return
  show.value = false
  void router.push(target)
}

function openAll() {
  show.value = false
  void router.push({ name: 'tasks' })
}
</script>

<template>
  <n-popover
    v-model:show="show"
    trigger="click"
    placement="bottom-end"
    :show-arrow="false"
    :z-index="2100"
    :content-style="{ padding: '0' }"
  >
    <template #trigger>
      <button
        type="button"
        class="recent-trigger"
        :class="{ running: hasProcessing, refreshing }"
        :aria-label="activeCount ? `最近任务，${processingSummary}` : '最近任务'"
        title="最近任务"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
          <circle cx="12" cy="12" r="8" />
          <path d="M12 7v5l3 2" />
        </svg>
        <span v-if="activeCount" class="running-badge qv-tnum" aria-hidden="true">{{ badgeText }}</span>
      </button>
    </template>

    <section class="recent-panel" :style="styleVars" aria-label="最近任务">
      <header class="recent-head">
        <div>
          <div class="recent-title">最近任务</div>
          <div class="recent-summary qv-tnum">
            {{ processingSummary }}
          </div>
        </div>
        <n-button size="tiny" quaternary :loading="refreshing" @click="refreshNow()">刷新</n-button>
      </header>

      <div v-if="loadError" class="recent-error">
        <span>任务列表暂不可用</span>
        <n-button size="tiny" quaternary type="primary" @click="refreshNow()">重新读取</n-button>
      </div>

      <n-spin :show="loading">
        <n-empty v-if="!recent.length && !loading" size="small" description="还没有任务记录" class="recent-empty" />
        <div v-else class="recent-list">
          <button
            v-for="task in recent"
            :key="task.id"
            type="button"
            class="recent-row"
            :disabled="!taskResultRoute(task)"
            :title="taskActionLabel(task) || undefined"
            @click="openTask(task)"
          >
            <span class="recent-row-main">
              <span class="recent-row-title">{{ task.title || taskKindLabel(task.kind) }}</span>
              <span class="recent-row-meta">
                <span>{{ taskKindLabel(task.kind) }}</span>
                <span class="qv-tnum">{{ formatTaskCompactTime(task.created_at) }}</span>
              </span>
            </span>
            <n-tag size="tiny" round :bordered="false" :type="statusTagType(task.status)">
              {{ TASK_STATUS_LABELS[task.status] }}
            </n-tag>
          </button>
        </div>
      </n-spin>

      <footer class="recent-foot">
        <button type="button" class="view-all" @click="openAll">
          <span>查看全部</span>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
            <path d="m9 18 6-6-6-6" />
          </svg>
        </button>
      </footer>
    </section>
  </n-popover>
</template>

<style scoped>
.recent-trigger {
  position: relative;
  display: inline-flex;
  width: 32px;
  height: 32px;
  flex: 0 0 32px;
  align-items: center;
  justify-content: center;
  box-sizing: border-box;
  padding: 0;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  transition: background-color 0.15s ease, color 0.15s ease;
}

.recent-trigger:hover,
.recent-trigger:active {
  background: var(--qv-menu-hover);
}

.recent-trigger.running {
  color: var(--qv-menu-active-text);
}

.recent-trigger:focus-visible {
  outline: 2px solid var(--qv-menu-active-text);
  outline-offset: 2px;
}

.recent-trigger svg {
  width: 18px;
  height: 18px;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.running-badge {
  position: absolute;
  top: -3px;
  right: -4px;
  min-width: 15px;
  height: 15px;
  box-sizing: border-box;
  padding: 0 3px;
  border: 1px solid var(--qv-header-bg);
  border-radius: 8px;
  background: var(--qv-menu-active-text);
  color: #fff;
  font-size: 9px;
  font-weight: 700;
  line-height: 13px;
  text-align: center;
  white-space: nowrap;
}

.recent-panel {
  width: min(360px, calc(100vw - 16px));
  max-height: min(520px, calc(100vh - 84px));
  box-sizing: border-box;
  overflow: auto;
}

.recent-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border-bottom: 1px solid var(--recent-border);
}

.recent-title {
  font-size: 14px;
  font-weight: 600;
  line-height: 1.35;
}

.recent-summary {
  margin-top: 2px;
  color: var(--recent-muted);
  font-size: 11px;
}

.recent-error {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 8px 14px;
  border-bottom: 1px solid var(--recent-border);
  color: var(--recent-muted);
  font-size: 12px;
}

.recent-empty {
  padding: 28px 12px;
}

.recent-list {
  display: flex;
  flex-direction: column;
}

.recent-row {
  display: flex;
  width: 100%;
  min-width: 0;
  min-height: 58px;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  box-sizing: border-box;
  padding: 10px 14px;
  border: 0;
  border-bottom: 1px solid var(--recent-border);
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
  transition: background-color 0.15s ease;
}

.recent-row:hover {
  background: var(--recent-hover);
}

.recent-row:focus-visible {
  outline: 2px solid var(--recent-primary);
  outline-offset: -2px;
}

.recent-row:disabled {
  cursor: default;
  opacity: 0.7;
}

.recent-row-main {
  display: block;
  min-width: 0;
}

.recent-row-title {
  display: block;
  overflow: hidden;
  font-size: 13px;
  font-weight: 500;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.recent-row-meta {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
  overflow: hidden;
  color: var(--recent-muted);
  font-size: 11px;
  white-space: nowrap;
}

.recent-row-meta span:first-child {
  overflow: hidden;
  text-overflow: ellipsis;
}

.recent-foot {
  padding: 7px;
}

.view-all {
  display: flex;
  width: 100%;
  min-height: 32px;
  align-items: center;
  justify-content: center;
  gap: 4px;
  box-sizing: border-box;
  padding: 4px 10px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--recent-primary);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}

.view-all:hover,
.view-all:focus-visible {
  background: var(--recent-hover);
}

.view-all svg {
  width: 14px;
  height: 14px;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}

@media (max-width: 768px) {
  .recent-panel {
    max-height: calc(100vh - 130px - env(safe-area-inset-bottom, 0px));
  }
}

@media (prefers-reduced-motion: reduce) {
  .recent-trigger,
  .recent-row {
    transition: none;
  }
}
</style>
