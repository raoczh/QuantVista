import assert from 'node:assert/strict'
import fs from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'

const source = fs.readFileSync(new URL('../src/composables/useListPageState.ts', import.meta.url), 'utf8')
const output = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS } }).outputText
const tick = () => new Promise(setImmediate)
const failures = []
const check = (name, actual, expected) => { if (actual !== expected) failures.push({ name, actual, expected }) }
function create(blockedStorage = false) {
  const mounted = [], unmount = [], frames = [], scrolls = [], storage = new Map()
  const auth = { user: { id: 11 } }
  const sessionStorage = {
    getItem: key => { if (blockedStorage) throw new Error('存储已禁用'); return storage.get(key) ?? null },
    setItem: (key, value) => { if (blockedStorage) throw new Error('存储已禁用'); storage.set(key, value) },
    removeItem: key => { if (blockedStorage) throw new Error('存储已禁用'); storage.delete(key) },
  }
  const window = Object.assign(new EventTarget(), {
    history: { state: { position: 0 }, length: 1 }, scrollY: 0,
    setTimeout, clearTimeout,
    requestAnimationFrame: fn => { frames.push(fn); return frames.length },
    scrollTo: options => scrolls.push(options),
  })
  const module = { exports: {} }
  new Function('require', 'module', 'exports', 'window', 'sessionStorage', output)(name => {
    if (name === 'vue') return { ...vue, onMounted: callback => mounted.push(callback), onBeforeUnmount: callback => unmount.push(callback) }
    if (name === 'vue-router') return { onBeforeRouteLeave() {}, onBeforeRouteUpdate() {} }
    if (name === '@/stores/auth') return { useAuthStore: () => auth }
    throw new Error(`未知依赖 ${name}`)
  }, module, module.exports, window, sessionStorage)
  const scope = vue.effectScope()
  const route = vue.reactive({ name: 'today', path: '/today', fullPath: '/today?page=1', query: { page: '1' }, hash: '' })
  const run = fn => scope.run(fn)
  return {
    api: module.exports, run, route, storage, frames, scrolls,
    mount: () => mounted.forEach(callback => callback()),
    close() { unmount.splice(0).forEach(callback => callback()); scope.stop() },
  }
}

const codecs = create()
for (const [name, codec] of [
  ['整数', codecs.api.integerQuery(5, 0, 10)],
  ['数字', codecs.api.numberQuery(50, 0, 100)],
  ['整数枚举', codecs.api.integerEnumQuery(5, [0, 5, 10])],
]) {
  for (const raw of [undefined, null, '', '  ', []]) check(`${name} 缺省值 ${JSON.stringify(raw)}`, codec.parse(raw), name === '数字' ? 50 : 5)
  check(`${name} 保留显式零值`, codec.parse('0'), 0)
}
const priceMax = vue.ref(50)
codecs.run(() => codecs.api.useRouteQueryState(codecs.route, { replace: async () => {} }, [codecs.api.queryRef('price_max', priceMax, codecs.api.numberQuery(50, 0, 1_000_000))]))
check('缺少 URL 参数时保留推荐价格上限默认值', priceMax.value, 50)
codecs.route.name = 'news'
codecs.route.query = { price_max: '77' }
check('离页后不消费其他路由参数', priceMax.value, 50)
codecs.close()

// 路由替换需要异步完成，期间加载的偏好/继续输入的值不能被旧 URL 覆盖。
for (const scenario of ['other_field', 'same_field']) {
  const runtime = create(), navigations = []
  runtime.route.query = {}
  const count = vue.ref(5), strategy = vue.ref(''), price = vue.ref(50)
  const router = { replace: target => new Promise(resolve => navigations.push({ target, resolve })) }
  runtime.run(() => runtime.api.useRouteQueryState(runtime.route, router, [
    runtime.api.queryRef('count', count, runtime.api.integerQuery(5, 3, 5)),
    runtime.api.queryRef('strategy', strategy, runtime.api.stringQuery('')),
    runtime.api.queryRef('price', price, runtime.api.numberQuery(50, 0, 100)),
  ]))
  await tick()
  if (scenario === 'other_field') strategy.value = 'momentum'
  else count.value = 4
  await tick()
  assert.equal(navigations.length, 1, '夹具应截获第一次路由同步')
  count.value = 3
  price.value = 23
  await tick()
  const first = navigations.shift()
  runtime.route.query = first.target.query
  first.resolve()
  await tick()
  check(`${scenario} 旧路由同步不能覆盖新数量`, count.value, 3)
  check(`${scenario} 旧路由同步不能覆盖新价格`, price.value, 23)
  for (let i = 0; i < 6 && navigations.length; i++) {
    const next = navigations.shift()
    runtime.route.query = next.target.query
    next.resolve()
    await tick()
  }
  check(`${scenario} 最终URL保留新数量`, runtime.route.query.count, '3')
  check(`${scenario} 最终URL保留新价格`, runtime.route.query.price, '23')
  runtime.route.query = { ...runtime.route.query, count: '4' }
  check(`${scenario} 同页显式导航仍回填参数`, count.value, 4)
  runtime.close()
  navigations.forEach(item => item.resolve())
}

