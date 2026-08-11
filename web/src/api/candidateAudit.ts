export interface CandidateAuditParameters {
  audit_version: string
  outcome_version: string
  discovery_version: string
  discovery_parameter_hash: string
  factor_version: string
  selection_outcome_version: string
  label_version: string
  opportunity_top_k: number
  minimum_amount: number
  leader_net_pct: number
  leader_alpha_pct: number
  false_positive_net_pct: number
  false_positive_alpha_pct: number
  false_positive_mae_pct: number
  minimum_days: number
  minimum_samples: number
}

export interface CandidateAuditMetric {
  samples: number
  outcome_days: number
  evaluated: boolean
  alpha_samples: number
  has_alpha: boolean
  avg_net_pct: number
  avg_alpha_pct: number
  avg_mfe_pct: number
  avg_mae_pct: number
  no_data: number
  forced: number
  pending: number
  early_adverse: number
  false_positive: number
  note?: string
}

export interface CandidateAuditRateRow {
  key: string
  label: string
  total: number
  missed: number
  missed_pct: number
  outcome_days: number
  evaluated: boolean
  note?: string
}

export interface CandidateAuditFunnelRow {
  stage: string
  reason_code: string
  count: number
}

export interface CandidateAuditSliceRow {
  dimension: string
  key: string
  samples: number
  outcome_days: number
  missed: number
  adverse: number
  avg_net_pct: number
  evaluated: boolean
  note?: string
}

export interface CandidateAuditRunView {
  id: number
  signal_date: string
  outcome_date: string
  status: string
  audit_version: string
  parameter_hash: string
  data_as_of: string
  signal_data_as_of?: string
  opportunity_count: number
  batch_count: number
  item_count: number
  missed_count: number
  adverse_count: number
  gap_json: string
}

export interface CandidateAuditDetailView {
  id: number
  batch_id: number
  symbol: string
  name: string
  signal_date: string
  outcome_date: string
  audit_type: 'missed_leader' | 'false_positive'
  conclusion_code: string
  rec_type: string
  funnel_stage: string
  primary_reason_code: string
  source: string
  source_set: string
  channel: string
  channel_set: string
  opportunity_rank: number
  score_rank: number
  llm_input_order: number
  outcome_status: string
  net_return_pct: number
  alpha_pct: number
  has_bench: boolean
  mfe_pct: number
  mae_pct: number
  reason_facts_json: string
  input_refs_json: string
  outcome_refs_json: string
}

export interface CandidateAuditUserReport {
  generated_at: string
  audit_version: string
  outcome_version: string
  parameter_hash: string
  parameters: CandidateAuditParameters
  runs: CandidateAuditRunView[] | null
  outcome: CandidateAuditMetric
  funnel: CandidateAuditFunnelRow[] | null
  source_recall: CandidateAuditRateRow[] | null
  channel_recall: CandidateAuditRateRow[] | null
  slices: CandidateAuditSliceRow[] | null
  items: CandidateAuditDetailView[] | null
  notes: string[] | null
}

export interface CandidateAuditAdminReport {
  generated_at: string
  audit_version: string
  outcome_version: string
  runs: number
  outcome_days: number
  users: number
  batches: number
  outcome: CandidateAuditMetric
  llm_rejected: CandidateAuditMetric
  source_recall: CandidateAuditRateRow[] | null
  channel_recall: CandidateAuditRateRow[] | null
  funnel: CandidateAuditFunnelRow[] | null
  slices: CandidateAuditSliceRow[] | null
  notes: string[] | null
}
