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
  let disposed = false

  async function refresh() {
    controller?.abort()
    controller = null
    task.value = null
    error.value = ''
    loading.value = false
    const id = resultID.value
    if (!id || disposed) return
    const current = new AbortController()
    controller = current
    loading.value = true
    try {
      const rows = await listTasks({ source: 'job', kind, limit: 50, include_steps: true }, current.signal)
      if (controller !== current || resultID.value !== id || disposed) return
      task.value = rows.find((row) => row.result_id === id) || null
      error.value = ''
    } catch (reason) {
      if (controller === current && !disposed && !isAbortError(reason)) error.value = (reason as Error).message || '任务状态读取失败'
    } finally {
      if (controller === current) {
        controller = null
        loading.value = false
      }
    }
  }

  async function cancel(): Promise<JobRun | null> {
    const target = task.value
    if (!target?.can_cancel || target.result_id !== resultID.value || actionLoading.value || disposed) return null
    actionLoading.value = true
    try {
      const updated = await cancelJob(target.source_id)
      if (target.result_id !== resultID.value || disposed) return null
      await refresh()
      return target.result_id === resultID.value && !disposed ? updated : null
    } finally {
      actionLoading.value = false
    }
  }

  async function retry(): Promise<JobRun | null> {
    const target = task.value
    if (!target?.can_retry || target.result_id !== resultID.value || actionLoading.value || disposed) return null
    actionLoading.value = true
    try {
      const rerun = await retryJob(target.source_id)
      if (target.result_id !== resultID.value || disposed) return null
      const updated = rerun.id > 0 ? await getJob(rerun.id) : rerun
      return target.result_id === resultID.value && !disposed ? updated : null
    } finally {
      actionLoading.value = false
    }
  }

  watch(resultID, () => void refresh(), { immediate: true, flush: 'sync' })
  onBeforeUnmount(() => {
    disposed = true
    controller?.abort()
    controller = null
  })

  return { task, loading, actionLoading, error, refresh, cancel, retry }
}
