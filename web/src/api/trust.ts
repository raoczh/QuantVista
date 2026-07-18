// 全站信任层共享类型：证据核验 / 程序合成置信度 / AI 复核结论。
// 推荐、分析、问答、对比、日报五处 LLM 链路共用同一套契约与展示（TrustBadges.vue）。

// 数值存在性核验：单个被核验数字的明细。
export interface EvidenceItem {
  raw: string
  value: number
  unit?: string
  direction?: string // up | down
  sentence?: string
  module?: string
  count?: number
  matched: boolean
  path?: string
  snap_value?: number
  tolerance?: number
  as_of?: string
  origin?: string // 命中值域来源：空=数据快照 | plan=模型自身计划价 | user=用户设定阈值
  reason?: string // not_found | direction_mismatch
}

// 证据数字核验结果（服务端程序化比对 LLM 引用的数字与数据快照）。
export interface EvidenceCheck {
  total: number
  matched: number
  unmatched?: string[]
  version?: string // ev2 起带 items 明细
  skipped_count?: number
  unmatched_total?: number
  truncated?: boolean
  items?: EvidenceItem[]
}

// AI 复核结论（verify 模式；symbol 仅推荐域按标的复核时使用）。
export interface TrustReview {
  verdict: 'pass' | 'warn' | 'reject'
  comment?: string
  confidence?: number
  symbol?: string
}

// 程序合成置信度档位。
export type SysConfidence = 'high' | 'medium' | 'low'

// 风险闸门标志（S1：ST/退市 block、一字板/流动性 warn、小市值 info；服务端程序化判定）。
export interface RiskFlag {
  level: 'block' | 'warn' | 'info'
  code: string
  text: string
}
