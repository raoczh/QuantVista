<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NButton, NPopconfirm, NSpin, NTag } from 'naive-ui'
import {
  JOB_STEP_LABELS,
  taskRecoveryAdvice,
  type TaskCenterItem,
  type TaskStatus,
} from '@/api/taskCenter'
import SectionCard from '@/components/SectionCard.vue'

const props = defineProps<{
  task: TaskCenterItem | null
  loading?: boolean
  actionLoading?: boolean
  error?: string
}>()
const emit = defineEmits<{
  (event: 'cancel'): void
  (event: 'retry'): void
  (event: 'refresh'): void
  (event: 'audit'): void
}>()

const statusMeta: Record<TaskStatus, { label: string; type: 'default' | 'info' | 'success' | 'warning' | 'error' }> = {
  queued: { label: '排队中', type: 'info' },
  running: { label: '运行中', type: 'info' },
  success: { label: '成功', type: 'success' },
  degraded: { label: '部分成功', type: 'warning' },
  failed: { label: '失败', type: 'error' },
  canceled: { label: '已取消', type: 'default' },
}
const status = computed(() => props.task ? statusMeta[props.task.status] : null)
const missingFacts = computed(() => {
  if (!props.task || props.task.failed <= 0) return ''
  return `${props.task.failed} 个步骤或数据项未完成，${props.task.succeeded}/${props.task.total || props.task.succeeded + props.task.failed} 可用。`
})
</script>

<template>
  <SectionCard title="任务状态" class="task-panel">
    <template #extra>
      <n-button size="tiny" quaternary :loading="loading" @click="emit('refresh')">刷新状态</n-button>
      <n-button size="tiny" quaternary @click="emit('audit')">查看调用审计</n-button>
    </template>
    <n-alert v-if="error" type="warning" :bordered="false">{{ error }}，已有页面数据仍保留。</n-alert>
    <n-spin :show="!!loading">
      <div v-if="!task" class="not-started">
        <n-tag size="small" :bordered="false">未开始</n-tag>
        <span>只有点击生成或分析按钮才会创建 AI 任务。</span>
      </div>
      <div v-else class="task-content">
        <div class="task-head">
          <n-tag size="small" round :bordered="false" :type="status?.type">{{ status?.label }}</n-tag>
          <span class="task-id qv-mono">作业 #{{ task.source_id }}</span>
          <span v-if="task.cancel_requested">正在收敛取消请求</span>
        </div>
        <ol v-if="task.steps?.length" class="steps">
          <li v-for="step in task.steps" :key="step.id">
            <span>{{ JOB_STEP_LABELS[step.name] || step.name }}</span>
            <n-tag size="tiny" :bordered="false">{{ statusMeta[step.status]?.label || step.status }}</n-tag>
          </li>
        </ol>
        <n-alert v-if="task.status === 'degraded' || task.status === 'failed'" :type="task.status === 'failed' ? 'error' : 'warning'" :bordered="false">
          <div>{{ task.error || (task.status === 'degraded' ? '部分数据未能取得，已保留可用结果。' : '任务未完成。') }}</div>
          <div v-if="missingFacts">{{ missingFacts }}</div>
          <div>{{ taskRecoveryAdvice(task) }}</div>
          <code v-if="task.error_code" class="error-code">{{ task.error_code }}</code>
        </n-alert>
        <div v-else class="recovery">{{ taskRecoveryAdvice(task) }}</div>
        <div class="actions">
          <n-popconfirm v-if="task.can_cancel" @positive-click="emit('cancel')">
            <template #trigger><n-button size="small" :loading="actionLoading">取消任务</n-button></template>
            取消只停止当前后台作业，不会删除已有历史结果。
          </n-popconfirm>
          <n-button v-if="task.can_retry" size="small" type="primary" :loading="actionLoading" @click="emit('retry')">重试任务</n-button>
        </div>
      </div>
    </n-spin>
  </SectionCard>
</template>

<style scoped>
.not-started,
.task-head,
.actions {
  display: flex;
  min-width: 0;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.not-started,
.recovery,
.task-id {
  color: var(--qv-task-muted, inherit);
  font-size: 12px;
}
.task-content {
  display: grid;
  gap: 10px;
}
.steps {
  display: flex;
  margin: 0;
  padding: 0;
  flex-wrap: wrap;
  gap: 6px 12px;
  list-style: none;
}
.steps li {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
}
.error-code {
  display: inline-block;
  margin-top: 5px;
  font-size: 11px;
}
@media (max-width: 480px) {
  .actions > * {
    flex: 1 1 auto;
  }
}
</style>
