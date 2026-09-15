// 全部为合成场景。名称、账户、模型和来源均不对应真实用户或服务。
import { demoQuote, demoStocks, demoTime, emptyResponses } from './mock-api.mjs'

const date = '2026-09-15'
export const accounts = [
  { id: 1, user_id: 9001, name: '主账户', kind: 'real', currency: 'CNY', status: 'active', is_default: true, created_at: demoTime, updated_at: demoTime },
  { id: 2, user_id: 9001, name: '独立模拟账户', kind: 'paper', currency: 'CNY', status: 'active', is_default: false, created_at: demoTime, updated_at: demoTime },
]
const exitSeed = {
  version: 'xp1', hash: 'fixture-exit', source: 'research', profile: 'trend', position_type: 'short_term',
  generated_at: demoTime, anchor_date: date, bars_as_of: '2026-09-14', data_status: 'ready', entry_price: 24,
  stop_price: 22, target_price: 28, extended_target: 30, initial_risk: 2, atr14: 0.8, support: 22,
  resistance: 28, trail_atr: 2.5, breakeven_r: 1, lock_fraction: 0.5, review_days: 10,
  quantity: 1000, cost: 24005, estimated_risk: 2070, estimated_reward: 3920,
  estimated_extended_reward: 5900, net_reward_risk: 1.89, slippage_bps: 5,
  evidence: ['合成场景：按完整日线和买入上沿计算风险。'], data_gaps: [],
}
export const pricePlan = {
  version: 'rp2', input_hash: 'fixture-price',
  context: { strategy_key: 'momentum_breakout', strategy_name: '动能突破', profile: 'trend', intent: 'breakout', horizon: 'short_term' },
  quote_as_of: demoTime, bars_as_of: '2026-09-14', reference_price: 24.68, status: 'wait',
  buy_low: 23.5, buy_high: 24, entry_anchor: 23.8, entry_atr: 0.8, horizon_days: 10, entry_valid_days: 3,
  reasons: ['价格已高于观察区间，等待回到区间并重新确认，不能追价。'],
  evidence: ['观察区间来自合成完整日线，目标未因当前价格上涨而抬高。'], exit: exitSeed,
}

