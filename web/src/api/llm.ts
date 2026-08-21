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
  reasoning_effort: string // 空=不发送该参数，沿用网关/模型默认档位
  stream: boolean
  is_default: boolean
  has_api_key: boolean
  created_at?: string
  updated_at?: string
}

// 提交体：api_key 为明文；更新时留空表示保留原密钥。
// 不含 provider——「类型」已从表单撤除（原 openai/other 走的本就是同一条兼容路径）；
// 后端该字段仍参与能力矩阵与审计标签，新建自动填 openai、更新沿用原值。
export interface LLMConfigInput {
  name: string
  base_url: string
  api_key: string
  model: string
  endpoint_type: string
  temperature: number
  max_tokens: number
  reasoning_effort: string
  stream: boolean
  is_default: boolean
  // config_id 只在「测试连接」时带上：编辑已有配置且密钥留空时，后端复用该配置已存的密钥
  // （与拉取模型同一套三态）。增改路径后端忽略此字段。
  config_id?: number
}

export interface TestResult {
  ok: boolean
  latency_ms: number
  message: string
}

export interface LLMModelOption {
  id: string
  owned_by?: string
}

// 拉取模型入参：api_key 留空且带 config_id 时，后端复用该配置已存的密钥
//（覆盖「编辑时改了 Base URL 但不想重填密钥」）。
export interface LLMModelsInput {
  base_url: string
  api_key: string
  config_id?: number
}

export interface LLMModelsResult {
  models: LLMModelOption[]
  truncated: boolean
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

export function setDefaultLLMConfig(id: number) {
  return request<LLMConfig>({ url: `/llm-configs/${id}/default`, method: 'post' })
}

export function fetchLLMModels(input: LLMModelsInput) {
  return request<LLMModelsResult>({ url: '/llm-config-models', method: 'post', data: input })
}
