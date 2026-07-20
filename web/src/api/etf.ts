import { request } from './client'

export interface EtfItem {
  symbol: string
  name: string
  index: string
  category: string
  price: number
  change_pct: number
  quote_ok: boolean
  quote_as_of?: string // 行情数据源时刻
  freshness_status?: string // fresh | stale（展示行统一过期徽标）
}

export function getEtfList() {
  return request<EtfItem[]>({ url: '/etf/list', method: 'get' })
}

// isEtfSymbol 前端按 A 股基金代码前缀判定场内基金（ETF/LOF/封基/REITs），
// 与后端 isCNFund 口径一致：沪 50/51/56/58、深 15/16/18。用于给持仓/流水标「ETF」。
export function isEtfSymbol(symbol: string): boolean {
  if (!symbol || symbol.length !== 6) return false
  return ['50', '51', '56', '58', '15', '16', '18'].includes(symbol.slice(0, 2))
}