function position(stock, index, level) {
  const known = level !== 'unknown'
  const quantity = index === 1 ? 2000 : 1000
  const buy = index === 0 ? 23.5 : index === 1 ? 12 : 20
  const cost = buy * quantity + 5
  const price = known ? stock.price : 0
  const profit = known ? price * quantity - cost : 0
  return {
    ...stock, id: index + 1, user_id: 9001, account_id: 1, position_type: 'short_term', status: 'holding', currency: 'CNY',
    buy_price: buy, buy_date: '2026-09-01', quantity, buy_fee: 5, buy_tax: 0, buy_reason: '合成场景：等待趋势确认后记录', user_note: '',
    plan_stop_loss: 22, plan_take_profit: 28, checklist_json: '[]', sell_price: 0, sell_date: '', sell_fee: 0, sell_tax: 0,
    sell_reason: '', review_note: '', sell_planned: '', ai_verdict: '', lesson_learned: '', realized_pnl: 0,
    total_buy_cost: cost, total_sell_net: 0, total_buy_qty: quantity, remaining_cost: cost, recommendation_id: 0,
    current_price: price, quote_ok: known, cost, market_value: price * quantity, profit_amount: profit,
    profit_pct: known ? 100 * profit / cost : 0, realized: false, day_change_pct: known ? stock.change_pct : 0,
    quote_as_of: known ? demoTime : '2026-09-11T15:00:00+08:00', freshness_status: known ? 'fresh' : 'unknown',
    stale_reason: known ? '' : '合成场景：行情时效未知', valuation_unavailable_reason: known ? '' : '行情时效未知，无法估值',
    last_price: known ? price : 18.46, held_trade_days: 10, short_term_review: true, near_stop_loss: false,
    below_stop_loss: false, last_analyzed_at: demoTime, analysis_stale: false,
    exit_assessment: {
      id: index + 10, user_id: 9001, account_id: 1, position_id: index + 1, ...stock,
      trade_date: date, session: 'intraday', evaluated_at: demoTime, level,
      primary_signal: level === 'urgent' ? 'protective_stop' : level,
      primary_reason: level === 'urgent' ? '当前价格已跌破 25.00 元保护线' : known ? '未发现新增退出信号' : '行情未知，无法核对保护条件',
      next_action: level === 'urgent' ? '先核对可卖数量与保护计划，不等待 AI 重新分析。' : known ? '按计划继续观察' : '先补齐行情，不把缺数据视为安全。',
      data_status: known ? 'ready' : 'unknown', trend: known ? 'intact' : 'unknown',
      quote_as_of: known ? demoTime : '', bars_as_of: '2026-09-14', quote_price: price, buy_price: buy,
      profit_pct: known ? 100 * profit / cost : 0, peak_price: 28, peak_drawdown_pct: known ? 12 : 0,
      ma20: 24, ma60: 22, atr14: 0.8, atr_line: 25, params_hash: 'fixture', fact_hash: `fixture-${index}`,
      version: 'pea5', should_todo: level !== 'normal', is_upgrade: level === 'urgent',
      signals: level === 'urgent' ? [{ key: 'protective_stop', label: '价格保护', detail: '保护线已触发', severity: 'urgent', value: price, threshold: 25, crossing: true }] : [],
      evidence: ['仅用于浏览器回归的合成持仓'], data_gaps: known ? [] : ['行情时效未知'], alert_event_ids: [], sell_review_ids: [],
    },
  }
}
export const positions = [
  position(demoStocks[0], 0, 'urgent'),
  position(demoStocks[1], 1, 'normal'),
  position({ symbol: '300100', market: 'cn', name: '示例电子', price: 18.46, change_pct: 0 }, 2, 'unknown'),
]
const positionOverview = {
  account_id: 1, account_name: '主账户', holding_count: 3, total_cost: 67515, total_value: 49380,
  total_profit: 1870, profit_pct: 0, realized_profit: 1200, win_count: 2, lose_count: 0, short_value: 49380, long_value: 0,
  top_symbol: '000001', top_name: '示例银行', top_weight_pct: 50.02, quote_failed_count: 1, quote_stale_count: 0,
  valuation_unavailable_reason: '1 笔持仓行情未知，完整估值不可用', signals: ['1 笔持仓已触发保护', '1 笔行情未知，需要核对'],
}
const riskOverview = {
  ...structuredClone(emptyResponses['/portfolios/1/overview']), account: accounts[0], as_of: date,
  total_assets: { status: 'partial', reason: '1 笔持仓行情未知，无法计算完整资产' },
  market_value: 49380, cash: { status: 'available', value: 130000 }, holding_count: 3, priced_count: 2,
  coverage_pct: 66.67, top_n_weight_pct: 100,
  holdings: positions.map(item => ({ symbol: item.symbol, market: item.market, name: item.name, industry: item.symbol === '000001' ? '银行' : '电子', quantity: item.quantity,
    ...(item.quote_ok ? { price: item.current_price, value: item.market_value, weight_pct: 100 * item.market_value / 49380 } : {}),
    status: item.quote_ok ? 'available' : 'unavailable', reason: item.valuation_unavailable_reason, valuation_known: item.quote_ok })),
  partial_reasons: ['示例电子：行情时效未知，不能按零市值处理'], data_version: 'fixture',
}
const metric = value => ({ status: 'available', value, sample_count: 30 })
const risk = {
  ...structuredClone(emptyResponses['/portfolios/1/risk']), account_id: 1, window_days: 252, parameter_hash: 'fixture',
  parameters: { annualization: 252, risk_free_rate_pct: 0, window_days: 252, benchmark_code: '000001', as_of: date, version: 'fixture' },
  twr_pct: metric(5.2), annualized_volatility_pct: metric(18.4), downside_volatility_pct: metric(12.6),
  sharpe: metric(0.84), sortino: metric(1.1), beta: metric(0.92), alpha_pct: metric(1.8),
  max_drawdown: { metric: metric(-8.3), peak_date: '2026-08-18', trough_date: '2026-09-01' },
  curve: Array.from({ length: 12 }, (_, i) => ({ trade_date: `2026-09-${String(i + 1).padStart(2, '0')}`, assets: 175000 + i * 450, return: .002, drawdown_pct: -i / 2, partial: false })),
  unknown_reasons: ['当前行情与历史风险窗口的覆盖口径不同'], data_version: 'fixture',
}

