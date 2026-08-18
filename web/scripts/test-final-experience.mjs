import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const read = (file) => fs.readFileSync(path.join(root, file), 'utf8')
const page = (name) => read(`src/pages/${name}.vue`)

const router = read('src/router/index.ts')
const routes = [
  '/setup', '/login', '/login/callback', '/', '/mood', '/news', '/stocks/:market/:symbol', '/today', '/daily-report', '/watchlist', '/screener',
  '/heatmap', '/boards/:code', '/backtest', '/positions', '/portfolio-risk', '/analysis', '/tasks', '/qa',
  '/compare', '/paper', '/etf', '/prompt-templates', '/recommendations', '/alerts', '/thesis', '/notes', '/settings',
  '/:pathMatch(.*)*',
]
for (const route of routes) {
  assert.ok(router.includes(`path: '${route}'`) || router.includes(`path: "${route}"`), `路由缺少 ${route}`)
}
for (const name of ['AdminSettings', 'AdminLlmCalls', 'AdminFactorIc', 'AdminWalkForward', 'AdminSelectionEval', 'AdminCalibration', 'AdminLlmRoles', 'AdminLlmExperiments', 'AdminJointEval']) {
  assert.match(router, new RegExp(`pages/${name}\\.vue`), `管理路由缺少 ${name}`)
}

// 三个大页必须由页面编排、职责组件共同组成；页面不能退回压缩单体。
assert.match(page('Screener'), /ScreenerQuickSelect|ScreenerScanResults|ScreenerHistory/)
assert.match(page('StockDetail'), /StockDetailResearchTabs|StockMobileActions/)
assert.match(page('PortfolioRisk'), /PortfolioRiskConclusion|PortfolioProfessionalMetrics/)
for (const file of [
  'src/components/screener/ScreenerQuickSelect.vue',
  'src/components/screener/ScreenerScanResults.vue',
  'src/components/screener/ScreenerHistory.vue',
  'src/components/stock-detail/StockDetailResearchTabs.vue',
  'src/components/portfolio-risk/PortfolioRiskConclusion.vue',
]) assert.ok(fs.existsSync(path.join(root, file)), `缺少职责组件 ${file}`)

// AI 只允许在用户动作中触发；挂载和页签监听只恢复已有任务或读取数据。
for (const name of ['Screener', 'StockDetail', 'PortfolioRisk', 'Backtest', 'Compare', 'Qa', 'Paper', 'Today', 'DailyReport', 'Tasks', 'Settings']) {
  const source = page(name)
  const mounted = source.slice(source.lastIndexOf('onMounted('))
  assert.doesNotMatch(mounted, /generateRecommendations\(|createAnalysis\(|askQa\(|parseScreenerStrategy\(|compareStocks\(.*with_ai/i, `${name} 挂载不得隐式调用 AI`)
}
assert.match(page('Screener'), /@click="runAiParse"/)
assert.match(page('Compare'), /@click="run"/)
assert.match(page('Qa'), /@click="send"/)
assert.match(read('src/components/screener/ScreenerScanResults.vue'), /数据完整度未知|部分数据 · 完整度/)
assert.match(read('src/components/screener/ScreenerScanResults.vue'), /允许停牌或滞后数据参与扫描/)

// 股票身份与数据状态：名称缺失只能显示统一占位，非 fresh 结果不得进入正常比较。
const identity = read('src/components/StockIdentity.vue')
assert.match(identity, /名称待补全/)
assert.match(page('Compare'), /freshness_status !== 'stale'/)
assert.match(page('Compare'), /未参与对比/)
assert.match(read('src/components/stock-detail/decisionSummary.ts'), /PositionExitAssessment/)
assert.match(page('StockDetail'), /exit_assessment/)
assert.doesNotMatch(page('StockDetail'), /below_stop_loss|near_stop_loss/, '个股卖出风险不得绕过统一评估事实')
assert.doesNotMatch(read('src/components/stock-detail/decisionSummary.ts'), /position-stop|belowStopLoss|nearStopLoss/)
for (const name of ['Alerts', 'Etf', 'Paper', 'Recommendations', 'Positions', 'Watchlist', 'Qa']) {
  assert.doesNotMatch(page(name), /name\s*\|\|\s*symbol|name\s*\|\|\s*item\.symbol/, `${name} 不得用代码冒充名称`)
}
assert.doesNotMatch(read('src/composables/useStockActions.ts'), /s\.name\s*\|\|\s*s\.symbol/, '统一股票动作不得用代码冒充名称')
assert.match(page('AdminWalkForward'), /it\.name \|\| '名称待补全'/)

// 模拟账户与真实账本隔离，组合页允许 risk 深链。
assert.match(page('Paper'), /全部属于模拟账户.*真实持仓.*隔离/)
assert.match(page('PortfolioRisk'), /allowedTabs = new Set\(\['overview', 'risk'/)
assert.match(read('src/components/portfolio-risk/PortfolioRiskConclusion.vue'), /不会自动调仓|不会自动交易/)

// 状态语义必须可见，失败后有重试或下一步。
for (const name of ['Heatmap', 'BoardDetail', 'News', 'Etf', 'Paper', 'Backtest', 'PortfolioRisk']) {
  const source = page(name)
  assert.match(source, /未知|暂不可用|读取失败|失败/, `${name} 缺少不可判定状态文案`)
}
assert.match(page('Heatmap'), /@click="load\(\)"/)
assert.match(page('BoardDetail'), /@click="load\(\)"/)
assert.match(read('src/api/taskCenter.ts'), /degraded: '部分成功'/)
assert.match(read('src/lib/authError.ts'), /stack trace|token|secret|password/)
assert.match(page('Login'), /safeInternalRoute/)
assert.match(page('OAuthCallback'), /qv_login_redirect/)

// 设置页的通知通道和浏览器通知能力不得被页面重构删除。
const settings = read('src/components/settings/NotificationSettings.vue')
for (const operation of ['createChannel', 'updateChannel', 'deleteChannel', 'testChannel', 'enableBrowserNotification', 'removeDevice']) {
  assert.match(settings, new RegExp(operation), `通知设置缺少 ${operation}`)
}
assert.match(settings, /全部留空会保留原配置/)
assert.match(settings, /前台 Notification API 不依赖 Service Worker/, '不支持 Service Worker 时仍必须保留前台通知降级')
assert.match(read('src/composables/useBrowserNotifications.ts'), /只有服务端确认了当前设备的投递，才能推进游标/, '前台通知确认失败不得推进游标')

console.log('剩余页面体验收口契约测试通过')
