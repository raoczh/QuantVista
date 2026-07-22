import { request } from './client'

export type LLMTaskStatus = 'processing' | 'success' | 'failed'

/** 通用 LLM 后台任务；完成后 result 为各业务原有的响应结构。 */
export interface LLMTask<T = unknown> {
  id: number
  kind: string
  status: LLMTaskStatus
  error?: string
  error_code?: string
  created_at: string
  updated_at: string
  result?: T | null
}

export interface ListLLMTasksParams {
  kind?: string
  status?: LLMTaskStatus
  limit?: number
}

export function getLLMTask<T = unknown>(id: number) {
  return request<LLMTask<T>>({ url: `/llm-tasks/${id}` })
}

export function listLLMTasks<T = unknown>(params: ListLLMTasksParams = {}) {
  return request<LLMTask<T>[]>({ url: '/llm-tasks', params })
}

export function isLLMTask<T = unknown>(value: unknown): value is LLMTask<T> {
  if (!value || typeof value !== 'object') return false
  const task = value as Partial<LLMTask<T>>
  return typeof task.id === 'number' && typeof task.kind === 'string' && typeof task.status === 'string'
}
