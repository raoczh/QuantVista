import { expect, test } from '@playwright/test'
import { demoTime, mockApi } from './mock-api.mjs'

const maintenanceTask = {
  id: 'job:710', source: 'data_sync', source_id: 710, kind: 'backfill_calendar', owner: 'system', triggered_by: 9001,
  title: '日历补采测试任务', target: 'cn', status: 'failed', raw_status: 'failed', stage: 'finished',
  error: '计划已变化，请重新预演', error_code: '', provider: '', model: '', prompt_tokens: 0, completion_tokens: 0,
  total_tokens: 0, latency_ms: 5, trace_id: '', total: 1, succeeded: 0, failed: 1,
  can_cancel: false, can_retry: false, cancel_requested: false, created_at: demoTime, updated_at: demoTime,
  steps: [{ id: 71, sequence: 1, name: 'execute', status: 'failed' }],
}

test('管理员深链定位系统补采任务并要求重新预演', async ({ page }, info) => {
  const focusedRequests: URL[] = []
  const api = await mockApi(page, {
    theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue',
    responses: {
      '/tasks': (url: URL) => {
        if (!url.searchParams.has('job_id')) return []
        focusedRequests.push(url)
        return [
          maintenanceTask,
          { ...maintenanceTask, id: 'data_sync:710', title: '同号旧日志不应混入' },
          { ...maintenanceTask, id: 'job:711', source_id: 711, title: '其他系统任务不应混入' },
        ]
      },
    },
  })
  await page.goto('/tasks?job_id=710&include_system=1', { waitUntil: 'networkidle' })
  await expect(page.getByRole('switch')).toBeChecked()
  await expect(page.locator('.task-title:visible')).toHaveText(['日历补采测试任务'])
  await expect(page.getByText('同号旧日志不应混入')).toHaveCount(0)
  await expect(page.getByText('其他系统任务不应混入')).toHaveCount(0)
  await expect(page.locator('.recovery:visible')).toContainText('返回管理后台重新预演')
  await expect(page.getByRole('button', { name: '重跑', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: '返回管理后台处理', exact: true })).toBeVisible()
  expect(focusedRequests.length).toBeGreaterThan(0)
  expect(focusedRequests.every(url => url.searchParams.get('job_id') === '710'
    && url.searchParams.get('include_system') === '1' && url.searchParams.get('include_steps') === '1')).toBe(true)

  await page.getByRole('switch').uncheck()
  await expect(page.getByText('当前筛选下没有任务')).toBeVisible()
  await expect.poll(() => focusedRequests.at(-1)?.searchParams.get('include_system')).toBeNull()
  expect(api.mutations).toEqual([])
  expect([...new Set(api.unknown)]).toEqual([])
})

test('普通用户深链不能打开系统任务开关或显示系统任务', async ({ page }, info) => {
  const focusedRequests: URL[] = []
  const api = await mockApi(page, {
    role: 'user',
    theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue',
    responses: {
      '/tasks': (url: URL) => {
        if (!url.searchParams.has('job_id')) return []
        focusedRequests.push(url)
        return [maintenanceTask]
      },
    },
  })
  await page.goto('/tasks?job_id=710&include_system=1', { waitUntil: 'networkidle' })
  await expect(page.getByRole('switch')).toHaveCount(0)
  await expect(page.getByText('当前筛选下没有任务')).toBeVisible()
  await expect(page.getByText('日历补采测试任务')).toHaveCount(0)
  expect(focusedRequests.length).toBeGreaterThan(0)
  expect(focusedRequests.every(url => !url.searchParams.has('include_system'))).toBe(true)
  expect(api.requests).not.toContain('/admin/jobs/metrics')
  expect(api.mutations).toEqual([])
  expect([...new Set(api.unknown)]).toEqual([])
})

test('个人作业深链仍可定位且不会混入同号旧日志', async ({ page }, info) => {
  const api = await mockApi(page, {
    role: 'user',
    theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue',
    responses: {
      '/tasks': (url: URL) => url.searchParams.has('job_id') ? [
        { ...maintenanceTask, id: 'job:720', source: 'job', source_id: 720, kind: 'qa', owner: 'user', owner_user_id: 9001, triggered_by: undefined, title: '个人问答测试任务', can_retry: true },
        { ...maintenanceTask, id: 'llm:720', source: 'llm', source_id: 720, title: '同号旧问答不应混入' },
      ] : [],
    },
  })
  await page.goto('/tasks?job_id=720', { waitUntil: 'networkidle' })
  await expect(page.locator('.task-title:visible')).toHaveText(['个人问答测试任务'])
  await expect(page.getByText('同号旧问答不应混入')).toHaveCount(0)
  await expect(page.getByRole('button', { name: '重跑', exact: true })).toBeVisible()
  expect(api.mutations).toEqual([])
  expect([...new Set(api.unknown)]).toEqual([])
})

test('涨跌家数快照明确不补采涨停池历史', async ({ page }, info) => {
  const api = await mockApi(page, { theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue' })
  await page.goto('/admin', { waitUntil: 'networkidle' })
  await expect(page.getByRole('button', { name: '涨跌家数快照', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '情绪快照', exact: true })).toHaveCount(0)
  await expect(page.getByText(/不是涨停池历史补采/)).toBeVisible()
  expect(api.mutations).toEqual([])
  expect([...new Set(api.unknown)]).toEqual([])
})
