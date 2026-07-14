// pollUntil 固定间隔轮询 fetch 直到 done 为真或超时，返回最后一次结果。
// 用于异步生成任务（收盘日报/推荐）的状态跟踪：后端立即返回 processing 记录，
// 前端轮询详情直到脱离 processing——浏览器超时/反代掐断/页面刷新都不再中断任务，
// 刷新后凭列表里的 processing 记录可恢复跟踪。
export async function pollUntil<T>(
  fetch: () => Promise<T>,
  done: (v: T) => boolean,
  opts: { intervalMs?: number; timeoutMs?: number } = {},
): Promise<T> {
  const interval = opts.intervalMs ?? 2500
  const timeout = opts.timeoutMs ?? 8 * 60 * 1000 // 与后端任务 deadline 同量级，超时后端会自行判 failed
  const start = Date.now()
  for (;;) {
    const v = await fetch()
    if (done(v)) return v
    if (Date.now() - start > timeout) {
      throw new Error('任务执行超时：请稍后刷新页面查看结果')
    }
    await new Promise((r) => setTimeout(r, interval))
  }
}
