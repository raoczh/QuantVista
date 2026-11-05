import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import { computed, reactive, ref } from 'vue'

// Exercise the real maintenance flow without a browser server or business API.
const source = fs.readFileSync(new URL('../src/pages/AdminSettings.vue', import.meta.url), 'utf8')
const start = source.indexOf('type MaintenanceKind =')
const end = source.indexOf('\nonMounted(() => {', start)
assert.ok(start > 0 && end > start)
const compiled = ts.transpileModule(source.slice(start, end), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText

function createMaintenanceRuntime() {
  const requests = []
  const refreshes = []
  let jobStatus = 'queued'
  let pendingPreview
  const send = async (kind, payload) => {
    requests.push({ kind, payload })
    if (!payload.dry_run) return { started: true, job_run_id: 71 }
    if (pendingPreview) return pendingPreview
    return {
      dry_run: true,
      plan: {
        market: 'cn', scope: payload.scope, from: '2026-01-05', to: '2026-11-03',
        plan_hash: `${payload.scope ?? kind}-confirmed-hash`, target_count: 800,
        universe_count: 5500, capped: true,
      },
    }
  }
  const dependencies = {
    computed, reactive, ref,
    pageActive: () => true,
    message: { success() {}, warning() {}, error(error) { throw new Error(error) } },
    triggerSyncBars: payload => send('bars', payload),
    triggerWideSync: payload => send('wide', payload),
    triggerBackfillCalendar: payload => send('calendar', payload),
    loadLogs: async () => refreshes.push('logs'),
    loadHealth: async () => refreshes.push('health'),
    loadCapabilities: async () => refreshes.push('capabilities'),
    getJob: async () => ({ id: 71, status: jobStatus }),
    useVisibleTaskPolling: refresh => ({ refreshNow: refresh }),
  }
  const execute = new Function(...Object.keys(dependencies), `${compiled}
    return { dailyBackfillScope, maintenancePayload, previewMaintenance,
      executeMaintenancePlan, maintenancePlan, refreshMaintenanceJob }`)
  return {
    ...execute(...Object.values(dependencies)), requests, refreshes,
    setJobStatus(status) { jobStatus = status },
    deferPreview() { pendingPreview = new Promise(resolve => { this.resolvePreview = resolve }) },
  }
}

const runtime = createMaintenanceRuntime()
assert.equal(runtime.dailyBackfillScope.value, 'marketwide', 'daily repair defaults to the market universe')
await runtime.previewMaintenance('bars')
assert.equal(runtime.requests[0].payload.scope, 'marketwide')
assert.equal(runtime.requests[0].payload.dry_run, true)

// Confirmation must authorize the displayed immutable plan, not a changed form.
runtime.dailyBackfillScope.value = 'tracked'
await runtime.executeMaintenancePlan()
assert.equal(runtime.requests[1].payload.scope, 'marketwide')
assert.equal(runtime.requests[1].payload.plan_hash, 'marketwide-confirmed-hash')
await runtime.previewMaintenance('bars')
assert.equal(runtime.requests[2].payload.scope, 'tracked')
assert.equal(runtime.maintenancePlan.value.scope, 'tracked')
assert.equal(Object.hasOwn(runtime.maintenancePayload('calendar', true), 'scope'), false)
assert.equal(Object.hasOwn(runtime.maintenancePayload('wide', true), 'scope'), false)

runtime.refreshes.length = 0
runtime.setJobStatus('degraded')
await runtime.refreshMaintenanceJob()
assert.deepEqual(runtime.refreshes.sort(), ['capabilities', 'health', 'logs'], 'terminal status refreshes coverage and audit data')
await runtime.refreshMaintenanceJob()
assert.equal(runtime.refreshes.length, 3, 'unchanged terminal state does not cause a refresh loop')

const stalePreview = createMaintenanceRuntime()
stalePreview.deferPreview()
const preview = stalePreview.previewMaintenance('bars')
stalePreview.dailyBackfillScope.value = 'tracked'
stalePreview.resolvePreview({ dry_run: true, plan: { scope: 'marketwide', plan_hash: 'stale' } })
await preview
assert.equal(stalePreview.maintenancePlan.value, null, 'late preview cannot authorize a different selected scope')

console.log('Daily backfill admin flow: scope defaults, confirmation isolation, stale preview and terminal refresh passed')
