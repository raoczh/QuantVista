import { request } from './client'

export interface PromptModuleInfo {
  module: string
  label: string
  default: string
}

export interface PromptTemplate {
  id: number
  module: string
  content: string
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface PromptInput {
  module: string
  content: string
  enabled: boolean
}

export function listPromptModules() {
  return request<PromptModuleInfo[]>({ url: '/prompt-templates/modules', method: 'get' })
}

export function listPromptTemplates() {
  return request<PromptTemplate[]>({ url: '/prompt-templates', method: 'get' })
}

export function upsertPromptTemplate(input: PromptInput) {
  return request<PromptTemplate>({ url: '/prompt-templates', method: 'post', data: input })
}

export function deletePromptTemplate(id: number) {
  return request<{ ok: boolean }>({ url: `/prompt-templates/${id}`, method: 'delete' })
}
