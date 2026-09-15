import { expect, test } from '@playwright/test'
import { mockApi } from './mock-api.mjs'

const pages = [
  '/', '/today', '/watchlist', '/screener', '/recommendations', '/analysis', '/qa', '/compare',
  '/mood', '/news', '/heatmap', '/etf', '/stocks/cn/600100', '/boards/BK0477',
  '/positions', '/portfolio-risk', '/alerts', '/daily-report', '/thesis', '/notes', '/paper',
  '/tasks', '/backtest', '/prompt-templates', '/settings', '/admin', '/admin/llm-calls',
  '/admin/factor-ic', '/admin/walk-forward', '/admin/selection-eval', '/admin/calibration',
  '/admin/llm-roles', '/admin/llm-experiments', '/admin/joint-eval', '/page-not-found',
  '/login', '/setup', '/login/callback',
]

for (const path of pages) {
  test(`页面可用与布局 ${path}`, async ({ page }, info) => {
    const runtimeErrors: string[] = []
    page.on('pageerror', error => runtimeErrors.push(error.message))
    const anonymous = ['/login', '/setup', '/login/callback'].includes(path)
    const api = await mockApi(page, { theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue', anonymous, setup: path === '/setup' })
    await page.goto(path, { waitUntil: 'networkidle' })
    await expect(page.locator('main')).toBeVisible()
    await expect(page.locator('main').getByRole('heading').first()).toBeVisible()
    await expect(page.locator('main').getByRole('heading', { level: 1 })).toHaveCount(1)
    expect(runtimeErrors).toEqual([])
    expect(api.mutations, '仅浏览页面不能发起 AI 或写入业务数据').toEqual([])
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth), '页面不得横向溢出，宽表应在自己的容器内滚动').toBeLessThanOrEqual(1)
    expect(await page.locator('main').innerText()).not.toMatch(/\bNaN\b|undefined%/)
    expect([...new Set(api.unknown)], '需要为页面所读取的接口提供明确的测试响应').toEqual([])
  })
}

test('键盘搜索与导航不隐式创建研究任务', async ({ page }, info) => {
  const api = await mockApi(page, { theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue', role: 'user' })
  await page.goto('/analysis', { waitUntil: 'networkidle' })
  await page.keyboard.press('Control+k')
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).not.toBeVisible()
  await page.getByRole('tab', { name: '历史与复盘', exact: true }).click()
  await expect(page).toHaveURL(/workspace=history/)
  await page.reload({ waitUntil: 'networkidle' })
  await expect(page.getByRole('tab', { name: '历史与复盘', exact: true })).toHaveAttribute('aria-selected', 'true')
  await page.getByRole('button', { name: '研究条件', exact: true }).click()
  await expect(page.getByRole('complementary', { name: '研究条件' })).toBeVisible()
  expect(api.mutations).toEqual([])
})
