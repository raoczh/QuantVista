// 全站信任层共享类型：证据核验 / 程序合成置信度 / AI 复核结论。
// 推荐、分析、问答、对比、日报五处 LLM 链路共用同一套契约与展示（TrustBadges.vue）。

// 证据数字核验结果（服务端程序化比对 LLM 引用的数字与数据快照）。
export interface EvidenceCheck {
  total: number
  matched: number
  unmatched?: string[]
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