export const recommendation = {
  ...structuredClone(emptyResponses['/recommendations/1']), id: 1, type: 'short_term', market: 'cn', strategy: 'momentum_breakout',
  title: '动能突破 · 合成研究样本', status: 'success', candidate_count: 2, provider: 'fixture', model: 'browser-fixture',
  candidate_pool: JSON.stringify(demoStocks.map(stock => ({ ...stock, sources: ['strategy_signal'], score: 78, bonus: [] }))),
  prompt_version: 'p19', strategy_version: 's14', scoring_version: 'qr3', score_profile: 'momentum',
  prompt_tokens: 3500, completion_tokens: 1200, total_tokens: 4700, latency_ms: 2800,
  items: demoStocks.map((stock, index) => ({
    ...stock, id: index + 1, batch_id: 1, action: index === 0 ? 'buy' : 'watch', confidence: index === 0 ? 76 : 52,
    summary: index === 0 ? '趋势结构仍在，价格高于观察区间，等待回落确认。' : '财务资料尚不完整，继续观察。',
    ref_price: stock.price, sort_order: index, status: null, position: null,
    detail: {
      symbol: stock.symbol, action: index === 0 ? 'buy' : 'watch', confidence: index === 0 ? 76 : 52,
      reason: ['完整日线趋势尚未破坏', '成交条件仍需与最新报价核对'], risks: ['当前价格高于观察区间', '财报中的扣非利润仍需验证'],
      evidence: ['合成场景：MA20 为 24.00，最新报价为 24.68'], buy_zone_low: 23.5, buy_zone_high: 24,
      take_profit: 28, stop_loss: 22, valid_days: 3, invalidation: '完整收盘跌破结构位后重新研究',
      thesis: '', valuation_low: 0, valuation_high: 0, key_metrics: [], review_cycle: '', disclaimer: '合成测试数据',
      quant_score: index === 0 ? 78 : 55, quant_rank: index + 1, pool_size: 2, quote_as_of: demoTime,
      sys_confidence: index === 0 ? 'medium' : 'low', sys_confidence_why: '入场条件尚待确认',
      price_plan: index === 0 ? pricePlan : { ...pricePlan, reference_price: stock.price, buy_low: 0, buy_high: 0, status: 'unavailable', exit: undefined, reasons: ['必要财务资料缺失，暂不生成计划'] },
      execution_plan: { status: 'wait', preference_explanation: ['预算仅用于研究估算'], planned_capital: 25000, planned_price: 24, quantity: 1000,
        estimated_capital: 24005, unavailable_reasons: ['价格高于观察区间，等待回落确认'], data_as_of: demoTime,
        checked_price: stock.price, data_status: 'fresh', budget_basis: 'research_budget', version: 'ep6' },
    },
  })),
}
export const analysis = {
  ...structuredClone(emptyResponses['/analysis/1']), id: 1, module: 'stock', market: 'cn', symbol: '600100', target: '示例科技',
  title: '示例科技 · 合成研究样本', status: 'success', mode: '', rating: 'bullish', confidence: 68,
  summary: '趋势仍在，但盈利质量与入场价格尚待确认。', provider: 'fixture', model: 'browser-fixture',
  prompt_version: 'p23', strategy_version: 'fixture', prompt_tokens: 3200, completion_tokens: 1100, total_tokens: 4300, latency_ms: 2300,
  panel: null, raw: '', data_snapshot: JSON.stringify({ quote: demoQuote, technicals: { bar_count: 250, ma20: 24, ma60: 22, atr14: .8, as_of: '2026-09-14' },
    finance: { latest: { net_profit: -5000000, report_date: '2026-06-30' } }, price_plan: pricePlan }),
  result: {
    rating: 'bullish', confidence: 68, summary: '趋势仍在，但盈利质量与入场价格尚待确认。',
    highlights: ['完整日线趋势尚未破坏', '观察区间仍低于最新报价'], risks: ['归母利润仍为负值', '价格高于观察区间，不宜追价'],
    opportunities: ['等待价格与财务事实进一步验证'], suggestions: ['先核对财报，再观察完整收盘'],
    anti_thesis: ['盈利质量可能推翻趋势判断'], kill_switches: ['完整收盘破位后重新评估'], unknowns: ['完整现金流覆盖不足'],
    disclaimer: '合成测试数据，不用于交易', sys_confidence: 'low', sys_confidence_why: '主分析与财务反证存在冲突',
    trade_plan: { price_plan: pricePlan, no_plan: false, buy_low: 23.5, buy_high: 24, target_price: 28, stop_price: 22, horizon_days: 10 },
    debate: {
      triggered: true, trigger_reasons: ['contradictory_claims'], rounds: 1, version: 'db3',
      bull: [{ id: 'bu-01', text: '价格仍在中期均线上方', evidence_ids: ['dx-001'] }],
      bear: [{ id: 'be-01', text: '最新报告仍亏损，不能称为盈利成长', evidence_ids: ['dx-002'], confirmed: true }],
      judge: { verdict: 'neutral', decisive_claim_ids: ['be-01'], confidence_reason: '技术趋势不能消除已经发生的亏损', invalidators: ['后续同口径财报实现盈利'] },
      evidence_index: [
        { evidence_id: 'dx-001', path: 'quote.price', value: 24.68, unit: '元', as_of: demoTime, source: '合成行情' },
        { evidence_id: 'dx-002', path: 'finance.latest.net_profit', value: -5000000, unit: '元', as_of: '2026-06-30', source: '合成财报' },
      ],
    },
  },
}

