import { expect, test } from '@playwright/test'
import { demoTime, mockApi } from './mock-api.mjs'

test('空库可预演日历回填并从管理页查看失败任务详情', async ({ page }, info) => {
  const planHash = 'a'.repeat(64)
  const api = await mockApi(page, {
    theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue',
    responses: {
      '/admin/data-health': { generated_at: demoTime, window_days: 45, query_hard_max: 60, items: [] },
      '/admin/market/backfill-calendar': (_url: URL, request: { postDataJSON: () => Record<string, unknown> }) => {
        const body = request.postDataJSON()
        if (body.dry_run) {
          expect(body.from, '空库不能依赖健康报告先返回日期').toBeUndefined()
          expect(body.to).toBeUndefined()
          return { dry_run: true, plan: {
            task: 'backfill_calendar', market: 'cn', from: '2026-06-16', to: '2026-09-15', window_days: 92,
            target_count: 92, expected_count: 92, existing_count: 0, missing_count: 92, suspended_count: 0,
            estimated_requests: 1, capped: false, difference_summary: '核对最近 92 个自然日，包含今天',
            plan_hash: planHash, generated_at: demoTime,
          } }
        }
        expect(body.plan_hash).toBe(planHash)
        expect(body.to).toBe('2026-09-15')
        return { started: true, task: 'backfill_calendar', market: 'cn', job_run_id: 710 }
      },
      '/tasks/710': { id: 710, kind: 'backfill_calendar', status: 'failed', error: '日历主备源暂不可用，请检查容器出网', cancel_requested: false, created_at: demoTime, updated_at: demoTime },
      '/tasks': (url: URL) => url.searchParams.get('job_id') === '710' && url.searchParams.get('include_system') === '1' ? [{
        id: 'job:710', source: 'data_sync', source_id: 710, owner: 'system', triggered_by: 9001,
        kind: 'backfill_calendar', title: '日历回填任务', target: 'cn', status: 'failed', raw_status: 'failed', stage: 'finished',
        error: '日历主备源暂不可用，请检查容器出网', error_code: '', provider: '', model: '', trace_id: '',
        prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, latency_ms: 5, total: 1, succeeded: 0, failed: 1,
        can_cancel: false, can_retry: false, cancel_requested: false, created_at: demoTime, updated_at: demoTime,
      }] : [],
    },
  })
  await page.goto('/admin', { waitUntil: 'networkidle' })
  expect(api.mutations).toEqual([])
  await page.getByRole('button', { name: '回填日历', exact: true }).click()
  await expect(page.getByText('核对最近 92 个自然日，包含今天')).toBeVisible()
  await page.getByRole('button', { name: '确认执行', exact: true }).click()
  await expect(page.getByText('维护任务 #710')).toBeVisible()
  await expect(page.getByText('日历主备源暂不可用，请检查容器出网')).toBeVisible()
  const taskDetailsLink = page.getByRole('link', { name: '查看任务详情', exact: true })
  await expect(taskDetailsLink).toHaveAttribute('href', /job_id=710/)
  await expect(taskDetailsLink).toHaveAttribute('href', /include_system=1/)
  await taskDetailsLink.click()
  await expect(page).toHaveURL(/\/tasks\?.*job_id=710/)
  await expect(page.locator('.task-title:visible')).toHaveText(['日历回填任务'])
  await expect(page.getByRole('switch')).toHaveCount(1)
  await expect(page.getByRole('switch')).toBeChecked()
  await expect(page.locator('.task-error:visible')).toHaveText(['日历主备源暂不可用，请检查容器出网'])
  expect(api.mutations).toHaveLength(2)
  expect([...new Set(api.unknown)]).toEqual([])
})

test('新浪热力图与详情明确区分分类及不支持能力', async ({ page }, info) => {
  const boardCode = 'sina:new_bank'
  const api = await mockApi(page, {
    theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue',
    responses: {
      '/markets/cn/boards': [{ code: boardCode, name: '示例银行行业', change_pct: 1.2, amount: 1e9, advances: 0, declines: 0, counts_available: false, member_count: 42, leader: '示例银行', leader_code: '000001', source: 'sina' }],
      [`/markets/cn/boards/${boardCode}`]: { code: boardCode, source: 'sina', bars: [], stocks: [
        { symbol: '000001', name: '示例银行', price: 12.35, change_pct: 1.2, amount: 1e8, turnover_rate: 1.5, total_cap: 1e10, float_cap: 1e10, is_leader: true, is_top_gainer: true, source: 'sina', data_time: demoTime },
      ], errors: { bars: '当前分类源不提供对应板块指数日线，不能用其他分类的指数代替' }, data_time: demoTime, captured_at: demoTime },
    },
  })
  await page.goto('/heatmap', { waitUntil: 'networkidle' })
  await expect(page.getByText(/当前使用新浪备用分类/)).toBeVisible()
  await page.locator('.heatmap-chart canvas').click({ position: { x: 80, y: 70 } })
  await expect(page).toHaveURL(/boards\/sina:new_bank/)
  await expect(page.getByText(/当前为新浪分类/)).toBeVisible()
  await expect(page.getByText('当前分类源不提供对应板块指数日线，不能用其他分类的指数代替')).toBeVisible()
  await expect(page.getByText('新浪分类暂不提供对应板块资金流，不使用东财同名板块替代')).toBeVisible()
  expect(api.requests.filter((path: string) => path.includes('/boards/') && path.endsWith('/fundflow'))).toEqual([])
  expect(api.mutations).toEqual([])
  expect([...new Set(api.unknown)]).toEqual([])
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1)
})

test('管理报告读取失败不能显示成所有来源断网', async ({ page }, info) => {
  await mockApi(page, { theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue', errors: ['/admin/data-health'] })
  await page.goto('/admin', { waitUntil: 'networkidle' })
  await expect(page.getByText(/数据健康报告读取失败/)).toBeVisible()
  await expect(page.getByText(/不应判断为所有数据源断网/)).toBeVisible()
})
