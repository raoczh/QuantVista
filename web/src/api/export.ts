import http, { request } from './client'

export type ExportKind = 'positions' | 'watchlist' | 'recommendations' | 'analyses'

// 触发浏览器下载。
function triggerDownload(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

// 下载 CSV 导出（走带鉴权的 axios 实例；window.open 带不上 Authorization）。
export async function downloadExport(kind: ExportKind) {
  const resp = await http.get(`/export/${kind}`, { responseType: 'blob' })
  const contentType = String(resp.headers['content-type'] || '')
  if (contentType.includes('application/json')) {
    // 出错时后端返回 JSON envelope，被 blob 包了一层。
    const text = await (resp.data as Blob).text()
    let msg = '导出失败'
    try {
      msg = JSON.parse(text)?.message || msg
    } catch {
      /* 保留默认消息 */
    }
    throw new Error(msg)
  }
  const dispo = String(resp.headers['content-disposition'] || '')
  const m = dispo.match(/filename="?([^";]+)"?/)
  triggerDownload(resp.data as Blob, m?.[1] || `${kind}.csv`)
}

export interface ImportRowError {
  row: number
  error: string
}

export interface ImportResult {
  imported: number
  failed: ImportRowError[]
}

export function importPositions(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return request<ImportResult>({
    url: '/positions/import',
    method: 'post',
    data: fd,
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: 60000,
  })
}

// 导入模板（前端本地生成，带 BOM 便于 Excel 编辑）。
export function downloadPositionTemplate() {
  const csv =
    '﻿' +
    'symbol,market,type,buy_price,buy_date,quantity,buy_fee,buy_tax,reason\n' +
    '600000,cn,short_term,8.50,2026-06-01,1000,5,0,示例行（导入前请删除）\n'
  triggerDownload(new Blob([csv], { type: 'text/csv;charset=utf-8' }), 'positions-import-template.csv')
}
