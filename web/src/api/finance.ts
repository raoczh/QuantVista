import { request } from './client'

// 个股财务（F2）：F10 主要财务指标 + 三大报表关键科目，最近 8 期升序。
// 首次访问触发后端按需同步（冷却 1h），非 A 股口径返回空集。

export interface FinanceIndicatorItem {
  report_date: string // YYYY-MM-DD
  report_name: string // 「2026一季报」
  notice_date: string
  eps: number | null
  bps: number | null
  ocf_ps: number | null
  revenue: number | null // 元；null 为缺失，0 为已知为零
  revenue_yoy: number | null // %
  net_profit: number | null // 元
  net_profit_yoy: number | null
  deduct_profit: number | null
  deduct_profit_yoy: number | null
  roe: number | null // 本报告期累计加权 ROE
  gross_margin: number | null
  net_margin: number | null
  debt_ratio: number | null
}

export interface FinanceStatementItem {
  report_date: string
  monetary_funds: number
  accounts_rece: number
  inventory: number
  total_assets: number
  total_liabilities: number
  total_equity: number
  operate_income: number
  operate_cost: number
  operate_profit: number
  rd_expense: number
  netcash_operate: number
  netcash_invest: number
  netcash_finance: number
}

export interface StockFinance {
  indicators: FinanceIndicatorItem[]
  statements: FinanceStatementItem[]
  note?: string
}

export function getStockFinance(market: string, symbol: string) {
  return request<StockFinance>({ url: `/markets/${market}/stocks/${symbol}/finance`, method: 'get' })
}
