import { onMounted, onUnmounted } from 'vue'

// A 股交易时段：周一~周五 09:15–11:30 与 13:00–15:05（含竞价与收盘尾差）；
// 午休 11:30–13:00 不交易，行情不动，无需轮询。
// 不考虑法定节假日——非交易日轮询会拿到旧数据但无害，自用场景可接受。
export function isTradingTime(d = new Date()): boolean {
  const day = d.getDay()
  if (day === 0 || day === 6) return false
  const mins = d.getHours() * 60 + d.getMinutes()
  const morning = mins >= 9 * 60 + 15 && mins <= 11 * 60 + 30
  const afternoon = mins >= 13 * 60 && mins <= 15 * 60 + 5
  return morning || afternoon
}

/**
 * 盘中自动刷新：仅「交易时段 + 页面可见」时轮询，切后台自动暂停，
 * 切回前台若在盘中立即补一次。数据源有限流，间隔不要低于 60s。
 */
export function useAutoRefresh(fn: () => void | Promise<unknown>, intervalMs = 60_000) {
  let timer: number | undefined

  function tick() {
    if (document.visibilityState === 'visible' && isTradingTime()) void fn()
  }
  function onVisibility() {
    if (document.visibilityState === 'visible' && isTradingTime()) void fn()
  }

  onMounted(() => {
    timer = window.setInterval(tick, intervalMs)
    document.addEventListener('visibilitychange', onVisibility)
  })
  onUnmounted(() => {
    if (timer !== undefined) clearInterval(timer)
    document.removeEventListener('visibilitychange', onVisibility)
  })
}
