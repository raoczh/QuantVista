import { onBeforeUnmount, onMounted, watch } from 'vue'
import { refreshAccessToken } from '@/api/client'
import { getAccessToken } from '@/api/token'

interface VisibleTaskPollingOptions {
  activeIntervalMs?: number
  idleIntervalMs?: number
  enabled?: () => boolean
  events?: boolean
  safetyIntervalMs?: number
}

const LAST_TASK_EVENT_ID_KEY = 'qv-task-last-event-id'

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
  let streamConnected = false
  let streamController: AbortController | null = null
  let reconnectTimer: number | undefined
  let reconnectAttempt = 0

  function clearTimer() {
    if (timer !== undefined) {
      window.clearTimeout(timer)
      timer = undefined
    }
  }

  function nextDelay() {
    if (!mounted || document.visibilityState !== 'visible' || options.enabled?.() === false) return 0
    if (hasProcessing()) return activeIntervalMs
    if (streamConnected) return options.safetyIntervalMs ?? 30_000
    return idleIntervalMs
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
    if (document.visibilityState === 'visible' && options.enabled?.() !== false) {
      void refreshNow()
      startEventStream()
    } else {
      stopEventStream()
    }
  }

  function readLastEventID(): string {
    try {
      const value = sessionStorage.getItem(LAST_TASK_EVENT_ID_KEY) || ''
      return /^\d+$/.test(value) ? value : ''
    } catch {
      return ''
    }
  }

  function rememberEventID(value: string) {
    if (!/^\d+$/.test(value)) return
    try {
      sessionStorage.setItem(LAST_TASK_EVENT_ID_KEY, value)
    } catch {
      // sessionStorage 不可用不影响当前连接内续传。
    }
  }

  function clearReconnectTimer() {
    if (reconnectTimer !== undefined) {
      window.clearTimeout(reconnectTimer)
      reconnectTimer = undefined
    }
  }

  function stopEventStream() {
    clearReconnectTimer()
    streamConnected = false
    streamController?.abort()
    streamController = null
    schedule()
  }

  function scheduleReconnect() {
    clearReconnectTimer()
    if (!mounted || document.visibilityState !== 'visible' || options.enabled?.() === false) return
    const delay = Math.min(15_000, 1000 * 2 ** Math.min(reconnectAttempt, 4))
    reconnectAttempt++
    reconnectTimer = window.setTimeout(() => startEventStream(), delay)
  }

  function processEventBlock(block: string) {
    let eventID = ''
    let hasData = false
    for (const rawLine of block.split('\n')) {
      const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
      if (line.startsWith(':')) continue
      if (line.startsWith('id:')) eventID = line.slice(3).trim()
      if (line.startsWith('data:')) hasData = true
    }
    if (eventID) rememberEventID(eventID)
    if (hasData) void refreshNow()
  }

  async function openEventStream(controller: AbortController, retried = false): Promise<void> {
    const headers = new Headers({ Accept: 'text/event-stream' })
    const token = getAccessToken()
    if (token) headers.set('Authorization', `Bearer ${token}`)
    const lastEventID = readLastEventID()
    if (lastEventID) headers.set('Last-Event-ID', lastEventID)

    const response = await fetch('/api/tasks/events', { headers, signal: controller.signal })
    if (response.status === 401 && !retried && (await refreshAccessToken())) {
      return openEventStream(controller, true)
    }
    if (!response.ok || !response.body) throw new Error(`任务事件流连接失败 (${response.status})`)

    streamConnected = true
    reconnectAttempt = 0
    schedule()
    const reader = response.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      buffer = buffer.replace(/\r\n/g, '\n')
      let boundary = buffer.indexOf('\n\n')
      while (boundary >= 0) {
        processEventBlock(buffer.slice(0, boundary))
        buffer = buffer.slice(boundary + 2)
        boundary = buffer.indexOf('\n\n')
      }
    }
    buffer += decoder.decode()
    if (buffer.trim()) processEventBlock(buffer)
  }

  function startEventStream() {
    if (options.events === false || streamController || !mounted || document.visibilityState !== 'visible') return
    if (options.enabled?.() === false) return
    clearReconnectTimer()
    const controller = new AbortController()
    streamController = controller
    void openEventStream(controller)
      .catch(() => undefined)
      .finally(() => {
        if (streamController !== controller) return
        streamController = null
        streamConnected = false
        schedule()
        if (!controller.signal.aborted) scheduleReconnect()
      })
  }

  watch(hasProcessing, schedule)
  if (options.enabled) {
    watch(options.enabled, (enabled) => {
      clearTimer()
      if (enabled && mounted && document.visibilityState === 'visible') {
        void refreshNow()
        startEventStream()
      } else if (!enabled) {
        stopEventStream()
      }
    })
  }

  onMounted(() => {
    mounted = true
    document.addEventListener('visibilitychange', onVisibilityChange)
    if (options.enabled?.() !== false) {
      void refreshNow()
      startEventStream()
    }
  })

  onBeforeUnmount(() => {
    mounted = false
    rerun = false
    clearTimer()
    stopEventStream()
    document.removeEventListener('visibilitychange', onVisibilityChange)
  })

  return { refreshNow }
}
