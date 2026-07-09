import { request } from './client'

export interface LLMConfig {
  id: number
  user_id: number
  name: string
  provider: string
  base_url: string
  model: string
  endpoint_type: string // chat_completions（默认）/ responses
  temperature: number
  max_tokens: number
  stream: boolean
  is_default: boolean
  has_api_key: boolean
  created_at?: string
  updated_at?: string
}

// 提交体：api_key 为明文；更新时留空表示保留原密钥。
export interface LLMConfigInput {
  name: string
  provider: string
  base_url: string
  api_key: string
  model: string
  endpoint_type: string
  temperature: number
  max_tokens: number
  stream: boolean
  is_default: boolean
}

export interface TestResult {
  ok: boolean
  latency_ms: number
  message: string
}

export function listLLMConfigs() {
  return request<LLMConfig[]>({ url: '/llm-configs' })
}

export function createLLMConfig(input: LLMConfigInput) {
  return request<LLMConfig>({ url: '/llm-configs', method: 'post', data: input })
}

export function updateLLMConfig(id: number, input: LLMConfigInput) {
  return request<LLMConfig>({ url: `/llm-configs/${id}`, method: 'put', data: input })
}

export function deleteLLMConfig(id: number) {
  return request<{ ok: boolean }>({ url: `/llm-configs/${id}`, method: 'delete' })
}

export function testLLMConfig(id: number) {
  return request<TestResult>({ url: `/llm-configs/${id}/test`, method: 'post' })
}

export function testLLMDraft(input: LLMConfigInput) {
  return request<TestResult>({ url: '/llm-config-test', method: 'post', data: input })
}
