import { request, HEAVY_TIMEOUT } from './client'
import type { AuthUser } from './auth'

export interface SystemSettings {
  registration_open: boolean
  github_oauth_enabled: boolean
  github_client_id: string
  has_github_secret: boolean
  news_collect_interval_min: number
  news_auto_llm: boolean
  llm_fallback_enabled: boolean
  llm_fallback_config_id: number
  llm_accuracy_contract: boolean
  llm_evidence_refs: boolean
  llm_semantic_validator: boolean
  llm_capability_routing: boolean
  llm_conditional_debate: boolean
  llm_reflection_shadow: boolean
  llm_challenger: boolean
  llm_layered_context: boolean
  llm_model_routing: boolean
  site_base_url: string
}

// 部分更新：仅传需要改的字段。github_client_secret 留空表示保留原值。
export interface SystemSettingsUpdate {
  registration_open?: boolean
  github_oauth_enabled?: boolean
  github_client_id?: string
  github_client_secret?: string
  news_collect_interval_min?: number
  news_auto_llm?: boolean
  llm_fallback_enabled?: boolean
  llm_fallback_config_id?: number
  llm_accuracy_contract?: boolean
  llm_evidence_refs?: boolean
  llm_semantic_validator?: boolean
  llm_capability_routing?: boolean
  llm_conditional_debate?: boolean
  llm_reflection_shadow?: boolean
  llm_challenger?: boolean
  llm_layered_context?: boolean
  llm_model_routing?: boolean
  site_base_url?: string
}

export function getSystemSettings() {
  return request<SystemSettings>({ url: '/admin/settings' })
}

export function updateSystemSettings(update: SystemSettingsUpdate) {
  return request<SystemSettings>({ url: '/admin/settings', method: 'put', data: update })
}

export function listUsers() {
  return request<AuthUser[]>({ url: '/admin/users' })
}

export function setUserStatus(id: number, status: string) {
  return request<{ ok: boolean }>({ url: `/admin/users/${id}/status`, method: 'put', data: { status } })
}

// ---------- 用户 AI 配额管理 ----------

export interface AdminUserQuota {
  user_id: number
  action_limit: number // 次数上限，0 = 不限
  action_used: number // 已用次数（手动触发的 AI 动作）
  token_used: number
  request_count: number
  updated_at: string
}

export function getUserQuota(id: number) {
  return request<AdminUserQuota>({ url: `/admin/users/${id}/quota` })
}

export function updateUserQuota(id: number, data: { action_limit: number; reset_used?: boolean }) {
  return request<AdminUserQuota>({ url: `/admin/users/${id}/quota`, method: 'put', data })
}

// ---------- 数据源同步日志 ----------

export interface SyncLog {
  id: number
  task: string
  market: string
  status: string // success / partial / failed
  total: number
  succeeded: number
  failed: number
  duration_ms: number
  message: string
  created_at: string
}

export function listSyncLogs(limit = 50) {
  return request<SyncLog[]>({ url: '/admin/market/sync-logs', params: { limit } })
}

// ---------- P1 数据健康总览与补跑 ----------

export interface DataHealthItem {
  key: string
  name: string
  expected_date: string // 按交易日历应有的最新日期
  observed_date: string // 库内实际最新日期（空=无数据）
  lag_open_days: number // 落后开市日数（-1=日历不可用无法判定）
  tolerance_open_days: number
  status: string // ok / behind / empty / unknown
  coverage?: string
  last_run?: SyncLog
  note?: string
}

export interface DataHealthReport {
  generated_at: string
  items: DataHealthItem[]
}

export function getDataHealth() {
  return request<DataHealthReport>({ url: '/admin/data-health' })
}

