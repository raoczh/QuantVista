import { expect, test, type Page, type TestInfo } from '@playwright/test'
import { mockApi } from './mock-api.mjs'
import { analysis, richResponses, todoResult } from './rich-responses.mjs'

async function start(page: Page, info: TestInfo, options: Record<string, unknown> = {}) {
  const runtimeErrors: string[] = []
  page.on('pageerror', error => runtimeErrors.push(error.message))
  const api = await mockApi(page, { theme: info.project.name.endsWith('dark') ? 'dark-blue' : 'light-blue', responses: richResponses, ...options })
  return {
    api,
    async check() {
      await expect(page.locator('main')).toBeVisible()
      await expect(page.locator('main').getByRole('heading', { level: 1 })).toHaveCount(1)
      expect(runtimeErrors, '页面运行错误').toEqual([])
      expect([...new Set(api.unknown)], '页面使用的接口必须明确模拟').toEqual([])
      expect(api.mutations, '浏览、切换视图和展开详情不能隐式写入').toEqual([])
      expect(await page.locator('main').innerText()).not.toMatch(/\bNaN\b|undefined%/)
      expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth), '宽表应在自己的容器内滚动').toBeLessThanOrEqual(1)
    },
  }
}

const populatedPages = [
  '/', '/today', '/watchlist', '/recommendations?batch_id=1', '/analysis?record_id=1',
  '/positions', '/positions?tab=all', '/positions?tab=risk', '/portfolio-risk', '/portfolio-risk?tab=risk', '/stocks/cn/600100',
  '/news', '/tasks', '/settings', '/settings?tab=notifications', '/admin/llm-calls',
]
for (const path of populatedPages) {
  test(`有数据与部分数据布局 ${path}`, async ({ page }, info) => {
    const session = await start(page, info)
    await page.goto(path, { waitUntil: 'networkidle' })
    if (path === '/stocks/cn/600100') {
      const summary = page.locator('.decision-grid')
      await expect(summary).toContainText('优先处理 · 数据完整')
      await expect(summary).toContainText('部分可用')
      await expect(summary).not.toContainText(/\b(unknown|partial|ready|PositionExitAssessment)\b/)
      const widths = await page.locator('.position-facts > div').evaluateAll(items => items.map(item => item.getBoundingClientRect().width))
      expect(widths.length).toBe(4)
      expect(Math.min(...widths), '持仓指标与完整时间不能挤在过窄的列中').toBeGreaterThanOrEqual(100)
    }
    await session.check()
  })
}

test('推荐等待条件不能呈现为已满足入场', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/recommendations?batch_id=1', { waitUntil: 'networkidle' })
  const first = page.locator('.recommendation-card').first()
  await expect(first).toContainText('等待')
  await expect(first).toContainText('价格高于观察区间')
  await expect(first.getByText('生成时满足价格条件', { exact: true })).toHaveCount(0)
  await expect(first.getByRole('button', { name: '买入', exact: true })).toHaveCount(0)
  await page.getByRole('tab', { name: '历史与复盘', exact: true }).click()
  await expect(page).toHaveURL(/workspace=history/)
  await page.reload({ waitUntil: 'networkidle' })
  await expect(page.getByRole('tab', { name: '历史与复盘', exact: true })).toHaveAttribute('aria-selected', 'true')
  await session.check()
})

test('独立复核展示负利润、来源与报告期', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/analysis?record_id=1', { waitUntil: 'networkidle' })
  await page.getByText('多空辩论（并列观点，不改写主结论）', { exact: true }).click()
  await page.getByText('核对复核使用的证据（2 项）', { exact: true }).click()
  const evidence = page.locator('.debate-evidence')
  await expect(evidence).toContainText('finance.latest.net_profit')
  await expect(evidence).toContainText('-5000000 元')
  await expect(evidence).toContainText('2026-06-30 · 合成财报')
  await page.getByRole('tab', { name: '历史与复盘', exact: true }).click()
  await page.reload({ waitUntil: 'networkidle' })
  const history = page.getByRole('tab', { name: '历史与复盘', exact: true })
  await expect(history).toHaveAttribute('aria-selected', 'true')
  await history.focus()
  await page.keyboard.press('Home')
  await expect(page.getByRole('tab', { name: '本次结果', exact: true })).toBeFocused()
  await expect(page.getByRole('tab', { name: '本次结果', exact: true })).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('ArrowRight')
  await expect(history).toBeFocused()
  await expect(history).toHaveAttribute('aria-selected', 'true')
  await session.check()
})

