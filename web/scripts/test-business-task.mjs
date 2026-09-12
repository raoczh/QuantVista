import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'

const source = fs.readFileSync(new URL('../src/composables/useBusinessTask.ts', import.meta.url), 'utf8')
const output = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const tick = () => new Promise(setImmediate)
function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const task = (id) => ({ source: 'job', kind: 'analysis', source_id: id * 100 + 1, result_id: id, can_cancel: true, can_retry: true })
function create(apis) {
  const unmount = []
  const module = { exports: {} }
  new Function('require', 'module', 'exports', output)((name) => {
    if (name === 'vue') return { ...vue, onBeforeUnmount: (callback) => unmount.push(callback) }
    if (name === '@/api/taskCenter') return apis
    if (name === '@/api/client') return { isAbortError: (error) => error.name === 'AbortError' }
    throw new Error(`未知依赖 ${name}`)
  }, module, module.exports)
  const scope = vue.effectScope()
  const resultID = vue.ref(1)
  const state = scope.run(() => module.exports.useBusinessTask('analysis', resultID))
  return { state, resultID, close() { unmount.forEach((callback) => callback()); scope.stop() } }
}

let reads = 0
let actions = 0
const failed = create({
  listTasks: async () => { if (++reads === 1) return [task(1)]; throw new Error('结果 2 读取失败') },
  cancelJob: async () => { actions++ },
  retryJob: async () => { actions++ },
})
await tick()
assert.equal(failed.state.task.value.source_id, 101)
failed.resultID.value = 2
assert.equal(failed.state.task.value, null, '对象变化时同步清除旧任务')
await tick()
assert.equal(failed.state.error.value, '结果 2 读取失败')
await failed.state.cancel()
await failed.state.retry()
assert.equal(actions, 0, '新结果读取失败不能操作旧任务')
failed.resultID.value = null
assert.equal(failed.state.loading.value, false)
assert.equal(failed.state.error.value, '')
failed.close()

const slow = deferred()
reads = 0
const racing = create({ listTasks: async () => ++reads === 1 ? slow.promise : [task(2)] })
racing.resultID.value = 2
await tick()
slow.resolve([task(1)])
await tick()
assert.equal(racing.state.task.value.result_id, 2, '传输层忽略取消时也不能回填迟到结果')
racing.close()

for (const action of ['cancel', 'retry']) {
  const response = deferred()
  const started = []
  reads = 0
  const active = create({
    listTasks: async () => [task(++reads === 1 ? 1 : 2)],
    cancelJob: (id) => { started.push(id); return response.promise },
    retryJob: (id) => { started.push(id); return response.promise },
    getJob: async () => { throw new Error('对象已切换，不应继续读取旧重试结果') },
  })
  await tick()
  const result = active.state[action]()
  active.resultID.value = 2
  await tick()
  response.resolve({ id: 300, result_id: 3 })
  assert.equal(await result, null, '操作中切换对象后，迟到结果不能重新选择旧业务')
  assert.deepEqual(started, [101])
  assert.equal(active.state.task.value.result_id, 2)
  active.close()
}

const late = deferred()
const unmounted = create({ listTasks: () => late.promise })
unmounted.close()
late.reject(new Error('卸载后的迟到故障'))
await tick()
assert.equal(unmounted.state.error.value, '')
assert.equal(unmounted.state.task.value, null)

const compile = (file) => ts.transpileModule(fs.readFileSync(new URL(file, import.meta.url), 'utf8'), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
}).outputText
const pollModule = { exports: {} }
new Function('module', 'exports', compile('../src/lib/poll.ts'))(pollModule, pollModule.exports)
const resultPollingSource = compile('../src/composables/useResultPolling.ts')
function createResultPolling(load) {
  const hooks = [], errors = [], values = []
  let epoch = 1, settled = 0
  const module = { exports: {} }
  new Function('require', 'module', 'exports', resultPollingSource)((name) => {
    if (name === 'vue') return { ...vue, onBeforeUnmount: (callback) => hooks.push(callback) }
    if (name === '@/lib/poll') return pollModule.exports
    if (name === '@/api/token') return { getSessionEpoch: () => epoch }
    throw new Error(`未知轮询依赖 ${name}`)
  }, module, module.exports)
  const state = module.exports.useResultPolling({ load, isDone: () => true,
    onResult: (id, value) => values.push({ id, value }), onError: (error) => errors.push(error.message),
    onSettled: () => { settled++ },
  })
  return { state, errors, values, get settled() { return settled }, nextSession() { epoch++ }, close() { hooks.forEach((callback) => callback()) } }
}

for (const finish of ['stop', 'unmount', 'session']) {
  const request = deferred()
  const active = createResultPolling(() => request.promise)
  const running = active.state.track(1)
  if (finish === 'stop') active.state.stop()
  if (finish === 'unmount') active.close()
  if (finish === 'session') active.nextSession()
  request.reject(new Error('不属于当前页面的迟到失败'))
  await running
  assert.deepEqual(active.errors, [], `${finish} 后旧失败不得触发页面错误回调`)
  assert.equal(active.settled, 0, `${finish} 后不能触发额外历史或任务读取`)
  assert.equal(active.state.polling.value, false)
  active.close()
}

const oldPoll = deferred(), newPoll = deferred()
const switched = createResultPolling((id) => id === 1 ? oldPoll.promise : newPoll.promise)
const firstPoll = switched.state.track(1), secondPoll = switched.state.track(2)
oldPoll.reject(new Error('旧对象错误'))
await firstPoll
assert.deepEqual(switched.errors, [])
assert.equal(switched.state.polling.value, true, '旧对象失败不结束新对象轮询')
newPoll.resolve({ id: 2 })
await secondPoll
assert.deepEqual(switched.values, [{ id: 2, value: { id: 2 } }])
assert.equal(switched.settled, 1)
switched.close()

let postUnmountReads = 0
const stoppedResult = createResultPolling(async () => { postUnmountReads++; return {} })
stoppedResult.close()
await stoppedResult.state.track(1)
assert.equal(postUnmountReads, 0, '卸载后的迟到调用不能重启结果轮询')
console.log('业务结果与任务绑定、迟到响应及操作隔离回归通过')