// 补跑入口（既有管理端接口）：全市场增量 / 历史初始化 / 日线批量同步 / 情绪快照 /
// 因子宽表重建 / 日历回填。均异步或幂等，返回启动标志。
export function triggerWideSync() {
  return request<{ started: boolean }>({ url: '/admin/market/wide-sync', method: 'post' })
}
export function triggerWideInit() {
  return request<{ started: boolean }>({ url: '/admin/market/wide-init', method: 'post' })
}
export function triggerSyncBars() {
  return request<{ started: boolean }>({ url: '/admin/market/sync-bars', method: 'post', timeout: HEAVY_TIMEOUT })
}
export function triggerSnapshot() {
  return request<unknown>({ url: '/admin/market/snapshot', method: 'post', timeout: HEAVY_TIMEOUT })
}
export function triggerFactorRebuild() {
  return request<{ started: boolean }>({ url: '/admin/market/factor-rebuild', method: 'post' })
}
export function triggerBackfillCalendar() {
  return request<unknown>({ url: '/admin/market/backfill-calendar', method: 'post', timeout: HEAVY_TIMEOUT })
}

// ---------- LLM 调用审计 ----------

export interface LLMCallLogItem {
  id: number
  user_id: number
  username: string
  module: string
  llm_config_id: number
  provider: string
  model: string
  endpoint_type: string
  stream: boolean
  status: string // success / error
  error_msg: string
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  total_tokens: number
  latency_ms: number
  first_chunk_ms: number // 流式首个 data 块耗时；0=非流式；≈latency_ms 说明上游整包返回（假流式）
  request_body: string // 仅详情接口返回，列表恒为空
  response_body: string
  reasoning_content: string
  // ---- P0-2/P0-8 调用关联与完整性元数据（旧记录为空字符串/0）----
  trace_id: string // 业务结果级追溯 ID（主调/repair/复核/反方共享）
  run_id: string // 逻辑调用组 ID（主调与其 repair 同组）
  parent_run_id: string // 派生调用（复核/反方/交易计划）回指主调 run
  attempt: number // 1 基轮次；0=旧记录未记录
  repair: boolean
  structured_method: string // json_object / free_text（实际生效形态）
  schema_version: string
  prompt_version: string
  prompt_hash: string
  data_hash: string
  finish_state: string // 规范化终态；空=成功但上游未报告
  finish_state_raw: string
  finish_attribution: string // reasoning_exhausted / content_exhausted / 空
  created_at: string
}

export interface LLMCallLogList {
  items: LLMCallLogItem[]
  total: number
  length_stats: {
    reasoning_exhausted: number
    content_exhausted: number
  }
}

export function listLlmCalls(params: {
  user_id?: number
  module?: string
  status?: string
  trace?: string // trace_id 或 run_id：列出一次业务运行的全部关联调用
  page?: number
  page_size?: number
}) {
  return request<LLMCallLogList>({ url: '/admin/llm-calls', params })
}

export function getLlmCall(id: number) {
  return request<LLMCallLogItem>({ url: `/admin/llm-calls/${id}` })
}

// S3-4 因子 RankIC 验证报表（管理端只读）。
export interface ICHorizonAgg {
  mean_ic: number
  icir: number
  win_rate_pct: number
  days: number
}

export interface FactorICStat {
  key: string
  name: string
  group: string
  horizons: Record<string, ICHorizonAgg>
}

export interface FactorICReport {
  trade_date: string
  dates: string[]
  universe: number
  st_skipped: number
  adjust_suspect: number
  min_cross: number
  stats: FactorICStat[] | null
  notes: string[]
  elapsed_ms: number
  generated_at: string
}

