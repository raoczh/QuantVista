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
