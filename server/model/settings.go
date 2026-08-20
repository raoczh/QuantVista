package model

import "time"

// LLM 请求端点类型：OpenAI 兼容 /v1/chat/completions（默认）或 /v1/responses。
const (
	LLMEndpointChat      = "chat_completions"
	LLMEndpointResponses = "responses"
)

// LLMConfig 用户级 LLM 连接配置。API Key 加密保存，不明文返回前端。
type LLMConfig struct {
	ID           int64   `gorm:"primaryKey" json:"id"`
	UserID       int64   `gorm:"index" json:"user_id"`
	Name         string  `gorm:"size:64" json:"name"`
	Provider     string  `gorm:"size:32" json:"provider"`
	BaseURL      string  `gorm:"size:256" json:"base_url"`
	APIKeyCipher string  `gorm:"size:512" json:"-"` // 加密后存储，json 永不输出
	Model        string  `gorm:"size:64" json:"model"`
	EndpointType string  `gorm:"size:24;default:chat_completions" json:"endpoint_type"` // 空值按 chat_completions
	Temperature  float64 `gorm:"default:0.7" json:"temperature"`
	// MaxTokens 新建缺省 8192（对齐 new-api Claude DefaultMaxTokens 缺省值）；无业务上限，
	// 仅请求层保留整型溢出护栏（llmGlobalHardCap）。
	MaxTokens int `gorm:"default:8192" json:"max_tokens"`
	// ReasoningEffort 推理努力度（chat: reasoning_effort / responses: reasoning.effort）。
	// 空 = 不发送该参数、沿用网关与模型默认档位。取值是自由字符串而非枚举——各家档位在持续扩
	//（o 系列只认 low/medium/high、GPT-5 加 minimal、GPT-5.5 到 none/xhigh，部分中转网关另有
	// max/ultra），与 provider 同样按「用户自由填写 + 运行时能力观察」处理。
	// 有意不加 gorm default：GORM 对零值字段会回落 DB 默认值，那样用户明确清空（要「不发送」）
	// 会被悄悄写回默认档位；新建缺省只由前端表单预填，service 层原样尊重入参。
	ReasoningEffort string    `gorm:"size:16" json:"reasoning_effort"`
	Stream          bool      `gorm:"default:true" json:"stream"`
	IsDefault       bool      `gorm:"default:false" json:"is_default"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DataSourceConfig 已删除（S1）：该表自骨架期建立后从未接线（死表）。数据源健康
// 现由 datasource.HealthTracker 进程内滑窗承担（GET /api/admin/datasources），
// 无需落库。旧库中残留的 data_source_configs 物理表无害，可手工 DROP。
