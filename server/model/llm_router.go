package model

import "time"

// P2-4 模型路由与成本优化（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3 P2-4）。
//
// 语义（不可漂移）：
//   - 一行=「某业务模块的 LLM 调用改走指定配置」（module 唯一）：路由只换调用目标
//     （BaseURL/APIKey/Model/EndpointType/Temperature），业务 prompt/schema/预算/repair
//     纪律零改动；token 配额仍记发起用户（路由是基建选择，不改记账主体）。
//   - 路由挂中央客户端 chatCompletion/chatCompletionStream 两公开出口（P0-5 声明化路由
//     同款收口），凭 chatMeta.Module 命中；flag `llm_model_routing` **缺省关**——它改变
//     业务调用目标，属行为开关非回滚开关（llm_challenger 同款语义）。
//   - **自动回退持久化**（AutoFallbackAt/AutoFallbackReason）：健康检查（失败率/成本比/
//     校准分层 Brier 对照）触发后写库并停用该路由，直到管理员显式恢复（防抖动：
//     不做自动重启，恶化原因须人看过）。重新保存路由=显式恢复。
//   - `experiment` 模块（P2-1 challenger 影子采样）不可单独配路由，恒跟随 recommendation
//     的路由——champion 与 challenger 必须打同一目标模型，否则单变量对照（§9.3）失效。
type LLMModuleRoute struct {
	ID int64 `gorm:"primaryKey" json:"id"`
	// Module 业务模块（llm_call_logs.module / llmModuleBudgets key 口径；experiment 除外）。
	Module string `gorm:"size:32;uniqueIndex" json:"module"`
	// ConfigID 路由目标 LLM 配置（须属启用状态的管理员——AllowPrivate 语义按配置所有者）。
	ConfigID int64 `gorm:"index" json:"config_id"`
	Enabled  bool  `json:"enabled"`
	// Note 管理员备注（为什么路由：如「小模型跑情绪标注省成本」）。
	Note string `gorm:"size:256" json:"note"`
	// MaxCostRatio 成本回退阈值：路由目标平均 token / 同模块其他配置平均 token 超过该比值
	// 即自动回退（0=默认 1.35，对齐 §8.1「token 成本增加不超过 35%」）。
	MaxCostRatio float64 `json:"max_cost_ratio"`

	// 自动回退状态（触发后路由停用，管理员显式恢复才重新生效）。
	AutoFallbackAt     *time.Time `json:"auto_fallback_at"`
	AutoFallbackReason string     `gorm:"size:256" json:"auto_fallback_reason"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