const watchlists = [{ id: 1, user_id: 9001, name: '重点研究', sort_order: 0, items: positions.map((item, i) => ({
  id: i + 1, user_id: 9001, watchlist_id: 1, symbol: item.symbol, market: item.market, name: item.name,
  note: '合成场景：跟踪完整收盘与新财报', focus_reason: '等待价格回到观察区间', is_pinned: i === 0,
  research_stage: 'waiting_price', passed_reason: '', passed_price: 0, stage_at: demoTime,
  price: item.current_price, change_pct: item.day_change_pct, quote_ok: item.quote_ok, data_time: item.quote_as_of, freshness_status: item.freshness_status,
})) }]
export const todoResult = {
  date, scope: 'all', status: 'needs_action', source: 'all', total: 2, matched_total: 2, alerts: 1, reviews: 1,
  items: [
    { source_kind: 'position_exit', source_id: 10, source_version: 'fixture', kind: 'position_exit', scope: 'ledger', status: 'needs_action',
      priority: 100, symbol: '600100', market: 'cn', name: '示例科技', title: '保护价已触发', detail: '先核对真实账户可卖数量，不等待 AI 复核。',
      ref_id: 1, ref_type: 'position', deep_link: '/positions?position_id=1', time: demoTime, source_label: '持仓保护', severity: 'urgent',
      read: false, can_complete: false, group_key: 'position-1', child_count: 1, children: [] },
    { source_kind: 'rec_review', source_id: 1, source_version: 'fixture', kind: 'rec_review', scope: 'research', status: 'needs_action',
      priority: 50, symbol: '000001', market: 'cn', name: '示例银行', title: '推荐观察窗口到期', detail: '核对新的财务资料后决定是否继续观察。',
      ref_id: 1, ref_type: 'recommendation', deep_link: '/recommendations?batch_id=1', time: demoTime, source_label: '推荐复盘', severity: 'info',
      read: false, can_complete: true, group_key: 'rec-1', child_count: 1, children: [] },
  ], complete: false, partial: true, errors: ['合成场景：公司行动来源暂不可用'], scope_counts: { ledger: 1, research: 1 },
  status_counts: { needs_action: 2 }, source_counts: { position_exit: 1, rec_review: 1 }, filtered: 0, page: 1, page_size: 20, has_more: false,
}
const news = Array.from({ length: 12 }, (_, i) => ({
  id: i + 1, title: `${i % 2 ? '示例银行' : '示例科技'}发布阶段性经营资料：收入结构与现金流变化仍需结合完整报告核对`,
  summary: '这是一条浏览器回归用的合成新闻，用来检查长标题、来源、发布时间与关联股票在窄屏的布局。',
  url: `https://example.invalid/news/${i + 1}`, source: 'cls', category: 'telegraph', publish_time: demoTime,
  related_symbols: JSON.stringify([demoStocks[i % 2].symbol]), related_stocks: [demoStocks[i % 2]], source_priority: 1,
  sentiment: i % 2 ? 'negative' : '', sentiment_score: 0, important_mark: i === 0,
}))
const tasks = ['success', 'failed', 'running'].map((status, i) => ({
  id: `job:${i + 1}`, source: 'job', source_id: i + 1, result_id: 1, kind: 'analysis', owner: 'user', owner_user_id: 9001,
  title: `合成研究任务 ${i + 1}`, target: '示例科技', status, raw_status: status, stage: status === 'running' ? 'running' : 'finished',
  error: status === 'failed' ? '合成场景：来源暂不可用' : '', error_code: status === 'failed' ? 'source_unavailable' : '',
  provider: 'fixture', model: 'browser-fixture', prompt_tokens: 1200, completion_tokens: 800, total_tokens: 2000, latency_ms: 1800,
  trace_id: `fixture-trace-${i + 1}`, total: 3, succeeded: status === 'success' ? 3 : 1, failed: status === 'failed' ? 1 : 0,
  can_cancel: status === 'running', can_retry: status === 'failed', cancel_requested: false, created_at: demoTime, updated_at: demoTime,
  steps: [{ id: 1, sequence: 1, name: '准备研究证据', status: 'success', started_at: demoTime, finished_at: demoTime }],
}))
tasks.push({ ...tasks[0], id: 'job:4', source_id: 4, kind: 'recommendation', title: '合成推荐任务' })
const calls = [1, 2, 3].map(id => ({
  id, user_id: 9001, username: 'researcher', module: 'analysis', llm_config_id: 1, provider: 'fixture', model: 'browser-fixture',
  endpoint_type: 'chat_completions', stream: true, status: id === 2 ? 'error' : 'success', error_msg: id === 2 ? '合成场景：请求超时' : '',
  prompt_tokens: 3400, completion_tokens: 1200, reasoning_tokens: 400, cached_tokens: 0, total_tokens: 4600, latency_ms: 2600, first_chunk_ms: 500,
  request_body: '', response_body: '', reasoning_content: '', trace_id: `fixture-trace-${id}`, run_id: `fixture-run-${id}`,
  parent_run_id: '', attempt: 1, repair: false, structured_method: 'json_object', schema_version: 'fixture', prompt_version: 'p23',
  prompt_hash: 'fixture', data_hash: 'fixture', finish_state: 'stop', finish_state_raw: 'stop', finish_attribution: '', created_at: demoTime,
}))

