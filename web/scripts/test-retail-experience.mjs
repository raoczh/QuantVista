import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const read = (relativePath) => fs.readFileSync(path.join(root, relativePath), 'utf8')

const identity = read('src/components/StockIdentity.vue')
assert.match(identity, /名称待补全/, '股票名称缺失时必须显示统一占位，不能重复股票代码')
assert.match(identity, /'normal' \| 'compact' \| 'table'/, '股票身份组件必须提供三种密度')
assert.match(identity, /useStockActions/, '股票身份跳转必须复用统一股票操作')

const stockActions = read('src/composables/useStockActions.ts')
assert.doesNotMatch(stockActions, /name:\s*s\.name\s*\|\|\s*s\.symbol/, '股票跳转不得用代码冒充名称')

const picker = read('src/components/StockPicker.vue')
assert.match(picker, /searchStocks/, '共享选股器必须复用现有股票搜索接口')

for (const page of ['Home', 'Today', 'Watchlist', 'StockDetail', 'Recommendations', 'Analysis', 'Qa', 'Compare', 'DailyReport', 'Positions', 'PortfolioRisk', 'Alerts', 'Screener', 'Backtest', 'Mood', 'BoardDetail', 'Etf', 'Paper', 'ThesisCards', 'Notes', 'Settings']) {
  assert.match(read(`src/pages/${page}.vue`), /StockIdentity/, `${page} 必须接入统一股票身份组件`)
}
for (const page of ['Analysis', 'Compare', 'ThesisCards', 'Notes', 'Settings']) {
  assert.match(read(`src/pages/${page}.vue`), /StockPicker/, `${page} 的常用选股入口必须使用共享选股器`)
}

const settings = read('src/pages/Settings.vue')
const notifications = read('src/components/settings/NotificationSettings.vue')
const alerts = read('src/pages/Alerts.vue')
assert.match(settings, /name="notifications"/, '设置页必须包含通知设置页签')
assert.match(settings, /route\.query\.tab/, '设置页必须支持通知设置深链')
for (const operation of ['createChannel', 'updateChannel', 'deleteChannel', 'testChannel']) {
  assert.match(notifications, new RegExp(operation), `通知设置必须支持 ${operation}`)
  assert.doesNotMatch(alerts, new RegExp(operation), `提醒页不得保留 ${operation} 编辑入口`)
}
assert.doesNotMatch(alerts, /notify-channels|SendKey|Webhook 地址|ntfy 服务地址/, '提醒页不得保留第二套通道表单')

const report = read('src/pages/DailyReport.vue')
assert.doesNotMatch(report, /已按止盈\/止损价自动创建到价卖点提醒/, '日报不得声称会为未持有推荐自动创建卖点提醒')
assert.match(report, /未持有推荐只做研究追踪/, '日报必须解释未持有推荐的真实处理方式')

const quickActions = read('src/components/AIQuickActions.vue')
assert.match(quickActions, /StockPicker/, '股票相关 AI 快捷操作必须先使用股票搜索')
assert.match(quickActions, /position-advice-panel/, '持仓检查必须进入现有持仓建议入口')
assert.doesNotMatch(quickActions, /request\(|axios|generate|askQa|analyze|compareStocks/, 'AI 快捷入口不得在导航时发起 AI 调用')

const displayMode = read('src/composables/useDisplayMode.ts')
assert.match(displayMode, /qv-display-mode:\$\{userID > 0 \? userID : 'guest'\}/, '显示偏好必须按账号隔离')

const terms = read('src/components/termDictionary.ts')
for (const term of ['alpha', 'beta', 'atr', 'rps', 'mfe', 'mae', 'twr', 'sharpe', 'sortino', 'ic', 'icir', 'rankic', 'regime', 'quant_score', 'confidence', 'ai_confidence', 'as_of', 'partial', 'unknown', 'llm', 'token']) {
  assert.match(terms, new RegExp(`\\b${term}:`), `术语字典缺少 ${term}`)
}
assert.match(terms, /回答/, '术语解释必须说明它回答什么问题')

console.log('散户体验契约测试通过')
