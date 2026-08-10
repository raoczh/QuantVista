import { request } from './client'
import type { LLMTask } from './llmTask'

// M1 条件树选股：因子宽表扫描 + 策略广场 + 自定义策略。

/** 因子元数据（自定义编辑器的因子选择与格式化）。 */
export interface FactorDef {
  key: string
  name: string
  group: string
  kind: 'price' | 'pct' | 'ratio' | 'int' | 'bool'
  desc: string
}

/** 条件树节点：all/any 组或 {factor,op,value/value2/ref} 叶子。 */
export interface CondNode {
  all?: CondNode[]
  any?: CondNode[]
  factor?: string
  op?: string
  value?: number
  value2?: number
  ref?: string
}

export interface BuiltinStrategy {
  key: string
  name: string
  desc: string
  period: 'short' | 'swing' | 'mid'
  risk: 'low' | 'mid' | 'high'
  conditions: string[]
}

export interface RetailTemplateParam {
  key: string
  label: string
  default: number
  min: number
  max: number
  step: number
  unit?: string
}

export interface RetailTemplate {
  key: string
  version: number
  name: string
  scenario: string
  risk: string
  data_requirements: string
  params: RetailTemplateParam[]
  conditions: string[]
  period: 'short' | 'swing' | 'mid'
  risk_level: 'low' | 'mid' | 'high'
}

export interface CustomStrategy {
  id: number
  current_revision_id: number
  revision: number
  content_hash: string
  name: string
  desc: string
  period: string
  risk: string
  tree: CondNode | null
  conditions: string[]
}

/** 自定义策略的不可变执行快照。 */
export interface ScreenerStrategyRevision {
  id: number
  strategy_id: number
  revision: number
  content_hash: string
  name: string
  desc: string
  period: string
  risk: string
  tree: CondNode | null
  conditions: string[]
  created_at: string
}

export interface ScreenerStrategyHistory {
  strategy_id: number
  current_revision_id: number
  revisions: ScreenerStrategyRevision[]
}

export interface StrategiesView {
  retail_templates: RetailTemplate[]
  builtin: BuiltinStrategy[]
  custom: CustomStrategy[] | null
  factors: FactorDef[]
}

export interface ScanRequest {
  template_key?: string
  template_version?: number
  template_params?: Record<string, number>
  strategy_key?: string
  strategy_id?: number
  strategy_revision_id?: number
  tree?: CondNode
  include_st?: boolean
  include_stale?: boolean
  limit?: number
}

export interface ScanHit {
  symbol: string
  name: string
  price: number
  chg_pct: number
  amount_yi: number
  turnover_rate?: number
  pos_60?: number
  reasons: string[]
}

export interface ScanResult {
  strategy: string
  strategy_id: number
  strategy_revision_id: number
  strategy_revision: number
  strategy_hash: string
  trade_date: string
  universe: number
  scanned: number
  stale_skipped: number
  st_skipped: number
  matched: number
  truncated: boolean
  items: ScanHit[] | null
  build_ms: number
  conditions: string[]
}

export type StrategyRunStatus = 'queued' | 'running' | 'success' | 'failed' | 'canceled'

/** 扫描/策略回测共用的持久结果事实；列表响应不含 request/result 正文。 */
export interface StrategyRun<T> {
  id: number
  job_run_id: number
  kind: 'screener_scan' | 'strategy_backtest'
  strategy_identity: 'builtin' | 'custom' | 'adhoc'
  strategy_key?: string
  strategy_id?: number
  strategy_revision_id?: number
  strategy_revision: number
  strategy_hash: string
  strategy_name: string
  request_hash: string
  as_of?: string
  content_hash?: string
  status: StrategyRunStatus
  error?: string
  error_code?: string
  request?: ScanRequest | Record<string, unknown>
  result?: T
  started_at?: string
  finished_at?: string
  created_at: string
  updated_at: string
}

export interface FactorTableStatus {
  ready: boolean
  building: boolean
  trade_date?: string
  built_at?: string
  build_ms?: number
  universe: number
  factors: number
}

export interface SaveStrategyRequest {
  id?: number
  base_revision_id?: number
  name: string
  desc?: string
  period?: string
  risk?: string
  tree: CondNode
}

export function getScreenerStrategies() {
  return request<StrategiesView>({ url: '/screener/strategies', method: 'get' })
}

interface RawStrategyRevision extends Omit<ScreenerStrategyRevision, 'tree' | 'conditions'> {
  tree?: CondNode | null
  tree_json?: string
  conditions?: string[] | null
}

