import { request, AI_TIMEOUT, refreshAccessToken } from './client'
import { getAccessToken, clearTokens } from './token'
import type { RiskFlag } from './trust'

export interface QaMessage {
  id: number
  conversation_id: number
  role: 'user' | 'assistant'
  content: string
  check_json?: string // assistant 回答的证据核验结果 JSON（服务端回填，旧消息无）
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

export function askQa(req: QaAskRequest) {
  return request<QaConversationView>({ url: '/qa/ask', method: 'post', data: req, timeout: AI_TIMEOUT })
}

// 流式问答 NDJSON 协议行（与后端 qaStreamLine 对齐；code 为批量场景预留、单标的恒空）。
interface QaStreamLine {
  module: string
  code: string
  status: 'streaming' | 'done' | 'error'
  chunk?: string
  message?: string
  data?: QaConversationView
}

// askQaStream 流式提问（S1）：fetch + getReader 逐行读 application/x-ndjson，
// 每个增量经 onChunk 回调；status=done 行携带最终会话视图（含后置核验徽章数据）。
// axios 不支持浏览器端流式读取，此处单独走 fetch，401 时复用 client 的单飞刷新重试一次。
export async function askQaStream(
  req: QaAskRequest,
  onChunk: (text: string) => void,
): Promise<QaConversationView> {
  const doFetch = () =>
    fetch('/api/qa/ask-stream', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${getAccessToken() || ''}`,
      },
      body: JSON.stringify(req),
    })

  let resp = await doFetch()
  if (resp.status === 401) {
    if (await refreshAccessToken()) {
      resp = await doFetch()
    } else {
      // 刷新失败即凭证彻底失效：与 axios 拦截器保持一致——清票并整页跳登录，
      // 否则流式路径会停留在“请求失败”文案、登录态残留不一致。/login 前缀豁免防循环。
      clearTokens()
      if (!location.pathname.startsWith('/login')) {
        location.href = '/login?redirect=' + encodeURIComponent(location.pathname + location.search)
      }
      throw new Error('登录已过期，请重新登录')
    }
  }
  // 建流前的失败（参数绑定/鉴权）走标准 JSON 包络而非 NDJSON。
  const ctype = resp.headers.get('content-type') || ''
  if (!ctype.includes('x-ndjson')) {
    let msg = `请求失败（HTTP ${resp.status}）`
    try {
      const body = (await resp.json()) as { success?: boolean; message?: string }
      if (body?.message) msg = body.message
    } catch {
      /* 保留默认消息 */
    }
    throw new Error(msg)
  }
  if (!resp.body) throw new Error('当前浏览器不支持流式读取')

  // 行缓冲状态机：网络分片与 NDJSON 行边界无关，按 \n 切分、残行滚入下一轮。
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let final: QaConversationView | null = null
  const handleLine = (raw: string) => {
    const line = raw.trim()
    if (!line) return
    let parsed: QaStreamLine
    try {
      parsed = JSON.parse(line) as QaStreamLine
    } catch {
      return // 坏行容错跳过
    }
    if (parsed.status === 'streaming' && parsed.chunk) onChunk(parsed.chunk)
    else if (parsed.status === 'error') throw new Error(parsed.message || '流式问答失败')
    else if (parsed.status === 'done' && parsed.data) final = parsed.data
  }
  for (;;) {
    const { done, value } = await reader.read()
    if (value) buf += decoder.decode(value, { stream: true })
    const lines = buf.split('\n')
    buf = lines.pop() || ''
    for (const l of lines) handleLine(l)
    if (done) {
      buf += decoder.decode()
      if (buf) handleLine(buf)
      break
    }
  }
  if (!final) throw new Error('流式响应异常中断，请重试（若已扣费可在会话列表查看是否已落库）')
  return final
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
