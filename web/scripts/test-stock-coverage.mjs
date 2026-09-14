import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import ts from 'typescript'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const source = fs.readFileSync(path.join(here, '../src/components/stockCoverage.ts'), 'utf8')
const output = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const module = { exports: {} }
new Function('module', 'exports', output)(module, module.exports)
const { createLoadEpoch, resolveCoverageStatus } = module.exports

const epoch = createLoadEpoch()
const oldStock = epoch.next()
const newStock = epoch.next()
assert.equal(epoch.isCurrent(oldStock), false, '切股后旧标的迟到响应必须失效')
assert.equal(epoch.isCurrent(newStock), true, '新标的响应应保持有效')
epoch.invalidate()
assert.equal(epoch.isCurrent(newStock), false, '卸载/再次切股必须让在途响应失效')

assert.equal(resolveCoverageStatus({ observed: false, available: false }), 'unknown')
assert.equal(resolveCoverageStatus({ observed: true, available: false }), 'missing')
assert.equal(resolveCoverageStatus({ observed: true, available: true, stale: true }), 'stale')
assert.equal(resolveCoverageStatus({ observed: true, available: true, error: 'timeout' }), 'error')
assert.equal(resolveCoverageStatus({ observed: true, available: true }), 'available')

console.log('stock coverage tests passed')

// 财务接口明确返回 null 后，决策摘要必须保留缺口，不能把未知显示成零或无风险。
const decisionSource = fs.readFileSync(path.join(here, '../src/components/stock-detail/decisionSummary.ts'), 'utf8')
const decisionOutput = ts.transpileModule(decisionSource, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const decisionModule = { exports: {} }
new Function('module', 'exports', 'require', decisionOutput)(decisionModule, decisionModule.exports, (name) => {
  assert.equal(name, '@/lib/formatPrice')
  return { formatPrice: (value) => String(value) }
})
const decisionBase = {
  quote: null, position: null, bars: [], valuation: null, score: null, fundflow: null,
  corpEvents: null, announcements: [], news: [], eventPhase: 'ready', eventPartial: false,
  fundamentalPhase: 'ready',
}
const financeRow = { report_name: '2026中报', report_date: '2026-06-30', roe: null, gross_margin: 0, revenue_yoy: null, net_profit_yoy: null, debt_ratio: null }
const summarize = (row) => decisionModule.exports.buildDecisionSummary({ ...decisionBase, finance: { indicators: [row], statements: [] } })
const missingFinance = summarize(financeRow)
const financeChange = missingFinance.changes.find((item) => item.id === 'finance-change')
assert.equal(financeChange.tone, 'unknown')
assert.match(financeChange.value, /营收 缺失.*净利 缺失/)
assert.match(financeChange.evidence, /ROE 缺失.*毛利率 0.00%/)
assert.ok(!missingFinance.risks.some((item) => item.id === 'finance-risk'))
const zeroFinance = summarize({ ...financeRow, revenue_yoy: 0, net_profit_yoy: 0 })
assert.equal(zeroFinance.changes.find((item) => item.id === 'finance-change').tone, 'neutral')
for (const row of [{ ...financeRow, net_profit_yoy: -8 }, { ...financeRow, debt_ratio: 80 }]) {
  const risk = summarize(row).risks.find((item) => item.id === 'finance-risk')
  assert.ok(risk, '已知恶化字段仍须显示，其他字段缺失不能令摘要崩溃')
  assert.match(risk.evidence, /缺失/)
}
console.log('财务缺失与真实零展示测试通过')

for (const row of [
  { ...financeRow, revenue_yoy: 20, net_profit_yoy: 50, net_profit: -1000, deduct_profit: -1200 },
  { ...financeRow, revenue_yoy: 20, net_profit_yoy: 50, net_profit: 1000, deduct_profit: 0 },
]) {
  const summary = summarize(row)
  assert.equal(summary.changes.find((item) => item.id === 'finance-change').tone, 'warning')
  assert.match(summary.risks.find((item) => item.id === 'finance-risk').title, /净利润不为正/)
  assert.match(summary.changes.find((item) => item.id === 'finance-change').detail, /亏损收窄或扭亏/)
}
console.log('亏损收窄与扣非未盈利不能显示成盈利成长')
