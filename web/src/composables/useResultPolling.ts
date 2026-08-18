import { onBeforeUnmount, ref } from 'vue'
import { isPollCancelled, pollUntil } from '@/lib/poll'

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

  async function track(id: number) {
    controller?.abort()
    const current = new AbortController()
    controller = current
    polling.value = true
    try {
      const value = await pollUntil(() => options.load(id), options.isDone, {
        signal: current.signal,
        timeoutMs: options.timeoutMs,
      })
      options.onResult(id, value)
      return value
    } catch (reason) {
      if (!isPollCancelled(reason)) options.onError(reason as Error)
      return null
    } finally {
      if (controller === current) {
        controller = null
        polling.value = false
        await options.onSettled?.()
      }
    }
  }

  function stop() {
    controller?.abort()
    controller = null
    polling.value = false
  }

  onBeforeUnmount(stop)
  return { polling, track, stop }
}
