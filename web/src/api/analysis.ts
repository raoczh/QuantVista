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
