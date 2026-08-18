import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import ts from 'typescript'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const repo = path.resolve(root, '..')
const read = (relativePath) => fs.readFileSync(path.join(root, relativePath), 'utf8')
const readRepo = (relativePath) => fs.readFileSync(path.join(repo, relativePath), 'utf8')
function loadTypeScript(relativePath) {
  const source = read(relativePath)
  const output = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
  }).outputText
  const module = { exports: {} }
  new Function('module', 'exports', output)(module, module.exports)
  return module.exports
}

const recPage = read('src/pages/Recommendations.vue')
const analysisPage = read('src/pages/Analysis.vue')
const generator = read('src/components/recommendations/RecommendationGenerator.vue')
const recResult = read('src/components/recommendations/RecommendationResultsWorkspace.vue')
const recCard = read('src/components/recommendations/RecommendationCard.vue')
const recHistory = read('src/components/recommendations/RecommendationHistoryTracking.vue')
const candidateAudit = read('src/components/recommendations/RecommendationCandidateAudit.vue')
const launcher = read('src/components/analysis/AnalysisLauncher.vue')
const analysisResult = read('src/components/analysis/AnalysisResultWorkspace.vue')
const taskPanel = read('src/components/ai/AiTaskStatusPanel.vue')
const taskState = read('src/composables/useBusinessTask.ts')
const polling = read('src/composables/useResultPolling.ts')
const stockActions = read('src/composables/useStockActions.ts')