export const richResponses = {
  '/portfolios': accounts,
  '/positions': url => url.searchParams.get('account_id') === '2' || url.searchParams.get('status') === 'closed' ? [] : positions,
  '/positions/overview': positionOverview,
  '/portfolios/1/overview': riskOverview,
  '/portfolios/1/risk': risk,
  '/portfolios/2/overview': { ...structuredClone(emptyResponses['/portfolios/1/overview']), account: accounts[1], total_assets: metric(100000), cash: metric(100000) },
  '/portfolios/2/risk': { ...structuredClone(emptyResponses['/portfolios/1/risk']), account_id: 2 },
  '/portfolios/2/cash-flows': [],
  '/watchlists': watchlists,
  '/todos': todoResult,
  '/recommendations': [recommendation],
  '/recommendations/1': recommendation,
  '/recommendations/strategies': [{ key: 'momentum_breakout', name: '动能突破', desc: '完整日线趋势与成交量确认，价格过高时等待', risk: 'high', group: 'rec', score_profile: 'momentum' }],
  '/analysis': [analysis],
  '/analysis/1': analysis,
  '/llm-configs': [{ id: 1, user_id: 9001, name: '合成研究模型', provider: 'fixture', base_url: 'https://example.invalid', model: 'browser-fixture',
    endpoint_type: 'chat_completions', temperature: .2, max_tokens: 4096, reasoning_effort: '', stream: true, is_default: true, has_api_key: false }],
  '/news': news,
  '/tasks': url => tasks.filter(task => (!url.searchParams.get('status') || task.status === url.searchParams.get('status'))
    && (!url.searchParams.get('source') || task.source === url.searchParams.get('source'))
    && (!url.searchParams.get('kind') || task.kind === url.searchParams.get('kind'))
    && (!url.searchParams.get('job_id') || task.source_id === Number(url.searchParams.get('job_id')))),
  '/notify-channels': [
    { id: 1, kind: 'ntfy', name: '行情提醒', enabled: true, has_target: true, last_sent_at: demoTime, last_error: '', created_at: demoTime },
    { id: 2, kind: 'webhook', name: '备用通知', enabled: true, has_target: true, last_sent_at: demoTime, last_error: '合成场景：通道请求超时', created_at: demoTime },
  ],
  '/admin/llm-calls': { items: calls, total: calls.length, length_stats: { reasoning_exhausted: 0, content_exhausted: 0 } },
}
