import { request, AI_TIMEOUT } from './client'

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

export interface CustomStrategy {
  id: number
  name: string
  desc: string
  period: string
  risk: string
  tree: CondNode | null
  conditions: string[]
}

export interface StrategiesView {
  builtin: BuiltinStrategy[]
  custom: CustomStrategy[] | null
  factors: FactorDef[]
}

export interface ScanRequest {
  strategy_key?: string
  strategy_id?: number
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
  name: string
  desc?: string
  period?: string
  risk?: string
  tree: CondNode
}

export function getScreenerStrategies() {
  return request<StrategiesView>({ url: '/screener/strategies', method: 'get' })
}

export function screenerScan(req: ScanRequest) {
  return request<ScanResult>({ url: '/screener/scan', method: 'post', data: req })
}

export function saveScreenerStrategy(req: SaveStrategyRequest) {
  return request<CustomStrategy>({ url: '/screener/strategies', method: 'post', data: req })
}

export function deleteScreenerStrategy(id: number) {
  return request<{ deleted: boolean }>({ url: `/screener/strategies/${id}`, method: 'delete' })
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
  // 实际使用的 LLM。
  llm_config_id?: number
  provider?: string
  model?: string
}

/** 白话描述 → 条件树（消耗 1 次 AI 配额；生成树需用户确认后才落编辑器）。 */
export function parseScreenerStrategy(text: string) {
  // 走 LLM 解析，服务端合法耗时可达 90s+，不能继承 client 默认 20s。
  return request<ParseStrategyResult>({
    url: '/screener/parse',
    method: 'post',
    data: { text },
    timeout: AI_TIMEOUT,
  })
}

export const PERIOD_LABEL: Record<string, string> = { short: '短线', swing: '波段', mid: '中线' }
export const RISK_LABEL: Record<string, string> = { low: '低风险', mid: '中风险', high: '高风险' }
export const RISK_TAG_TYPE: Record<string, 'success' | 'warning' | 'error'> = {
  low: 'success',
  mid: 'warning',
  high: 'error',
}
