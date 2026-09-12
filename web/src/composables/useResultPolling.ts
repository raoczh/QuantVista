import { onBeforeUnmount, ref } from 'vue'
import { isPollCancelled, pollUntil } from '@/lib/poll'
import { getSessionEpoch } from '@/api/token'

/** 页面刷新可恢复的业务结果轮询；只读取既有结果，不创建新任务。 */
export function useResultPolling<T>(options: {
  load: (id: number) => Promise<T>
  isDone: (value: T) => boolean
  onResult: (id: number, value: T) => void
  onError: (error: Error) => void
  onSettled?: () => void | Promise<void>
  timeoutMs?: number
}) {
  const polling = ref(false)
  let controller: AbortController | null = null
  let disposed = false

  async function track(id: number) {
    if (disposed) return null
    controller?.abort()
    const current = new AbortController()
    controller = current
    const session = getSessionEpoch()
    const owns = () => !disposed && controller === current && !current.signal.aborted && getSessionEpoch() === session
    polling.value = true
    try {
      const value = await pollUntil(() => options.load(id), options.isDone, {
        signal: current.signal,
        timeoutMs: options.timeoutMs,
      })
      if (!owns()) return null
      options.onResult(id, value)
      return value
    } catch (reason) {
      if (owns() && !isPollCancelled(reason)) options.onError(reason as Error)
      return null
    } finally {
      const notify = owns()
      if (controller === current) {
        controller = null
        polling.value = false
        if (notify && !disposed && !controller && getSessionEpoch() === session) await options.onSettled?.()
      }
    }
  }

  function stop() {
    controller?.abort()
    controller = null
    polling.value = false
  }

  onBeforeUnmount(() => { disposed = true; stop() })
  return { polling, track, stop }
}