test('没有独立证据的复核明确显示无法完成', async ({ page }, info) => {
  const record = structuredClone(analysis)
  record.result.debate = { triggered: true, trigger_reasons: ['low_confidence'], rounds: 0, version: 'db3', degraded_reason: 'evidence_unavailable' }
  const session = await start(page, info, { responses: { ...richResponses, '/analysis/1': record } })
  await page.goto('/analysis?record_id=1', { waitUntil: 'networkidle' })
  await page.getByText('多空辩论（并列观点，不改写主结论）', { exact: true }).click()
  await expect(page.getByText('缺少可引用的快照证据，本次未发起多空复核。', { exact: true })).toBeVisible()
  await expect(page.locator('.debate-evidence')).toHaveCount(0)
  await session.check()
})

test('普通用户导航、抽屉关闭与当前页定位', async ({ page }, info) => {
  const session = await start(page, info, { role: 'user' })
  await page.goto('/analysis', { waitUntil: 'networkidle' })
  const drawer = (page.viewportSize()?.width || 1440) <= 1150
  if (drawer) await page.getByRole('button', { name: '打开导航菜单' }).click()
  const nav = page.locator('nav.workspace-nav:visible')
  await expect(nav.getByRole('button', { name: '管理与评估' })).toHaveCount(0)
  await expect(nav.getByRole('link', { name: 'AI 分析', exact: true })).toHaveAttribute('aria-current', 'page')
  await nav.getByRole('button', { name: '市场动态', exact: true }).click()
  await nav.getByRole('link', { name: '市场快讯', exact: true }).click()
  await expect(page).toHaveURL(/\/news$/)
  if (drawer) {
    await expect(page.getByRole('button', { name: '打开导航菜单' })).toHaveAttribute('aria-expanded', 'false')
    await page.getByRole('button', { name: '打开导航菜单' }).click()
  }
  await expect(page.locator('nav.workspace-nav:visible').getByRole('link', { name: '市场快讯', exact: true })).toHaveAttribute('aria-current', 'page')
  if (drawer) await page.keyboard.press('Escape')
  await session.check()
})

test('待办部分失败保留风险与不完整提示', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/today', { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toContainText('保护价已触发')
  await expect(page.locator('main')).toContainText('公司行动来源暂不可用')
  await expect(page.locator('main')).toContainText(/不完整|部分/)
  await session.check()
})

test('有效定价盈亏与未知持仓明确区分', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/', { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toContainText('已定价持仓盈亏')
  await expect(page.locator('main')).toContainText('盈亏仅包含有效定价持仓，未包含行情缺口')
  await page.goto('/positions', { waitUntil: 'networkidle' })
  const urgent = page.locator('#position-item-1')
  await expect(urgent).toHaveClass(/is-urgent/)
  await expect(urgent).toContainText('当前价格已跌破 25.00 元保护线')
  await expect(urgent).toContainText('不等待 AI 重新分析')
  await expect(page.locator('main')).toContainText('1 笔持仓尚无完整风险评估')
  await page.getByText('全部持仓', { exact: true }).click()
  const unknown = page.locator('#position-ledger-item-3')
  await expect(unknown).toContainText(/时效|未知|不可/)
  await expect(unknown).not.toContainText('0.00%')
  await session.check()
})

