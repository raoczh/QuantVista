import { onBeforeUnmount, ref, watch, type Ref } from 'vue'
import {
  cancelJob,
  getJob,
  listTasks,
  retryJob,
  type JobRun,
  type TaskCenterItem,
} from '@/api/taskCenter'
import { isAbortError } from '@/api/client'

/**
 * 读取某个业务结果对应的统一作业事实。这里只做 GET；取消和重试必须由页面按钮显式调用。
 */
export function useBusinessTask(kind: 'recommendation' | 'analysis', resultID: Ref<number | null>) {
  const task = ref<TaskCenterItem | null>(null)
  const loading = ref(false)
  const actionLoading = ref(false)
  const error = ref('')
  let controller: AbortController | null = null

  async function refresh() {
    controller?.abort()
    const id = resultID.value
    if (!id) {
      task.value = null
      return
    }
    const current = new AbortController()
    controller = current
    loading.value = true
    try {
      const rows = await listTasks({ source: 'job', kind, limit: 50, include_steps: true }, current.signal)
      task.value = rows.find((row) => row.result_id === id) || null
      error.value = ''
    } catch (reason) {
      if (!isAbortError(reason)) error.value = (reason as Error).message || '任务状态读取失败'
    } finally {
      if (controller === current) {
        controller = null
        loading.value = false
      }
    }
  }

  async function cancel(): Promise<JobRun | null> {
    if (!task.value?.can_cancel || actionLoading.value) return null
    actionLoading.value = true
    try {
      const updated = await cancelJob(task.value.source_id)
      await refresh()
      return updated
    } finally {
      actionLoading.value = false
    }
  }

  async function retry(): Promise<JobRun | null> {
    if (!task.value?.can_retry || actionLoading.value) return null
    actionLoading.value = true
    try {
      const rerun = await retryJob(task.value.source_id)
      return rerun.id > 0 ? await getJob(rerun.id) : rerun
    } finally {
      actionLoading.value = false
    }
  }

  watch(resultID, () => void refresh(), { immediate: true })
  onBeforeUnmount(() => controller?.abort())

  return { task, loading, actionLoading, error, refresh, cancel, retry }
}
