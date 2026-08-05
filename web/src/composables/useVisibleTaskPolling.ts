import { onBeforeUnmount, onMounted, watch } from 'vue'

interface VisibleTaskPollingOptions {
  activeIntervalMs?: number
  idleIntervalMs?: number
  enabled?: () => boolean
}

/**
 * 页面可见时串行刷新任务摘要：有运行任务用高频间隔，无运行任务可选低频探测。
 * setTimeout 在请求完成后才续排，避免慢请求叠加；隐藏与卸载都会清理定时器。
 */
export function useVisibleTaskPolling(
  refresh: () => void | Promise<unknown>,
  hasProcessing: () => boolean,
  options: VisibleTaskPollingOptions = {},
) {
  const activeIntervalMs = options.activeIntervalMs ?? 4000
  const idleIntervalMs = options.idleIntervalMs ?? 0
  let timer: number | undefined
  let inFlight: Promise<void> | null = null
  let rerun = false
  let mounted = false

  function clearTimer() {
    if (timer !== undefined) {
      window.clearTimeout(timer)
      timer = undefined
    }
  }

  function nextDelay() {
    if (!mounted || document.visibilityState !== 'visible' || options.enabled?.() === false) return 0
    return hasProcessing() ? activeIntervalMs : idleIntervalMs
  }

  function schedule() {
    clearTimer()
    const delay = nextDelay()
    if (delay > 0) timer = window.setTimeout(() => void refreshNow(), delay)
  }

  function refreshNow(): Promise<void> {
    clearTimer()
    if (inFlight) {
      rerun = true
      return inFlight
    }

    const run = Promise.resolve(refresh())
      .then(() => undefined)
      .finally(() => {
        if (inFlight !== run) return
        inFlight = null
        if (!mounted) return
        if (rerun && document.visibilityState === 'visible') {
          rerun = false
          void refreshNow()
          return
        }
        rerun = false
        schedule()
      })
    inFlight = run
    return run
  }

  function onVisibilityChange() {
    clearTimer()
    if (document.visibilityState === 'visible' && options.enabled?.() !== false) void refreshNow()
  }

  watch(hasProcessing, schedule)
  if (options.enabled) {
    watch(options.enabled, (enabled) => {
      clearTimer()
      if (enabled && mounted && document.visibilityState === 'visible') void refreshNow()
    })
  }

  onMounted(() => {
    mounted = true
    document.addEventListener('visibilitychange', onVisibilityChange)
    if (options.enabled?.() !== false) void refreshNow()
  })

  onBeforeUnmount(() => {
    mounted = false
    rerun = false
    clearTimer()
    document.removeEventListener('visibilitychange', onVisibilityChange)
  })

  return { refreshNow }
}
