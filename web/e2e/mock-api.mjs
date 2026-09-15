// 浏览器回归专用的合成数据；所有 /api 请求均在浏览器内截获，不连接真实服务。
import { readFileSync } from 'node:fs'
export const emptyResponses = JSON.parse(readFileSync(new URL('./empty-responses.json', import.meta.url), 'utf8'))
export const demoTime = '2026-09-15T10:15:00+08:00'
export const demoStocks = [
  { symbol: '600100', market: 'cn', name: '示例科技', price: 24.68, change_pct: 1.82 },
  { symbol: '000001', market: 'cn', name: '示例银行', price: 12.35, change_pct: -0.64 },
]
export const demoQuote = {
  ...demoStocks[0], open: 24.3, high: 24.8, low: 24.1, prev_close: 24.24,
  volume: 12450000, amount: 306850000, source: 'browser_fixture', data_time: demoTime,
  freshness: { captured_at: demoTime, source_data_time: demoTime, expected_as_of: '2026-09-15', source: 'browser_fixture', market_state: 'trading', freshness_status: 'fresh' },
}
export const demoPreference = { id: 9001, user_id: 9001, risk_level: 'balanced', default_market: 'cn', horizon_pref: 'mid_term', default_rec_count: 5, enable_notify: false, enable_daily_report: false, blacklist_json: '[]', min_candidate_amount: 0, rec_filters_json: '{}', total_capital: 200000, investment_guide_version: 1, investment_guide_status: 'completed', guard_config_json: '{}' }
const demoIndices = [
  { code: '000001', name: '上证指数', price: 3286.42, change_pct: .82 },
  { code: '399001', name: '深证成指', price: 10852.76, change_pct: 1.16 },
  { code: '399006', name: '创业板指', price: 2164.32, change_pct: -.28 },
].map(item => ({ ...item, open: item.price - 12, high: item.price + 30, low: item.price - 20, prev_close: item.price - 10, source: 'browser_fixture', data_time: demoTime }))

export async function mockApi(page, options = {}) {
  const requests = [], mutations = [], unknown = []
  await page.clock.setFixedTime(new Date(demoTime))
  await page.addInitScript(({ theme, anonymous }) => {
    localStorage.setItem('qv-theme', theme)
    if (!anonymous) {
      localStorage.setItem('qv-access-token', 'browser-fixture-no-real-credential')
      localStorage.setItem('qv-refresh-token', 'browser-fixture-no-real-refresh')
      localStorage.setItem('qv-session-id', 'browser-fixture-session')
    }
  }, { theme: options.theme || 'light-blue', anonymous: !!options.anonymous })
  await page.route(url => url.pathname.startsWith('/api/'), async route => {
    const request = route.request()
    const url = new URL(request.url()), path = url.pathname.slice(4)
    requests.push(path)
    if (request.method() !== 'GET') mutations.push({ path, method: request.method(), body: request.postData() })
    const send = (data, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ success: status === 200, data, message: status === 200 ? '' : '测试场景：数据暂不可用，请重试' }) })
    if (path === '/setup/status') return send({ initialized: !options.setup, github_oauth_enabled: false, registration_open: false })
    if (path === '/user/self') return send({ id: 9001, username: 'researcher', display_name: '研究员', github_id: '', email: '', avatar_url: '', role: options.role || 'admin', status: 'active' })
    if (path === '/status') return send({ version: 'browser-test', uptime_sec: 3600, db: true, redis: false, server_time: demoTime })
    if (path === '/tasks/events') return route.fulfill({ status: 204, body: '' })
    if (path === '/onboarding') return send({ id: 1, version: 1, run: 1, status: 'completed', preference_status: 'completed', portfolio_status: 'completed', alert_status: 'skipped', should_prompt: false, suggested_step: 'complete', created_at: demoTime, updated_at: demoTime })
    if (options.errors === true || (Array.isArray(options.errors) && options.errors.includes(path))) return send(null, 503)
    if (options.responses && Object.hasOwn(options.responses, path)) {
      const response = options.responses[path]
      return send(typeof response === 'function' ? await response(url, request) : response)
    }
    if (path === '/user/preference') return send(demoPreference)
    if (path === '/user/quota') return send({ user_id: 9001, action_limit: 100, action_used: 12, token_used: 45000, request_count: 20 })
    if (path === '/markets/cn/overview') return send({ indices: demoIndices, gainers: [], actives: [], sectors: [], breadth: { advances: 3128, declines: 1860, unchanged: 122, limit_up: 46, limit_down: 8, trade_date: '2026-09-15', source: 'browser_fixture', data_time: demoTime }, fund_flow: null, errors: {}, data_time: demoTime })
    if (path.endsWith('/quote')) return send(demoQuote)
    if (path.endsWith('/valuation')) return send({ ...demoStocks[0], pe_ttm: 18.5, pe_dynamic: 19.2, pe_static: 20, pb: 2.1, total_cap: 18000000000, float_cap: 12000000000, turnover_rate: 2.5, amplitude: 2.8, volume_ratio: 1.2, limit_up: 26.66, limit_down: 21.82, is_st: false, source: 'browser_fixture', data_time: demoTime })
    if (path.endsWith('/bars') || path.endsWith('/daily')) return send([])
    if (path === '/todos') return send({ date: '2026-09-15', scope: url.searchParams.get('scope') || 'all', status: url.searchParams.get('status') || 'needs_action', source: 'all', total: 0, matched_total: 0, alerts: 0, reviews: 0, items: [], complete: true, partial: false, errors: [], scope_counts: {}, status_counts: {}, source_counts: {}, filtered: 0, page: 1, page_size: 20, has_more: false })
    if (path === '/daily-reports/latest') return send(null)
    if (path === '/portfolios') return send([{ id: 1, name: '主账户', kind: 'real', currency: 'CNY', status: 'active', is_default: true, is_system: false }])
    if (path === '/positions/overview') return send({ account_id: 1, account_name: '主账户', holding_count: 0, total_cost: 0, total_value: 0, total_profit: 0, profit_pct: 0, realized_profit: 0, win_count: 0, lose_count: 0, short_value: 0, long_value: 0, top_symbol: '', top_name: '', top_weight_pct: 0, quote_failed_count: 0, quote_stale_count: 0, signals: [] })
    if (path === '/recommendations/performance') return send({ type: url.searchParams.get('type') || 'short_term', total: 0, tracked: 0, completed: 0, win_rate: 0, by_strategy: [], by_horizon: [] })
    if (path.includes('/discovery')) return send({ status: 'idle', items: [], latest: null, enabled: false })
    if (path === '/search/stocks' || path === '/stocks/search') return send({ items: demoStocks, source: 'browser_fixture' })
    const lists = ['/watchlists', '/watchlist-items/missed', '/positions', '/positions/sell-reviews', '/positions/corp-adjusts', '/recommendations', '/recommendations/strategies', '/analysis', '/llm-configs', '/llm-tasks', '/tasks', '/news', '/alerts', '/alerts/events', '/thesis-cards', '/notes', '/daily-reports', '/prompt-templates', '/notify/channels', '/portfolios/1/cash-flows']
    if (lists.includes(path)) return send([])
    if (request.method() === 'GET' && Object.hasOwn(emptyResponses, path)) return send(emptyResponses[path])
    unknown.push(path)
    return send(null, 404)
  })
  return { requests, mutations, unknown }
}