interface RawStrategyHistory {
  strategy_id?: number
  current_revision_id?: number
  revisions?: RawStrategyRevision[] | null
  // 兼容后端将历史挂在策略列表视图上的早期响应结构。
  history?: RawStrategyRevision[] | null
  custom?: CustomStrategy[] | null
}

function parseRevisionTree(revision: RawStrategyRevision): CondNode | null {
  if (revision.tree !== undefined) return revision.tree
  if (!revision.tree_json) return null
  try {
    return JSON.parse(revision.tree_json) as CondNode
  } catch {
    return null
  }
}

/** 获取单个策略最近 50 个 revision，按 revision 降序。 */
export async function getScreenerStrategyHistory(strategyId: number): Promise<ScreenerStrategyHistory> {
  const view = await request<RawStrategyHistory>({
    url: '/screener/strategies',
    method: 'get',
    params: { history: 1, strategy_id: strategyId },
  })
  const current = view.custom?.find((item) => item.id === strategyId)
  const revisions = (view.revisions ?? view.history ?? []).map((revision) => ({
    ...revision,
    tree: parseRevisionTree(revision),
    conditions: revision.conditions ?? [],
  }))
  return {
    strategy_id: view.strategy_id ?? strategyId,
    current_revision_id: view.current_revision_id ?? current?.current_revision_id ?? 0,
    revisions,
  }
}

export function screenerScan(req: ScanRequest) {
  return request<StrategyRun<ScanResult>>({ url: '/screener/scan', method: 'post', data: req })
}

export function listScreenerResults(limit = 20) {
  return request<StrategyRun<ScanResult>[]>({ url: '/screener/results', method: 'get', params: { limit } })
}

export function getScreenerResult(id: number) {
  return request<StrategyRun<ScanResult>>({ url: `/screener/results/${id}`, method: 'get' })
}

export type WatchlistBatchStatus = 'applied' | 'undone' | 'undo_conflict'
export type WatchlistBatchItemStatus = 'created' | 'existed' | 'failed' | 'removed' | 'conflict'

export interface WatchlistBatchItem {
  id: number
  batch_id: string
  symbol: string
  market: string
  name: string
  watchlist_item_id: number
  status: WatchlistBatchItemStatus
  error_code?: string
  message?: string
}

export interface WatchlistBatch {
  id: string
  result_id: number
  group_id: number
  status: WatchlistBatchStatus
  requested: number
  created: number
  existed: number
  failed: number
  removed: number
  conflicts: number
  undone_at?: string
  created_at: string
  updated_at: string
  items: WatchlistBatchItem[]
}

export function createWatchlistBatch(resultId: number, groupId: number, symbols: string[]) {
  return request<WatchlistBatch>({
    url: `/screener/results/${resultId}/watchlist-batches`,
    method: 'post',
    data: { group_id: groupId, symbols },
  })
}

export function undoWatchlistBatch(batchId: string) {
  return request<WatchlistBatch>({
    url: `/screener/watchlist-batches/${encodeURIComponent(batchId)}/undo`,
    method: 'post',
  })
}

export function saveScreenerStrategy(req: SaveStrategyRequest) {
  return request<CustomStrategy>({ url: '/screener/strategies', method: 'post', data: req })
}

export function deleteScreenerStrategy(id: number) {
  return request<{ archived?: boolean; deleted?: boolean }>({ url: `/screener/strategies/${id}`, method: 'delete' })
}

export function getScreenerStatus() {
  return request<FactorTableStatus>({ url: '/screener/status', method: 'get' })
}

/** P3c AI 白话建策略：解析结果（tree 可为 null——全部表述都无法映射时）。 */
export interface ParseStrategyResult {
  tree: CondNode | null
  unmatched: string[] | null
  explain: string
  conditions: string[] | null
  prompt_version: string
  total_tokens: number
  // P0-2 调用追溯 ID。
  trace_id?: string
  // 实际使用的 LLM。
  llm_config_id?: number
  provider?: string
  model?: string
}

/** 白话描述 → 条件树（消耗 1 次 AI 配额；生成树需用户确认后才落编辑器）。 */
export function parseScreenerStrategy(text: string) {
  return request<LLMTask<ParseStrategyResult>>({
    url: '/screener/parse',
    method: 'post',
    data: { text },
  })
}

export const PERIOD_LABEL: Record<string, string> = { short: '短线', swing: '波段', mid: '中线' }
export const RISK_LABEL: Record<string, string> = { low: '低风险', mid: '中风险', high: '高风险' }
export const RISK_TAG_TYPE: Record<string, 'success' | 'warning' | 'error'> = {
  low: 'success',
  mid: 'warning',
  high: 'error',
}