// 进入页面、切标签、打开历史只允许 GET 与恢复轮询，不能调用生成入口。
const recMounted = recPage.slice(recPage.lastIndexOf('onMounted('))
const analysisMounted = analysisPage.slice(analysisPage.lastIndexOf('onMounted('))
assert.doesNotMatch(recMounted, /generateRecommendations\(|\bgenerate\(\)/, '推荐页 mounted 不得自动生成')
assert.doesNotMatch(analysisMounted, /createAnalysis\(|submitAnalysis\(/, '分析页 mounted 不得自动分析')
assert.match(generator, /明确生成推荐/, '推荐生成必须是明确按钮')
assert.match(launcher, /明确开始分析/, '分析必须是明确按钮')

// 同步锁在任何 await 前生效，按钮 loading 之外仍能挡住程序化重复调用。
for (const [source, label] of [[recPage, '推荐'], [analysisPage, '分析']]) {
  assert.match(source, /if \(submitLocked \|\| running\.value\) return/, `${label}重复点击必须被同步锁拦截`)
  assert.match(source, /submitLocked = true[\s\S]{0,100}submitting\.value = true/, `${label}提交锁必须先于请求设置`)
}

// processing 历史和 ID 深链在刷新后恢复，只轮询既有结果。
assert.match(recPage, /route\.query\.batch_id/, '推荐必须保留 batch_id 深链')
assert.match(recPage, /history\.value\.find\(\(item\) => item\.status === 'processing'\)/, '推荐刷新后必须恢复运行任务')
assert.match(analysisPage, /route\.query\.record_id/, '分析必须保留 record_id 深链')
assert.match(analysisPage, /history\.value\.find\(\(item\) => item\.status === 'processing'\)/, '分析刷新后必须恢复运行任务')
assert.match(polling, /只读取既有结果，不创建新任务/, '共享轮询必须声明只读边界')

// 状态、失败原因与下一步。
for (const label of ['未开始', '排队中', '运行中', '成功', '部分成功', '失败', '已取消']) {
  assert.match(taskPanel, new RegExp(label), `任务状态缺少“${label}”`)
}
assert.match(taskPanel, /task\.error/, '失败和部分成功必须展示服务端真实原因')
assert.match(taskPanel, /taskRecoveryAdvice/, '失败必须展示可执行的下一步')
assert.match(taskState, /cancelJob/, '必须保留显式取消入口')
assert.match(taskState, /retryJob/, '必须保留显式重试入口')

// 推荐事实、身份和候选边界。
assert.match(recCard, /:symbol="item\.symbol"[\s\S]*:market="item\.market"[\s\S]*:name="item\.name"/, '推荐卡名称、代码和市场必须来自同一条记录')
assert.match(candidateAudit, /selectedSymbols/, '候选池与最终结果必须分开核对')
assert.match(candidateAudit, /最终推荐只能来自本批候选池/, '候选池外标的不得混入结果')
assert.match(recHistory, /历史推荐事实只读，追踪只追加状态和解释/, '追踪不得改写历史推荐事实')
assert.match(recCard, /尚未到结算时间，不评价为准确或失败/, '未成熟结果不得展示为准确或失败')
assert.match(recResult, /部分数据或 AI 结果不可用/, '部分成功必须有缺口提示')

// 选中股票是分析 payload 的唯一 symbol 来源；逐笔持仓动作强制 position_id。
assert.match(analysisPage, /payload\.symbol = selectedStock\.value!\.symbol/, '分析只能提交用户选中的股票')
assert.doesNotMatch(analysisPage, /payload\.symbol = form\.value\.symbol/, '分析不得从陈旧表单字段提交股票')
assert.match(stockActions, /position_id: String\(positionID\)/, '卖出决策必须提交准确 position_id')
assert.match(stockActions, /缺少准确的持仓 ID/, '缺 position_id 必须 fail-closed')
assert.match(stockActions, /goPositionFromRecommendation/, '推荐建仓入口必须保留推荐血缘深链')
assert.match(stockActions, /rec_id: String\(recommendationID\)/, '推荐建仓深链必须携带 rec_id')
assert.match(recCard, /item\.position!\.position_id/, '推荐持仓入口必须使用关联持仓 ID')
assert.match(recCard, /按推荐记录建仓/, '推荐卡必须保留显式建仓入口')
assert.match(candidateAudit, /technicalDiagnostics/, '推荐运行诊断必须保留为只读折叠字段')

// AI 复核与程序事实分层，失败不清空已有结果。
assert.match(analysisResult, /AI 观点不会覆盖程序风险等级、持仓卖出等级或任务状态/, 'AI 不得改写系统风险和状态')
assert.doesNotMatch(analysisPage, /catch[\s\S]{0,120}current\.value = null/, 'AI 请求失败不得清空已有结果')
assert.match(recCard, /仅供研究追踪，不代表持仓建议/, '未持有推荐必须明确研究边界')
assert.match(recCard, /普通推荐结论不能当作卖出结论/, '本人持仓不得把推荐当卖出结论')

// 深链和导航只预填上下文，不调用 AI。
assert.match(stockActions, /review_context: 'recommendation'/, '推荐复核深链必须携带上下文类型')
assert.match(stockActions, /recommendation_id: String\(recommendationID\)/, '推荐复核深链必须携带推荐 ID')
assert.doesNotMatch(stockActions, /createAnalysis|generateRecommendations|request\(|axios/, '股票导航不得隐式调用 AI')

// 白话状态映射为纯函数，锁住 unknown/partial/未成熟语义。
const recPresentation = loadTypeScript('src/components/recommendations/recommendationPresentation.ts')
assert.equal(recPresentation.confidenceExplanation(20, 'low'), '把握较低，需要补数据或等待更多信号')
assert.equal(recPresentation.trackingState({ outcome: 'active' }), 'immature')
assert.equal(recPresentation.trackingState({ outcome: 'no_data' }), 'insufficient')
assert.equal(recPresentation.trackingState({ outcome: 'take_profit' }), 'settled')
assert.equal(recPresentation.exclusionReason('股票停牌'), '停牌：股票停牌')

const analysisPresentation = loadTypeScript('src/components/analysis/analysisPresentation.ts')
assert.equal(analysisPresentation.recordStockName({ symbol: '600000', target: '600000', title: '浦发银行' }), '浦发银行')
assert.equal(analysisPresentation.parseSnapshotFreshness('{"freshness_status":"partial"}').freshness, 'partial')
assert.equal(analysisPresentation.parseSnapshotFreshness('{}').freshness, 'unknown')

// 术语帮助与简明/专业模式。
const modeSwitch = read('src/components/DisplayModeSwitch.vue')
assert.match(modeSwitch, /useDisplayMode/, '工作台显示模式必须复用账号隔离的统一状态')
assert.match(modeSwitch, /简明[\s\S]*专业/, '必须提供简明/专业切换')
for (const term of ['alpha', 'beta', 'mfe', 'mae', 'twr', 'sharpe', 'sortino', 'atr', 'rps', 'ic', 'icir', 'rankic', 'regime']) {
  assert.match(analysisResult, new RegExp(`term="${term}"`), `分析证据区缺少 ${term} 术语帮助`)
}
assert.match(analysisResult, /证据与专业指标/, '专业指标必须放在后续折叠区')

// 服务端既有隔离回归测试必须继续存在：推荐、分析、候选审计和任务均按 user_id 收口。
const recServiceTest = readRepo('server/service/recommendation_test.go')
const analysisService = readRepo('server/service/analysis.go')
const candidateAuditTest = readRepo('server/service/candidateaudit_test.go')
const taskCenterTest = readRepo('server/service/task_center_test.go')
assert.match(recServiceTest, /跨用户 Get\/Delete 隔离/, '推荐用户隔离测试缺失')
assert.match(analysisService, /Where\("id = \? AND user_id = \?"/, '分析详情必须按本人 user_id 查询')
assert.match(analysisService, /q := common\.DB\.Where\("user_id = \?"/, '分析历史必须按本人 user_id 查询')
assert.match(candidateAuditTest, /TestCandidateAuditUserIsolationAndAdminAggregatePrivacy/, '候选池审计用户隔离测试缺失')
assert.match(taskCenterTest, /TestTaskCenterUserIsolationAdminSystemAndStatusMapping/, '调用审计/任务用户隔离测试缺失')

console.log('AI 推荐与分析工作台契约测试通过')
