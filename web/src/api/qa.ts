import { request } from './client'
import type { LLMTask } from './llmTask'
import type { RiskFlag } from './trust'

export interface QaMessage {
  id: number
  conversation_id: number
  role: 'user' | 'assistant'
  content: string
  check_json?: string // assistant 回答的证据核验结果 JSON（服务端回填，旧消息无）
  run_id?: string // P0-2：本轮回答对应的调用组 ID（llm_call_logs.run_id 同值；旧消息无）
  total_tokens: number
  created_at: string
}

export interface QaConversation {
  id: number
  symbol: string
  market: string
  name: string
  title: string
  llm_config_id?: number // 会话创建时固化的 LLM 配置 id
  provider: string
  model: string
  message_count: number
  total_tokens: number
  trace_id?: string // P0-2：会话级调用追溯 ID（旧会话首次新提问时补写）
  created_at: string
  updated_at: string
}

export interface QaSnapshotMeta {
  captured_at?: string
  quote_as_of?: string
  bars_as_of?: string
  quote_source?: string
  freshness_status?: string // fresh | stale | unknown（快照创建时的判定，历史事实）
  market_state?: string // trading | break | pre_open | post_close | closed
  // 按读取时刻重判的当前时效（旧会话跨天后以它为准展示，而非创建时的 freshness_status）
  current_status?: string // fresh | stale | unknown
  current_note?: string
}

export interface QaConversationView extends QaConversation {
  messages: QaMessage[]
  risk_flags?: RiskFlag[] // 快照 risk_gate 程序化风险标志（S1）
  snapshot_meta?: QaSnapshotMeta // 快照行情新鲜度元数据（q9）
}

export interface QaAskRequest {
  conversation_id?: number
  symbol?: string
  market?: string
  llm_config_id?: number
  question: string
  analysis_record_id?: number // 新会话时复用该分析记录的数据快照（从分析结果「继续问答」）
  allow_stale?: boolean // 行情过期时的显式降级确认：按截至行情时刻的历史数据解释继续提问
}

export interface QaTaskResult {
  conversation_id: number
}

export function askQa(req: QaAskRequest) {
  return request<LLMTask<QaTaskResult>>({ url: '/qa/ask', method: 'post', data: req })
}

export function listConversations(limit = 30) {
  return request<QaConversation[]>({ url: '/qa', method: 'get', params: { limit } })
}

export function getConversation(id: number) {
  return request<QaConversationView>({ url: `/qa/${id}`, method: 'get' })
}

// 会话固定的数据快照原文（透明面板；详情接口刻意不带快照，按需单取）。
export function getQaSnapshot(id: number) {
  return request<{ data_snapshot: string }>({ url: `/qa/${id}/snapshot`, method: 'get' })
}

export function deleteConversation(id: number) {
  return request<{ ok: boolean }>({ url: `/qa/${id}`, method: 'delete' })
}
