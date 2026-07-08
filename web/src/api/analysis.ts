import { request, AI_TIMEOUT } from './client'
import type { EvidenceCheck, TrustReview, SysConfidence, RiskFlag } from './trust'

export type AnalysisModule = 'market' | 'sector' | 'stock' | 'watchlist' | 'position'
export type AnalysisStatus = 'success' | 'degraded' | 'failed'
export type AnalysisRating = 'bullish' | 'neutral' | 'bearish'

export interface AnalyzeRequest {
  module: AnalysisModule
  market?: string
  symbol?: string
  target?: string
  llm_config_id?: number
  question?: string
  mode?: 'panel' // 缺省=标准分析；panel=多角色观点（仅个股）
  verify?: boolean // AI 复核（独立复核员逐项挑刺；panel/降级不复核）
  as_of?: string // M2 回溯诊断日期（YYYY-MM-DD，仅个股标准模式）：截断日线组装 prompt 无未来泄露
}

// 结构化分析结果。
export interface AnalysisResult {
  rating: AnalysisRating
  confidence: number
  summary: string
  highlights: string[]
  risks: string[]
  opportunities: string[]
  suggestions: string[]
  anti_thesis: string[] // 反方观点：为什么结论可能是错的
  kill_switches: string[] // 结论失效条件
  unknowns: string[] // 数据盲区
  disclaimer: string
  // 信任层（服务端回填）
  evidence_check?: EvidenceCheck
  sys_confidence?: SysConfidence
  sys_confidence_why?: string
  review?: TrustReview
}

// 多角色观点（mode=panel）。
export type PanelRoleKind = 'technical' | 'momentum' | 'risk' | 'contrarian'
export interface PanelRole {
  role: PanelRoleKind
  rating: AnalysisRating
  summary: string
}
export interface PanelResult {
  roles: PanelRole[]
  consensus: string
  disagreement: string
}

// 与上一份同对象成功分析的差异（变化检测）。
export interface AnalysisDiff {
  prev_id: number
  prev_at: string
  prev_title: string
  rating_from: AnalysisRating | ''
  rating_to: AnalysisRating | ''
  confidence_from: number
  confidence_to: number
  confidence_delta: number
  summary_prev: string
  summary_now: string
  highlights_added: string[]
  highlights_removed: string[]
  risks_added: string[]
  risks_removed: string[]
}

// 分析记录（列表项不含 result_json/data_snapshot）。
export interface AnalysisRecord {
  id: number
  module: AnalysisModule
  market: string
  symbol: string
  target: string
  title: string
  status: AnalysisStatus
  mode: '' | 'panel'
  as_of?: string // 回溯诊断日期（空=实时分析）
  rating: AnalysisRating | ''
  confidence: number
  summary: string
  error: string
  provider: string
  model: string
  prompt_version: string
  strategy_version: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  latency_ms: number
  created_at: string
  // 详情附带：
  result_json?: string
  data_snapshot?: string
}

// 详情/发起返回的视图。
export interface AnalysisView extends AnalysisRecord {
  result: AnalysisResult | null
  panel: PanelResult | null
  raw: string
  risk_flags?: RiskFlag[] // 快照 risk_gate 程序化风险标志（S1，个股模块）
}

export function createAnalysis(req: AnalyzeRequest) {
  return request<AnalysisView>({ url: '/analysis', method: 'post', data: req, timeout: AI_TIMEOUT })
}

export function listAnalysis(module?: string, limit = 30) {
  return request<AnalysisRecord[]>({
    url: '/analysis',
    method: 'get',
    params: { module, limit },
  })
}

export function getAnalysis(id: number) {
  return request<AnalysisView>({ url: `/analysis/${id}`, method: 'get' })
}

export function deleteAnalysis(id: number) {
  return request<{ ok: boolean }>({ url: `/analysis/${id}`, method: 'delete' })
}

export function getAnalysisDiff(id: number) {
  return request<AnalysisDiff>({ url: `/analysis/${id}/diff`, method: 'get' })
}

// ---------- M2 回溯诊断：事后核验 ----------

export interface HindsightNode {
  return_pct: number
  date: string
}

export interface HindsightTouch {
  price: number
  date: string
  day_index: number
}

export interface HindsightView {
  record_id: number
  symbol: string
  name: string
  as_of: string
  base_date: string
  base_price: number
  rating: AnalysisRating | ''
  elapsed_bars: number
  returns: Record<'d5' | 'd10' | 'd20' | 'd60', HindsightNode | null>
  max_gain_pct: number
  max_drawdown_pct: number
  bench_return_pct?: number
  alpha_pct?: number
  target_touch?: HindsightTouch
  stop_touch?: HindsightTouch
  rating_hit?: boolean
  note: string
}

// 个股分析的事后核验：回溯分析按 as_of、实时分析按创建日，对照之后的真实走势。
export function getAnalysisHindsight(id: number, targetPrice?: number, stopPrice?: number) {
  return request<HindsightView>({
    url: `/analysis/${id}/hindsight`,
    method: 'get',
    params: { target_price: targetPrice || undefined, stop_price: stopPrice || undefined },
  })
}