// 默认取进程内缓存；refresh=true 全量重算（数秒级，服务端全局互斥）。
export function getFactorIC(refresh = false) {
  return request<FactorICReport>({
    url: '/admin/market/factor-ic',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// S3-5 walk-forward 评估基线报表（管理端只读）。
export interface WFSpec {
  train: number
  val: number
  test: number
  step: number
  purge: number
  embargo: number
}

export interface WFMetricFields {
  signals: number
  picked: number
  trades: number
  skipped: number
  pending: number
  precision_net_pct: number
  median_net_pct: number
  avg_net_pct: number
  severe_loss_pct: number
  alpha_sample: number
  precision_alpha_pct: number
  median_alpha_pct: number
}

export interface WFSegRow extends WFMetricFields {
  fold: number // 0=全折合并
  segment: string // val / test
  strategy: string
  strategy_name: string
  hold: number
}

export interface WFFoldView {
  fold: number
  train_range: [string, string]
  val_range: [string, string]
  test_range: [string, string]
  val_signals: number
  test_signals: number
}

export interface WFMonthlyItem {
  symbol: string
  name: string
  score: number
  status: string
  net_pct?: number
  alpha_pct?: number
}

export interface WFMonthlyRow extends WFMetricFields {
  month: string
  signal_date: string
  strategy: string
  strategy_name: string
  hold: number
  items: WFMonthlyItem[] | null
}

export interface WFSectionReport {
  rec_type: string // short_term / long_term
  holds: number[]
  strategies: string[] | null
  spec: WFSpec
  target_spec: WFSpec
  adapted: boolean
  spec_note: string
  folds: WFFoldView[] | null
  rows: WFSegRow[] | null
  monthly: WFMonthlyRow[] | null
}

export interface WalkForwardReport {
  trade_date: string
  top_k: number
  universe: number
  st_skipped: number
  adjust_suspect: number
  sections: WFSectionReport[] | null
  notes: string[]
  elapsed_ms: number
  generated_at: string
}

// 默认取进程内缓存；refresh=true 全量重算（每信号日一次全市场 as-of 重算，数十秒级）。
export function getWalkForward(refresh = false) {
  return request<WalkForwardReport>({
    url: '/admin/market/walk-forward',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// S3-6B fixed-hold selection outcome 与同批配对评估（管理端只读）。
export interface SelectionBootstrapSpec {
  seed: number
  iterations: number
}

export interface SelectionEvalCoverage {
  batches: number
  facts_ready_batches: number
  success_batches: number
  degraded_excluded: number
  facts_missing_excluded: number
  events_missing_excluded: number
  ranking_excluded: number
  duplicate_facts_excluded: number
  pick_mismatch_excluded: number
  zero_pick_batches: number
  zero_pick_rate_pct: number
  opportunity_symbols: number
  ai_picks: number
  outcome_rows: number
  outcome_matured: number
  outcome_pending: number
  outcome_skipped: number
  outcome_no_data: number
  outcome_forced: number
  challenger_runs: number
  challenger_valid_runs: number
  challenger_invalid_runs: number
  challenger_zero_pick_runs: number
}

export interface SelectionMetric {
  group: string
  label: string
  evaluated: boolean
  batches: number
  selected_symbols: number
  sample_symbols: number
  coverage_pct: number
  avg_gross_pct: number
  avg_net_pct: number
  median_net_pct: number
  p10_net_pct: number
  net_positive_pct: number
  severe_loss_pct: number
  alpha_sample: number
  avg_alpha_pct: number
  median_alpha_pct: number
  p10_alpha_pct: number
  alpha_positive_pct: number
  avg_mfe_pct: number
  median_mfe_pct: number
  avg_mae_pct: number
  median_mae_pct: number
}

export interface SelectionBootstrapCI {
  sample_batches: number
  estimate: number
  low_95: number
  high_95: number
}

export interface SelectionBatchDiff {
  batch_id: number
  signal_date: string
  k: number
  left_symbols: string[] | null
  right_symbols: string[] | null
  avg_net_diff_pct: number
  median_net_diff_pct: number
  p10_net_diff_pct: number
  net_positive_diff_pct: number
  severe_loss_diff_pct: number
  avg_mfe_diff_pct: number
  avg_mae_diff_pct: number
  has_alpha: boolean
  avg_alpha_diff_pct: number
}

export interface SelectionPairedRow {
  pair: string
  label: string
  left_group: string
  right_group: string
  batches: number
  left_wins: number
  ties: number
  right_wins: number
  avg_net_pct: SelectionBootstrapCI
  median_net_pct: SelectionBootstrapCI
  p10_net_pct: SelectionBootstrapCI
  net_positive_pct: SelectionBootstrapCI
  avg_alpha_pct: SelectionBootstrapCI
  severe_loss_pct: SelectionBootstrapCI
  avg_mfe_pct: SelectionBootstrapCI
  avg_mae_pct: SelectionBootstrapCI
  batch_diffs: SelectionBatchDiff[] | null
}

export interface SelectionSectionCoverage {
  candidate_batches: number
  comparable_batches: number
  coverage_pct: number
  missing_excluded: number
  pending_excluded: number
  skipped_excluded: number
  no_data_excluded: number
  forced_excluded: number
}

export interface SelectionPickView {
  symbol: string
  name: string
  action?: string
  order: number
  score_rank: number
}

export interface SelectionBatchView {
  batch_id: number
  signal_date: string
  n: number
  ai: SelectionPickView[] | null
  quant: SelectionPickView[] | null
  comparable: boolean
  exclusion?: string
}

export interface SelectionActionRow {
  action: string
  label: string
  metric: SelectionMetric
}

export interface SelectionActionTransition {
  from: string
  to: string
  count: number
}

export interface SelectionPlanPanel {
  coverage: SelectionSectionCoverage
  fixed_hold: SelectionMetric
  plan_l2: SelectionMetric
  pair: SelectionPairedRow
  notes: string[] | null
}

export interface SelectionSliceRow {
  key: string
  batches: number
  evaluated: boolean
  avg_net_pct?: SelectionBootstrapCI
  note?: string
}

export interface SelectionSliceGroup {
  dim: string
  label: string
  rows: SelectionSliceRow[] | null
}

export interface SelectionChallengerCoverage {
  runs: number
  metric_excluded: number
  failed_runs: number
  out_of_pool_runs: number
  native_k_min: number
  native_k_max: number
  native_k_avg: number
  native_eligible: number
  matched_eligible: number
  outcome_excluded: number
  zero_matched: number
}

export interface SelectionScoreBlindProtocolStatus {
  horizon_days: number
  window_group: string
  effective_batches: number
  min_effective_batches: number
  champion_coverage_pct: number
  score_blind_coverage_pct: number
  coverage_drop_pct: number
  max_coverage_drop_pct: number
  severe_loss_rate_pct: number
  max_severe_loss_rate_pct: number
  multiple_testing_method: string
  multiple_testing_family: number
  multiple_testing_applied: boolean
  ready: boolean
  guardrails_passed: boolean
  note: string
}

export interface SelectionChallengerEval {
  experiment_id: number
  name: string
  /** 旧 ep1 记录没有该字段时按 prompt 兼容。 */
  experiment_type?: LLMExperimentTypeValue
  input_schema_version?: string
  protocol?: LLMExperimentProtocol
  protocol_hash?: string
  protocol_status?: SelectionScoreBlindProtocolStatus
  coverage: SelectionChallengerCoverage
  groups: SelectionMetric[] | null
  pairs: SelectionPairedRow[] | null
  notes: string[] | null
}

export interface SelectionEvalSection {
  rec_type: string
  horizon_days: number
  coverage: SelectionSectionCoverage
  groups: SelectionMetric[] | null
  pairs: SelectionPairedRow[] | null
  batches: SelectionBatchView[] | null
  action_veto: SelectionActionRow[] | null
  action_transitions: SelectionActionTransition[] | null
  plan: SelectionPlanPanel
  challengers: SelectionChallengerEval[] | null
  slices: SelectionSliceGroup[] | null
  notes: string[] | null
}

export interface SelectionEvalReport {
  generated_at: string
  outcome_version: string
  schema_version: string
  ranking_version: string
  challenger_schema_version: string
  bootstrap: SelectionBootstrapSpec
  coverage: SelectionEvalCoverage
  sections: SelectionEvalSection[] | null
  notes: string[] | null
  elapsed_ms: number
}

// 默认取进程内缓存；refresh=true 推进 outcome 并全量重算，计算路径不调用 LLM。
export function getSelectionEval(refresh = false) {
  return request<SelectionEvalReport>({
    url: '/admin/selection-eval',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// ---------- P1-7 校准与后验标签报表（管理端只读，纯测量零门控） ----------

export interface CalibBucket {
  label: string
  sample: number
  avg_conf: number
  hit_rate_pct: number
  avg_net_pct: number
  gap_pct: number
}

export interface CalibTierCell {
  tier: string
  sample: number
  hit_rate_pct: number
  median_net_pct: number
  avg_net_pct: number
  avg_gross_pct: number
  avg_alpha_pct: number
  alpha_sample: number
  severe_loss_pct: number
}

export interface CalibCoverage {
  total: number
  matured: number
  pending: number
  skipped: number
  no_data: number
  forced: number
  degraded_excl: number
  orphan_excl: number
  matured_ratio_pct: number
}

export interface CalibActionPR {
  sample: number
  buy_sample: number
  precision_net_pct?: number
  recall_net_pct?: number
  watch_hit_pct?: number
  precision_alpha_pct?: number
  recall_alpha_pct?: number
  alpha_sample: number
}

/* 第五十六批②：原始口头置信度口径分列（复核改写前的模型预测快照单独测校准） */
export interface CalibRawSummary {
  sample: number
  missing: number
  diverged: number
  brier?: number
  ece?: number
}

export interface RecCalibReport {
  type: string
  horizon_days: number
  evaluated: boolean
  sample: number
  buy_sample: number
  coverage: CalibCoverage
  brier?: number
  ece?: number
  reliability?: CalibBucket[]
  raw_calib?: CalibRawSummary
  sys_tiers?: CalibTierCell[]
  tier_monotone?: string
  slices?: CalibSliceGroup[]
  action_pr: CalibActionPR
  notes: string[]
}

// P2-5 分层维度（策略/regime/provider·model/prompt_version；buy 口径）。
export interface CalibSliceRow {
  key: string
  sample: number
  hit_rate_pct: number
  avg_net_pct: number
  median_net_pct: number
  avg_alpha_pct: number
  alpha_sample: number
  brier?: number
  ece?: number
}

export interface CalibSliceGroup {
  dim: string
  label: string
  rows: CalibSliceRow[]
}

export interface AnalysisCalibTier {
  tier: string
  sample: number
  hit_rate_pct: number
  avg_ret20_pct: number
}

export interface AnalysisCalibReport {
  evaluated: boolean
  scanned: number
  judged: number
  neutral_skipped: number
  immature_skipped: number
  no_data_skipped: number
  no_sys_conf: number
  dup_skipped: number
  brier?: number
  ece?: number
  reliability?: CalibBucket[]
  sys_tiers?: AnalysisCalibTier[]
  notes: string[]
}

export interface LLMCalibrationReport {
  generated_at: string
  label_version: string
  recommendation: RecCalibReport[]
  analysis: AnalysisCalibReport
  elapsed_ms: number
  notes: string[]
}

export function getLLMCalibration(refresh = false) {
  return request<LLMCalibrationReport>({
    url: '/admin/llm-calibration',
    params: refresh ? { refresh: 1 } : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// ---------- P2-5 组合/回测联合评估（管理端只读，纯测量零门控） ----------

export interface JointEvalSegment {
  segment: string
  date_start: string
  date_end: string
  signal_days: number
  sample: number
  buy_sample: number
  win_rate_pct: number
  avg_net_pct: number
  median_net_pct: number
  p10_net_pct: number
  severe_loss_pct: number
  avg_gross_pct: number
  cost_drag_pct: number
  avg_alpha_pct: number
  alpha_sample: number
  nav_return_pct: number
  max_drawdown_pct: number
  avg_mae_pct: number
  worst_mae_pct: number
  calib_sample: number
  brier?: number
  ece?: number
  raw_calib?: CalibRawSummary
}

export interface JointLockedPreview {
  date_start: string
  date_end: string
  signal_days: number
  sample: number
}

export interface JointTurnover {
  pairs: number
  avg_new_pct: number
  avg_overlap_pct: number
}

export interface JointEvalSection {
  type: string
  horizon_days: number
  coverage: CalibCoverage
  dev?: JointEvalSegment
  locked?: JointEvalSegment
  locked_preview?: JointLockedPreview
  turnover: JointTurnover
  slices?: CalibSliceGroup[]
  notes: string[]
}

export interface JointLockedAudit {
  count: number
  last_at: string
  log?: string[]
}

export interface JointEvalReport {
  generated_at: string
  label_version: string
  include_locked: boolean
  locked_audit?: JointLockedAudit
  sections: JointEvalSection[]
  elapsed_ms: number
  notes: string[]
}

export function getJointEval(opts: { refresh?: boolean; includeLocked?: boolean } = {}) {
  const params: Record<string, number> = {}
  if (opts.refresh) params.refresh = 1
  if (opts.includeLocked) params.include_locked = 1
  return request<JointEvalReport>({
    url: '/admin/llm-joint-eval',
    params: Object.keys(params).length ? params : undefined,
    timeout: HEAVY_TIMEOUT,
  })
}

// ---------- P1-8 角色/提示词资产 registry（管理端只读声明表） ----------

export interface LLMRoleAsset {
  role_id: string
  name: string
  version: string
  schema_version: string
  purpose: string
  market: string
  horizons: string
  trigger: string
  input_whitelist: string[]
  must_answer: string[]
  forbidden_actions: string[]
  fallback: string
  max_tokens: number
  repair_attempts: number
  /* 第五十六批①：P1-6 满额坐标（每角色 ≥2 known-answer + ≥1 edge-case，测试锁定） */
  known_answers: string[]
  edge_cases: string[]
}

export interface LLMRolesResponse {
  roles: LLMRoleAsset[]
  disciplines: string[]
}

export function getLLMRoles() {
  return request<LLMRolesResponse>({ url: '/admin/llm-roles' })
}

// ---------- P2-1/P2-2 prompt challenger + S3-6C score-blind 输入实验 ----------

export type LLMExperimentType = 'prompt' | 'score_blind'

// 服务端历史行可以缺类型，未来版本也可能返回当前前端尚不认识的类型。读取模型保留
// 未知字符串，页面必须显式 fail-closed；创建接口仍只接受上面的已知类型联合。
export type LLMExperimentTypeValue = LLMExperimentType | (string & {})

export interface LLMExperimentProtocol {
  short_horizons: number[]
  long_horizons: number[]
  min_effective_batches: number
  max_coverage_drop_pct: number
  max_severe_loss_rate_pct: number
  multiple_testing_method: string
  severe_loss_definition_pct?: number
}

export interface LLMExperiment {
  id: number
  user_id: number
  /** 旧记录为空/缺失时按 prompt 读取。 */
  experiment_type?: LLMExperimentTypeValue
  input_schema_version?: string
  protocol_json?: string
  protocol_hash?: string
  protocol_locked_at?: string
  module: string
  prompt_module: string
  name: string
  hypothesis: string
  expected_improvement: string
  challenger_content: string
  challenger_hash: string
  champion_version: string
  champion_hash: string
  champion_custom: boolean
  status: 'draft' | 'running' | 'completed' | 'promoted' | 'abandoned' | 'rolled_back'
  sample_target: number
  sample_count: number
  actual_json: string
  conclusion: string
  failure_reason: string
  parent_id: number
  promoted_revision: number
  pre_promote_enabled: boolean
  /** 非空=创建实验时固化的 champion 基线已漂移；实验不可再启动、审计或晋级 */
  baseline_stale?: string
  /** 非空=回滚已失去对象（当前启用模板不再是本实验晋级产物），后端会拒绝回滚 */
  rollback_stale?: string
  rolled_back_at?: string
  started_at?: string
  completed_at?: string
  created_at: string
}

export interface LLMExperimentRun {
  id: number
  experiment_id: number
  /** 旧 ep1 记录为空/缺失时按 prompt 读取。 */
  experiment_type?: LLMExperimentTypeValue
  input_schema_version?: string
  batch_id: number
  trace_id: string
  champion_run_id: string
  seed?: number
  input_hash?: string
  input_order_json?: string
  run_status?: string
  valid: boolean
  pick_schema_version?: string
  picks_count: number
  champion_picks: number
  overlap_count: number
  coverage_json: string
  champion_tokens: number
  challenger_tokens: number
  champion_ms: number
  challenger_ms: number
  finish_state: string
  error: string
  created_at: string
}

export interface LLMExperimentActual {
  samples: number
  valid_count: number
  valid_rate_pct: number
  avg_champion_tokens: number
  avg_challenger_tokens: number
  avg_champion_ms: number
  avg_challenger_ms: number
  avg_overlap_pct: number
  avg_champion_picks: number
  avg_challenger_picks: number
  errors?: string[]
}

export interface LLMExperimentInput {
  module: string
  experiment_type?: LLMExperimentType
  name: string
  hypothesis: string
  expected_improvement: string
  challenger_content?: string
  /** score-blind 启动前必填并在创建时锁定；prompt 实验不传。 */
  protocol?: LLMExperimentProtocol
  sample_target?: number
  parent_id?: number
}

export function listLLMExperiments() {
  return request<LLMExperiment[]>({ url: '/admin/llm-experiments' })
}

export function getLLMExperiment(id: number) {
  return request<{ experiment: LLMExperiment; runs: LLMExperimentRun[]; audits: LLMReleaseAudit[] }>({ url: `/admin/llm-experiments/${id}` })
}

export function createLLMExperiment(input: LLMExperimentInput) {
  return request<{ experiment: LLMExperiment; warnings: string[] }>({
    url: '/admin/llm-experiments', method: 'post', data: input,
  })
}

export function actLLMExperiment(id: number, action: 'start' | 'complete' | 'promote' | 'rollback' | 'abandon', body?: { conclusion?: string; failure_reason?: string }) {
  return request<LLMExperiment>({ url: `/admin/llm-experiments/${id}/${action}`, method: 'post', data: body ?? {} })
}

// ---------- P2-6 自动发布门（发布审计工件） ----------

export interface LLMReleaseAuditFinding {
  code: string
  severity: 'high' | 'med' | 'low'
  message: string
}

export interface LLMReleaseAudit {
  id: number
  experiment_id: number
  user_id: number
  verdict: 'pass' | 'fail' | 'error'
  findings_json: string
  summary: string
  challenger_hash: string
  /** 审计所用 champion 基线 hash（工件与实验锚不一致时旧 PASS 不可复用） */
  champion_hash: string
  trace_id: string
  tokens_used: number
  created_at: string
}

export function auditLLMExperiment(id: number) {
  return request<LLMReleaseAudit>({ url: `/admin/llm-experiments/${id}/audit`, method: 'post', data: {} })
}

// ---------- P2-4 模型路由 ----------

export interface LLMRouteCallStats {
  total: number
  errors: number
  avg_tokens: number
  success_n: number
}

export interface LLMRouteHealth {
  routed: LLMRouteCallStats
  baseline: LLMRouteCallStats
  cost_ratio: number
  calib_brier?: number
  calib_best_peer?: number
}

export interface LLMRouteView {
  id: number
  module: string
  config_id: number
  enabled: boolean
  note: string
  max_cost_ratio: number
  auto_fallback_at?: string
  auto_fallback_reason: string
  created_at: string
  updated_at: string
  config_name: string
  config_provider: string
  config_model: string
  config_missing: boolean
  health: LLMRouteHealth
}

export interface LLMRouteModuleOption {
  module: string
  label: string
}

export interface LLMRouteInput {
  module: string
  config_id: number
  enabled: boolean
  note?: string
  max_cost_ratio?: number
}

export function listLLMRoutes() {
  return request<{ routes: LLMRouteView[]; modules: LLMRouteModuleOption[] }>({ url: '/admin/llm-routes' })
}

export function upsertLLMRoute(input: LLMRouteInput) {
  return request<LLMRouteView>({ url: '/admin/llm-routes', method: 'post', data: input })
}

export function deleteLLMRoute(id: number) {
  return request<{ deleted: boolean }>({ url: `/admin/llm-routes/${id}`, method: 'delete' })
}

export function resetLLMRoute(id: number) {
  return request<LLMRouteView>({ url: `/admin/llm-routes/${id}/reset`, method: 'post', data: {} })
}
