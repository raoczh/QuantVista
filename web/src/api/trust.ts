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
  evidence_id?: string // ev4：命中项证据链 ID（ev-001…），与 path/source/as_of/snap_value 构成可回指快照的证据引用
  path?: string
  snap_value?: number
  tolerance?: number
  as_of?: string
  source?: string // ev4：命中字段的数据源标识（eastmoney/tencent/daily_bars/eastmoney_f10 等，尽力而为）
  origin?: string // 命中值域来源：空=数据快照 | plan=模型自身计划价 | user=用户输入 | context=新闻/公告标题等上下文文本
  reason?: string // not_found | direction_mismatch
}

// ev4：快照 builder 声明的结构化数据缺口（缺失≠为零；旧记录无此字段）。
export interface EvidenceUnknown {
  field_path: string
  reason: string
  impact?: string
}

// ev4：关键结论段（分析=总结/问答=回答/对比=AI点评/日报=总结）的快照佐证计数。
export interface EvidenceKeySection {
  module: string
  total: number
  snapshot_matched: number
}

// 证据数字核验结果（服务端程序化比对 LLM 引用的数字与数据快照）。
// ev3 起 matched 拆分来源：snapshot_matched 才是「被数据快照佐证」；plan/user/context
// 命中只是合法复述（模型计划价/用户输入/上下文文本），展示时不得混称「快照命中」。
// ev4 起命中项带 evidence_id/source，另有 unknowns（结构化数据缺口）与 key_section。
export interface EvidenceCheck {
  total: number
  matched: number // 总命中（legacy：含复述命中）
  unmatched?: string[]
  version?: string // ev2 起带 items 明细；ev3 起带来源分类计数；ev4 起带证据链 ID/source/unknowns
  skipped_count?: number
  unmatched_total?: number
  truncated?: boolean
  items?: EvidenceItem[]
  snapshot_matched?: number // 被数据快照佐证
  plan_matched?: number // AI 计划价复述
  user_matched?: number // 用户输入复述
  context_matched?: number // 上下文文本（新闻/公告标题、提醒文案）复述
  unknowns?: EvidenceUnknown[]
  key_section?: EvidenceKeySection
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
