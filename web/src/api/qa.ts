import { request, AI_TIMEOUT } from './client'

export interface QaMessage {
  id: number
  conversation_id: number
  role: 'user' | 'assistant'
  content: string
  total_tokens: number
  created_at: string
}

export interface QaConversation {
  id: number
  symbol: string
  market: string
  name: string
  title: string
  provider: string
  model: string
  message_count: number
  total_tokens: number
  created_at: string
  updated_at: string
}

export interface QaConversationView extends QaConversation {
  messages: QaMessage[]
}

export interface QaAskRequest {
  conversation_id?: number
  symbol?: string
  market?: string
  llm_config_id?: number
  question: string
  analysis_record_id?: number // 新会话时复用该分析记录的数据快照（从分析结果「继续问答」）
}

export function askQa(req: QaAskRequest) {
  return request<QaConversationView>({ url: '/qa/ask', method: 'post', data: req, timeout: AI_TIMEOUT })
}

export function listConversations(limit = 30) {
  return request<QaConversation[]>({ url: '/qa', method: 'get', params: { limit } })
}

export function getConversation(id: number) {
  return request<QaConversationView>({ url: `/qa/${id}`, method: 'get' })
}

export function deleteConversation(id: number) {
  return request<{ ok: boolean }>({ url: `/qa/${id}`, method: 'delete' })
}
