// pollUntil 固定间隔轮询 fetch 直到 done 为真或超时，返回最后一次结果。
// 用于异步生成任务（收盘日报/推荐）的状态跟踪：后端立即返回 processing 记录，
// 前端轮询详情直到脱离 processing——浏览器超时/反代掐断/页面刷新都不再中断任务，
// 刷新后凭列表里的 processing 记录可恢复跟踪。

// 轮询被主动取消（组件卸载/离开页面）时抛出，与真正的失败区分：调用方 catch 到它
// 应静默返回，不弹错误提示。
export class PollCancelled extends Error {
  constructor() {
    super('poll cancelled')
    this.name = 'PollCancelled'
  }
}

export function isPollCancelled(e: unknown): boolean {
  return e instanceof PollCancelled
}

export async function pollUntil<T>(
  fetch: () => Promise<T>,
  done: (v: T) => boolean,
  opts: { intervalMs?: number; timeoutMs?: number; signal?: AbortSignal } = {},
): Promise<T> {
  const interval = opts.intervalMs ?? 2500
  const timeout = opts.timeoutMs ?? 8 * 60 * 1000 // 与后端任务 deadline 同量级，超时后端会自行判 failed
  const signal = opts.signal
  const start = Date.now()
  for (;;) {
    if (signal?.aborted) throw new PollCancelled()
    const v = await fetch()
    if (signal?.aborted) throw new PollCancelled()
    if (done(v)) return v
    if (Date.now() - start > timeout) {
      throw new Error('任务执行超时：请稍后刷新页面查看结果')
    }
    // 可中断的间隔等待：abort 时立即结束等待并在下一轮抛 PollCancelled。
    await new Promise<void>((resolve) => {
      const timer = setTimeout(() => {
        signal?.removeEventListener('abort', onAbort)
        resolve()
      }, interval)
      const onAbort = () => {
        clearTimeout(timer)
        resolve()
      }
      signal?.addEventListener('abort', onAbort, { once: true })
    })
  }
}
