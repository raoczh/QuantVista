import { HEAVY_TIMEOUT, request } from './client'
import type { ScoreProfile } from './screener'
import type { RecType } from './recommendation'

export type RankingAlgorithm = 'qr1' | 'additive_sp1' | 'ridge1'
export const RANKING_ALGORITHM_LABEL: Record<RankingAlgorithm, string> = {
  qr1: '质量规则', additive_sp1: '原加法评分对照', ridge1: '学习排序',
}
export interface RankingResearchRequest {
  source: 'recommendations' | 'snapshots'
  rec_type: RecType
  profile: ScoreProfile
  horizon: number
  target: 'net' | 'alpha'
  as_of?: string
  max_dates: number
  max_symbols: number
  top_k: number
}
export interface RankingResearchMetric {
  algorithm: string
  target: string
  groups: number
  selected: number
  traded: number
  skipped: number
  pending: number
  no_data: number
  forced: number
  known_targets: number
  coverage_pct: number
  benchmark_coverage_pct: number
  fill_rate_pct: number
  mean_net_pct: number
  median_net_pct: number
  p10_net_pct: number
  win_rate_pct: number
  severe_loss_pct: number
  mean_alpha_pct?: number | null
  mean_mae_pct: number
  mean_holding_days: number
  allocation_mean_pct?: number | null
  allocation_net_mean_pct?: number | null
  allocation_alpha_mean_pct?: number | null
  complete_dates: number
}
export interface RankingRidgeModel {
  version: string
  lambda: number
  samples: number
  dates: number
  weights: Array<{ feature: string; weight: number }>
}
export interface RankingResearchFold {
  train_from: string; train_to: string
  validate_from: string; validate_to: string
  test_from: string; test_to: string
  training_rows: number; purged_rows: number
  lambda?: number; status: string; reason?: string
  model?: RankingRidgeModel | null
  metrics: RankingResearchMetric[] | null
}
export interface RankingResearchReport {
  version: string
  request: RankingResearchRequest
  dataset_hash: string
  outcome_version: string
  feature_version: string
  evaluated: boolean
  reason?: string
  coverage: {
    input_rows: number; feature_rows: number; matured: number; pending: number
    skipped: number; no_data: number; forced: number; benchmark_rows: number
    finance_rows: number; trade_dates: number; universe_symbols: number; sampled_symbols: number
    reasons: Record<string, number>; sampling: string
  }
  split: { train: number; val: number; test: number; step: number; purge: number; embargo: number }
  adapted: boolean
  folds: RankingResearchFold[] | null
  descriptive: RankingResearchMetric[] | null
  out_of_time: RankingResearchMetric[] | null
  comparisons: Array<{ challenger: string; baseline: string; target: string; dates: number; block_days: number; delta_pct: number; low_95: number; high_95: number }> | null
  promotion_ready: boolean
  promotion_reasons: string[] | null
  notes: string[]
  generated_at: string
}
export interface RankingArtifact {
  id: number; rec_type: RecType; profile: ScoreProfile; horizon: number; target: 'net' | 'alpha'
  version: string; dataset_hash: string; artifact_hash: string; eligible: boolean
  as_of: string; created_at: string
}
export interface RankingPolicy {
  key: string; rec_type: RecType; profile: ScoreProfile; algorithm: RankingAlgorithm
  artifact_id: number; revision: number; updated_at: string
}
export interface RankingPolicyState {
  default_algorithm: RankingAlgorithm
  policies: RankingPolicy[]
  artifacts: RankingArtifact[]
  changes: Array<{ id: number; policy_key: string; before_json: string; after_json: string; actor_id: number; created_at: string }>
}
export function runRankingResearch(params: RankingResearchRequest, signal?: AbortSignal) {
  return request<RankingResearchReport>({ url: '/admin/ranking-research', params, signal, timeout: HEAVY_TIMEOUT })
}
export function getRankingPolicyState(signal?: AbortSignal) {
  return request<RankingPolicyState>({ url: '/admin/ranking-policy', signal })
}
export function captureRankingArtifact(report: RankingResearchReport, signal?: AbortSignal) {
  return request<RankingArtifact>({ url: '/admin/ranking-artifacts', method: 'post', data: { request: report.request, dataset_hash: report.dataset_hash }, signal, timeout: HEAVY_TIMEOUT })
}
export function updateRankingPolicy(data: { rec_type: RecType; profile: ScoreProfile; algorithm: RankingAlgorithm; artifact_id: number; base_revision: number }, signal?: AbortSignal) {
  return request<RankingPolicy>({ url: '/admin/ranking-policy', method: 'put', data, signal })
}
