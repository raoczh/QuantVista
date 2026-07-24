package model

import "time"

// P2-1 champion/challenger prompt 实验 + P2-2 hypothesis→experiment→feedback
// （docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3、§9、§10）。
//
// 核心语义（不可漂移）：
//   - champion 指针=既有 PromptTemplate.enabled（P0-6 体系）：晋级只是「把 challenger
//     内容经 PromptService.Upsert 落为启用中的自定义模板」——生成新的不可变
//     prompt_template_revisions 快照并切启用指针，不删旧工件；回滚=在提示词页恢复上一
//     revision 内容（快照表全程可查）。
//   - challenger 内容在**创建实验时拷贝固化**（ChallengerContent/ChallengerHash），不引用
//     活模板行——实验期间用户改模板不影响正在采样的 challenger（固定快照纪律 §9.3）。
//   - 单变量纪律（§9.3）：同一 prompt 模块同时只允许一个 running 实验；实验只改任务段
//     prompt，schema/模型/数据/路由全部与 champion 相同（同一批次同一输入对照）。
//   - 影子纪律（§10.1）：challenger 只在影子流量运行——采样只命中**实验创建者本人**的
//     推荐生成（同一用户上下文才是单变量对照），challenger 输出只落实验 run 表，
//     永不进入业务结果。
//   - P2-2 反馈闭环：Hypothesis/ExpectedImprovement 创建时必填，ActualJSON 完成时聚合，
//     Conclusion/FailureReason 由管理员按报表判定；ParentID 串父版本谱系；
//     **无增量不晋级**（promote 门槛硬检查 Conclusion=improved 等，见 service）。
type LLMExperiment struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"index" json:"user_id"` // 实验创建者（管理员）；影子采样只命中其本人请求

	// Module 业务模块（llm_call_logs.module 口径）；PromptModule 对应 PromptTemplate.module。
	// 首版仅支持 recommendation/recommend（llmExperimentSupportedModules）。
	Module       string `gorm:"size:32;index" json:"module"`
	PromptModule string `gorm:"size:16" json:"prompt_module"`

	Name                string `gorm:"size:128" json:"name"`
	Hypothesis          string `gorm:"size:512" json:"hypothesis"`           // P2-2：想验证什么
	ExpectedImprovement string `gorm:"size:512" json:"expected_improvement"` // P2-2：预期改善（可含机读指标目标）

	ChallengerContent string `gorm:"type:text" json:"challenger_content"` // 创建时固化的任务段快照
	ChallengerHash    string `gorm:"size:32" json:"challenger_hash"`
	ChampionVersion   string `gorm:"size:32" json:"champion_version"` // 创建时 champion 版本串（base 或 base-custom.hash8）
	ChampionHash      string `gorm:"size:32" json:"champion_hash"`    // champion 为自定义模板时的内容 hash（默认模板为空）

	Status       string `gorm:"size:16;index" json:"status"` // draft/running/completed/promoted/abandoned
	SampleTarget int    `json:"sample_target"`               // 影子采样目标条数（达标停采）
	SampleCount  int    `json:"sample_count"`                // 已采样本数

	ActualJSON    string `gorm:"type:text" json:"actual_json"`     // 完成时聚合指标（llmExperimentActual）
	Conclusion    string `gorm:"size:16" json:"conclusion"`        // improved / no_improvement / worse / 空=未判定
	FailureReason string `gorm:"size:512" json:"failure_reason"`   // P2-2：失败原因（未达预期时必填）
	ParentID      int64  `gorm:"index;default:0" json:"parent_id"` // 父实验（版本谱系）

	PromotedRevision int `json:"promoted_revision"` // 晋级后模板 revision（快照表可回放）

	// P2-6 一键切回 champion 的回滚锚：晋级瞬间固化「当时启用中的模板状态」——
	// PrePromoteEnabled=false 表示晋级前用默认模板（回滚=停用自定义模板）；true 表示
	// 晋级前已有启用中的自定义模板（回滚=把该内容重新落为启用模板，生成新 revision，
	// 不删工件——champion 指针语义与 promote 对称）。内容不出 JSON（体积+无展示需求）。
	PrePromoteEnabled bool       `json:"pre_promote_enabled"`
	PrePromoteContent string     `gorm:"type:text" json:"-"`
	RolledBackAt      *time.Time `json:"rolled_back_at"`

	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// LLMExperimentRun 一次影子采样工件：同一批次里 champion（业务主调）与 challenger
// （影子调用）的对照记录。TraceID/ChampionRunID 可回查 llm_call_logs 逐请求审计。
type LLMExperimentRun struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	ExperimentID int64  `gorm:"index" json:"experiment_id"`
	UserID       int64  `json:"user_id"`
	BatchID      int64  `json:"batch_id"` // 推荐批次
	TraceID      string `gorm:"size:40" json:"trace_id"`
	ChampionRun  string `gorm:"size:40" json:"champion_run_id"`

	Valid         bool   `json:"valid"`       // challenger 输出结构化解析+池校验通过
	PicksCount    int    `json:"picks_count"` // challenger 有效 picks 数
	ChampionPicks int    `json:"champion_picks"`
	OverlapCount  int    `json:"overlap_count"`                  // 与 champion picks 的标的交集
	CoverageJSON  string `gorm:"type:text" json:"coverage_json"` // challenger 侧 RecCoverageDiag

	ChampionTokens   int    `json:"champion_tokens"`
	ChallengerTokens int    `json:"challenger_tokens"`
	ChampionMs       int64  `json:"champion_ms"`
	ChallengerMs     int64  `json:"challenger_ms"`
	FinishState      string `gorm:"size:24" json:"finish_state"` // challenger 规范化终态
	Error            string `gorm:"size:512" json:"error"`       // challenger 失败原因（失败也是实验数据）

	CreatedAt time.Time `json:"created_at"`
}

// LLMReleaseAudit P2-6 自动发布门的 LLM 复核工件（PASS/FAIL）：审计员角色
// （module=release_audit）对 completed 实验做「程序硬检覆盖不了的缺口」复核，
// 每次运行落一行（完整运行工件保留，晋级门只消费最新一行且要求 hash 匹配）。
// Verdict 封闭三态：pass / fail / error（LLM 输出无效或调用失败——error 不是 FAIL
// 判定，但同样挡晋级：没有 PASS 就不进 synthesis，P1-9 语义）。
type LLMReleaseAudit struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	ExperimentID int64  `gorm:"index" json:"experiment_id"`
	UserID       int64  `json:"user_id"` // 触发审计的管理员（quota 记账主体为配置所有者）
	Verdict      string `gorm:"size:8" json:"verdict"`
	FindingsJSON string `gorm:"type:text" json:"findings_json"`
	Summary      string `gorm:"size:512" json:"summary"`
	// ChallengerHash 审计对象内容 hash（晋级门校验：审计的必须是当前 challenger 内容）。
	ChallengerHash string    `gorm:"size:32" json:"challenger_hash"`
	TraceID        string    `gorm:"size:40" json:"trace_id"` // llm-calls 可回查逐请求审计
	TokensUsed     int       `json:"tokens_used"`
	CreatedAt      time.Time `json:"created_at"`
}

// 发布审计判定封闭枚举。
const (
	ReleaseAuditPass  = "pass"
	ReleaseAuditFail  = "fail"
	ReleaseAuditError = "error"
)

// 实验状态封闭枚举。
const (
	ExpStatusDraft      = "draft"
	ExpStatusRunning    = "running"
	ExpStatusCompleted  = "completed"
	ExpStatusPromoted   = "promoted"
	ExpStatusAbandoned  = "abandoned"
	ExpStatusRolledBack = "rolled_back" // P2-6 一键切回 champion 后的终态
)

// 实验结论封闭枚举（P2-2 反馈）。
const (
	ExpConcludeImproved = "improved"
	ExpConcludeNoGain   = "no_improvement"
	ExpConcludeWorse    = "worse"
)
