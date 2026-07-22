import { request } from './client'

export interface PromptModuleInfo {
  module: string
  label: string
  default: string
  // P0-6：本模块不可覆盖的系统契约段（纪律/输出 schema，自定义时由系统自动追加在
  // 任务段之后）；分析 5 模块由组装结构保证，不返回此字段。
  contract?: string
  placeholders?: string[] // 模板可用占位符（{{name}} 形式，未提供值时原样保留）
}

export interface PromptTemplate {
  id: number
  module: string
  content: string
  enabled: boolean
  // P0-6 内容归因：content_hash=sha256 前 16 位（版本串 -custom.<hash8> 取其前 8）；
  // revision 每次内容变化递增。旧行为 0/空。
  content_hash?: string
  revision?: number
  created_at: string
  updated_at: string
}

export interface PromptInput {
  module: string
  content: string
  enabled: boolean
}

// P0-6：保存响应带 lint 警告（占位符拼写/疑似重复 schema 等，不阻断保存）。
export interface PromptUpsertResult {
  template: PromptTemplate
  warnings?: string[]
}

export function listPromptModules() {
  return request<PromptModuleInfo[]>({ url: '/prompt-templates/modules', method: 'get' })
}

export function listPromptTemplates() {
  return request<PromptTemplate[]>({ url: '/prompt-templates', method: 'get' })
}

export function upsertPromptTemplate(input: PromptInput) {
  return request<PromptUpsertResult>({ url: '/prompt-templates', method: 'post', data: input })
}

export function deletePromptTemplate(id: number) {
  return request<{ ok: boolean }>({ url: `/prompt-templates/${id}`, method: 'delete' })
}
