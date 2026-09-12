import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'

const source = fs.readFileSync(new URL('../src/components/home/homeWorkspace.ts', import.meta.url), 'utf8')
const output = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const module = { exports: {} }
new Function('module', 'exports', output)(module, module.exports)
const { sortPositionRisks } = module.exports

const position = (id, level, dataStatus = 'ready', flags = {}) => ({
  id,
  below_stop_loss: false,
  near_stop_loss: false,
  short_term_review: false,
  analysis_stale: false,
  ...(level ? { exit_assessment: { level, data_status: dataStatus } } : {}),
  ...flags,
})
const positions = [
  position(1, 'normal'),
  position(2, 'normal', 'ready', { analysis_stale: true }),
  position(3, 'watch'),
  position(4, 'review'),
  position(5, 'urgent'),
  position(6, 'normal', 'partial'),
  position(7, 'unknown', 'unknown'),
  position(8),
  position(9, 'normal', 'ready', { below_stop_loss: true }),
]
const original = structuredClone(positions)
assert.deepEqual(
  sortPositionRisks(positions).map(item => item.id),
  [5, 9, 4, 3, 2, 6, 7, 8],
  '紧急与止损风险优先，统一评估和旧标记均保留，数据缺口不能消失',
)
assert.deepEqual(positions, original, '首页排序不得改写持仓事实')
assert.deepEqual(sortPositionRisks([position(1, 'normal')]), [], '完整且正常的评估不应制造需关注事项')
console.log('首页持仓风险汇总回归通过')
