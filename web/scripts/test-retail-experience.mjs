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

for (const page of ['Home', 'Today', 'Watchlist', 'StockDetail', 'Qa', 'Compare', 'DailyReport', 'Positions', 'PortfolioRisk', 'Alerts', 'Screener', 'Backtest', 'Mood', 'BoardDetail', 'Etf', 'Paper', 'ThesisCards', 'Notes', 'Settings']) {
  assert.match(read(`src/pages/${page}.vue`), /StockIdentity/, `${page} 必须接入统一股票身份组件`)
}
for (const page of ['Compare', 'ThesisCards', 'Notes', 'Settings']) {
  assert.match(read(`src/pages/${page}.vue`), /StockPicker/, `${page} 的常用选股入口必须使用共享选股器`)
}
const recommendationCard = read('src/components/recommendations/RecommendationCard.vue')
const recommendationAudit = read('src/components/recommendations/RecommendationCandidateAudit.vue')
const analysisLauncher = read('src/components/analysis/AnalysisLauncher.vue')
const analysisResult = read('src/components/analysis/AnalysisResultWorkspace.vue')
for (const component of [recommendationCard, recommendationAudit, analysisResult]) {
  assert.match(component, /StockIdentity/, '推荐/分析工作区必须通过职责组件复用统一股票身份')
}
assert.match(analysisLauncher, /StockPicker/, '分析发起区必须复用共享选股器')

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
assert.match(alerts, /name="rules" tab="提醒规则"/, '提醒页必须用清晰页签分隔规则')
assert.match(alerts, /name="events" tab="命中记录"/, '提醒页必须用清晰页签分隔命中记录')
assert.match(alerts, /v-model:show="wizardOpen"/, '新建和编辑提醒必须进入按需打开的对话框')
assert.match(alerts, /通知接收设置/, '提醒页必须保留唯一通知管理入口')

for (const operation of ['editChannel', 'testSavedChannel', 'toggleChannel', 'removeChannel']) {
  assert.match(notifications, new RegExp(operation), `设置页通道必须保留 ${operation}`)
}
assert.match(notifications, /全部留空会保留原配置/, 'ntfy 编辑必须明确全部敏感字段留空时保留原配置')
assert.equal((notifications.match(/Notification\.requestPermission/g) || []).length, 1, '浏览器权限只能在一个用户动作入口申请')
assert.match(notifications, /async function enableBrowserNotification[\s\S]*Notification\.requestPermission/, '权限申请必须位于开启按钮处理函数')
for (const operation of ['enableBrowserNotification', 'removeDevice', 'sendBrowserTest', 'saveBrowserSettings']) {
  assert.match(notifications, new RegExp(operation), `浏览器通知设置必须支持 ${operation}`)
}
assert.match(notifications, /HTTPS 或 localhost/, '浏览器通知必须说明安全上下文限制')
assert.match(notifications, /添加到主屏幕/, '浏览器通知必须说明 iOS 主屏幕限制')

const browserRuntime = read('src/composables/useBrowserNotifications.ts')
assert.match(browserRuntime, /serviceWorker\.register\('\/sw\.js'/, '必须注册浏览器通知 Service Worker')
assert.match(browserRuntime, /new Notification\(/, '网站打开期间必须调用 Notification API')
assert.match(browserRuntime, /setInterval\(poll/, '前台通知必须轮询服务端事件')
assert.doesNotMatch(browserRuntime, /visibilityState === 'hidden'/, '后台标签页不得停止前台通知轮询降级')

const serviceWorker = read('public/sw.js')
assert.match(serviceWorker, /addEventListener\('push'/, 'Service Worker 必须处理 push')
assert.match(serviceWorker, /addEventListener\('notificationclick'/, 'Service Worker 必须处理通知点击')
assert.match(serviceWorker, /openWindow\(route\)/, '网站关闭后点击通知必须恢复精确路由')
assert.match(serviceWorker, /raw\.startsWith\('\/\/'\)/, 'Service Worker 深链必须阻止开放重定向')

const notifyApi = read('src/api/notify.ts')
for (const endpoint of ['/browser-notifications/config', '/browser-notifications/subscriptions', '/browser-notifications/events', '/browser-notifications/test']) {
  assert.match(notifyApi, new RegExp(endpoint.replaceAll('/', '\\/')), `浏览器通知 API 缺少 ${endpoint}`)
}
const login = read('src/pages/Login.vue')
const oauthCallback = read('src/pages/OAuthCallback.vue')
assert.match(login, /safeInternalRoute/, '登录恢复路由必须经过站内路径校验')
assert.match(oauthCallback, /qv_login_redirect/, 'OAuth 登录后必须恢复通知目标路由')

const positions = read('src/pages/Positions.vue')
const decisionCenter = read('src/components/positions/PositionDecisionCenter.vue')
assert.match(positions, /name="needs_action" tab="需要处理"/, '持仓页必须默认提供需要处理任务')
assert.match(positions, /name="all" tab="全部持仓"/, '持仓页必须保留全部持仓任务')
assert.match(positions, /name="review" tab="交易与复盘"/, '持仓页必须保留交易复盘任务')
assert.match(positions, /name="risk" tab="组合风险"/, '持仓页必须保留组合风险任务')
assert.match(positions, /getPositionExitAssessment/, '持仓通知深链必须读取具体 assessment_id')
assert.match(positions, /position:\$\{positionID\}:assessment:\$\{assessmentID\}/, '同一持仓切换评估时去重键必须包含 assessment_id')
assert.match(decisionCenter, /紧急处理[\s\S]*需要复核/, '决策中心必须显示 urgent 和 review 汇总')
assert.match(decisionCenter, /AI 复核不会自动卖出，也不会修改程序风险等级/, 'AI 复核必须声明不会改写程序事实或自动交易')
assert.match(decisionCenter, /<details class="evidence-details">/, '专业指标和原始证据必须折叠展示')
assert.match(decisionCenter, /StockIdentity/, '风险卡必须复用统一股票身份')

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
for (const term of ['alpha', 'beta', 'atr', 'ma', 'rps', 'mfe', 'mae', 'twr', 'sharpe', 'sortino', 'ic', 'icir', 'rankic', 'regime', 'quant_score', 'confidence', 'ai_confidence', 'as_of', 'partial', 'unknown', 'llm', 'token']) {
  assert.match(terms, new RegExp(`\\b${term}:`), `术语字典缺少 ${term}`)
}
assert.match(terms, /回答/, '术语解释必须说明它回答什么问题')

console.log('散户体验契约测试通过')