test('成功任务默认收起，结果优先展示且运行记录可展开', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/analysis?record_id=1', { waitUntil: 'networkidle' })
  const details = page.locator('.research-task-details')
  await expect(details).toBeVisible()
  await expect(details).not.toHaveAttribute('open')
  await expect(page.getByRole('heading', { name: '任务状态', exact: true })).not.toBeVisible()
  await details.locator('summary').click()
  await expect(page.getByRole('heading', { name: '任务状态', exact: true })).toBeVisible()
  await session.check()
})

test('搜索结果可用键盘打开，关闭后焦点返回入口', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/analysis', { waitUntil: 'networkidle' })
  const trigger = page.locator('.app-header').getByRole('button', { name: '搜股票', exact: true })
  await trigger.click()
  await page.getByRole('combobox', { name: '搜索股票', exact: true }).fill('600100')
  await expect(page.getByRole('option').first()).toContainText('示例科技')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/stocks\/cn\/600100/)
  await expect(page.getByRole('dialog')).not.toBeVisible()
  await trigger.click()
  await page.keyboard.press('Escape')
  await expect(trigger).toBeFocused()
  await session.check()
})

test('真实与模拟组合的现金流操作保持分离', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/portfolio-risk?account_id=2&tab=cash', { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toContainText('模拟账户沿用内置现金账本')
  await expect(page.locator('main').getByRole('button', { name: '新增现金流', exact: true })).toHaveCount(0)
  await page.goto('/portfolio-risk?account_id=1&tab=cash', { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toContainText('真实账户')
  await expect(page.locator('main').getByRole('button', { name: '新增现金流', exact: true })).toBeVisible()
  await session.check()
})

test('通知通道分别呈现成败，浏览设置不发送测试通知', async ({ page }, info) => {
  const session = await start(page, info)
  await page.goto('/settings?tab=notifications', { waitUntil: 'networkidle' })
  await expect(page.locator('main')).toContainText('行情提醒')
  await expect(page.locator('main')).toContainText('备用通知')
  const healthy = page.locator('.channel-row').filter({ hasText: '行情提醒' })
  const failed = page.locator('.channel-row').filter({ hasText: '备用通知' })
  await expect(healthy).toContainText('最近发送')
  await expect(healthy).not.toContainText('上次推送失败')
  await expect(failed).toContainText('最近尝试')
  await expect(failed).toContainText('上次推送失败')
  await session.check()
})

for (const path of ['/watchlist', '/news', '/settings', '/admin/llm-calls']) {
  test(`数据读取失败可见 ${path}`, async ({ page }, info) => {
    const session = await start(page, info, { errors: true })
    await page.goto(path, { waitUntil: 'networkidle' })
    await expect(page.locator('main')).toContainText(/失败|不可用|重试/)
    await session.check()
  })
}

test('加载中的待办不会显示为零风险', async ({ page }, info) => {
  let release: (() => void) | undefined
  const pending = new Promise<void>(resolve => { release = resolve })
  const session = await start(page, info, { responses: { ...richResponses, '/todos': async () => { await pending; return todoResult } } })
  try {
    await page.goto('/today', { waitUntil: 'domcontentloaded' })
    await expect(page.locator('main').getByRole('heading', { level: 1 })).toBeVisible()
    await expect(page.locator('main').getByText('暂无待办', { exact: true })).toHaveCount(0)
  } finally {
    release?.()
  }
  await expect(page.locator('main')).toContainText('保护价已触发')
  await session.check()
})

for (const theme of ['light-blue', 'dark-blue', 'dark-emerald', 'light-violet', 'dark-amber', 'light-rose']) {
  test(`主题令牌与研究内容 ${theme}`, async ({ page }, info) => {
    const session = await start(page, info, { theme })
    await page.goto('/recommendations?batch_id=1', { waitUntil: 'networkidle' })
    expect(await page.evaluate(() => document.documentElement.style.colorScheme)).toBe(theme.startsWith('dark') ? 'dark' : 'light')
    expect(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--qv-primary').trim())).not.toBe('')
    await session.check()
  })
}