// 同一页面中的父页与嵌入页拥有不同参数，慢导航不能相互抹掉已提交参数。
for (const action of ['merge', 'leave']) {
  const runtime = create(), navigations = []
  runtime.route.query = {}
  const parent = vue.ref('all'), child = vue.ref(252)
  const router = { replace: target => new Promise(resolve => navigations.push({ target, resolve })) }
  runtime.run(() => {
    runtime.api.useRouteQueryState(runtime.route, router, [runtime.api.queryRef('tab', parent, runtime.api.stringQuery('all'))])
    runtime.api.useRouteQueryState(runtime.route, router, [runtime.api.queryRef('window', child, runtime.api.integerQuery(252, 30, 730))])
  })
  await tick()
  parent.value = 'risk'
  child.value = 120
  await tick()
  check(`${action} 多个参数拥有者只发起一个在途导航`, navigations.length, 1)
  let committed = 0
  while (navigations.length && committed < 10) {
    const next = navigations.shift()
    if (action === 'leave') {
      runtime.route.name = 'news'
      runtime.route.path = '/news'
      runtime.route.query = {}
    } else runtime.route.query = next.target.query
    next.resolve()
    committed++
    await tick()
  }
  if (action === 'merge') {
    check('合并同步保留父页页签', runtime.route.query.tab, 'risk')
    check('合并同步保留子页窗口', runtime.route.query.window, '120')
  } else check('离页后排队的参数同步不再导航', committed, 1)
  runtime.close()
  navigations.forEach(item => item.resolve())
}

for (const action of ['route', 'unmount']) {
  const runtime = create()
  runtime.storage.set('qv:list-scroll:v1:11:today', JSON.stringify({ version: 1, entries: [{ entry: '0', path: '/today?page=1', top: 600, updatedAt: Date.now() }] }))
  const scroll = runtime.run(() => runtime.api.useListPageScroll(runtime.route, 'today'))
  runtime.mount()
  const pending = scroll.restoreScroll()
  await tick()
  if (action === 'route') runtime.route.fullPath = '/today?page=2'
  else runtime.close()
  await tick()
  while (runtime.frames.length) runtime.frames.shift()()
  await pending
  check(`${action} 后不执行旧页面的滚动恢复`, runtime.scrolls.length, 0)
  runtime.close()
}

const valid = create()
valid.storage.set('qv:list-scroll:v1:11:today', JSON.stringify({ version: 1, entries: [{ entry: '0', path: '/today?page=1', top: 450, updatedAt: Date.now() }] }))
const scroll = valid.run(() => valid.api.useListPageScroll(valid.route, 'today'))
const pending = scroll.restoreScroll()
await tick()
while (valid.frames.length) valid.frames.shift()()
await pending
check('当前页面正常恢复已保存位置', valid.scrolls[0]?.top, 450)
valid.close()

const blocked = create(true)
const protectedScroll = blocked.run(() => blocked.api.useListPageScroll(blocked.route, 'today'))
try { protectedScroll.saveScroll() } catch { failures.push({ name: '存储被禁用时保存滚动状态抛错' }) }
try { blocked.close() } catch { failures.push({ name: '存储被禁用时卸载抛错' }) }

console.log(JSON.stringify({ failures }, null, 2))
assert.deepEqual(failures, [], '列表参数默认值和滚动恢复必须属于当前页面')
console.log('列表 URL 默认值、路由归属和滚动恢复回归通过')
