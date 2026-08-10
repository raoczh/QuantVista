import http, { request } from './client'

export type DataImportKind = 'watchlist' | 'position' | 'trade'
export type DataImportStatus = 'uploaded' | 'previewed' | 'confirmed' | 'rolled_back'
export type DataImportRowStatus = 'uploaded' | 'valid' | 'error' | 'conflict'

export interface DataImportColumn {
  key: string
  label: string
  required: boolean
}

export interface DataImportRow {
  row: number
  status: DataImportRowStatus
  error_code?: string
  message?: string
  raw: Record<string, string>
  normalized?: Record<string, unknown>
}

export interface DataImportBatch {
  id: string
  user_id: number
  kind: DataImportKind
  schema_version: number
  attempt: number
  version: number
  status: DataImportStatus
  file_name: string
  file_digest: string
  mapping_digest: string
  target_group_id: number
  total_rows: number
  valid_rows: number
  error_rows: number
  conflict_rows: number
  created_rows: number
  updated_rows: number
  confirmed_at?: string
  rolled_back_at?: string
  headers: string[]
  columns: DataImportColumn[]
  suggestions: Record<string, string>
  mapping: Record<string, string>
  rows: DataImportRow[]
}

export type DataImportBatchSummary = Omit<DataImportBatch, 'headers' | 'columns' | 'suggestions' | 'mapping' | 'rows'>

export interface DataImportRollbackConflict {
  record_kind: string
  record_id: number
  message: string
}

export interface DataImportRollbackResult {
  batch_id: string
  status: 'rolled_back' | 'conflict'
  conflicts: DataImportRollbackConflict[]
}

export function uploadDataImport(kind: DataImportKind, file: File) {
  const data = new FormData()
  data.append('kind', kind)
  data.append('file', file)
  return request<DataImportBatch>({
    url: '/imports',
    method: 'post',
    data,
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: 60_000,
  })
}

export function listDataImports(limit = 20) {
  return request<DataImportBatchSummary[]>({ url: '/imports', params: { limit } })
}

export function getDataImport(id: string) {
  return request<DataImportBatch>({ url: `/imports/${id}` })
}

export function previewDataImport(
  id: string,
  input: { version: number; mapping: Record<string, string>; target_group_id?: number },
) {
  return request<DataImportBatch>({ url: `/imports/${id}/preview`, method: 'put', data: input })
}

export function confirmDataImport(id: string, version: number) {
  return request<DataImportBatch>({ url: `/imports/${id}/confirm`, method: 'post', data: { version } })
}

export function rollbackDataImport(id: string) {
  return request<DataImportRollbackResult>({ url: `/imports/${id}/rollback`, method: 'post' })
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}

export async function downloadDataImportTemplate(kind: DataImportKind) {
  const response = await http.get(`/imports/templates/${kind}`, { responseType: 'blob' })
  const type = String(response.headers['content-type'] || '')
  if (type.includes('application/json')) {
    const body = JSON.parse(await (response.data as Blob).text()) as { message?: string }
    throw new Error(body.message || '模板下载失败')
  }
  const disposition = String(response.headers['content-disposition'] || '')
  const match = disposition.match(/filename="?([^";]+)"?/)
  downloadBlob(response.data as Blob, match?.[1] || `quantvista-${kind}-import-template.csv`)
}
