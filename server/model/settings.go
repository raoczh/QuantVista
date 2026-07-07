package model

import "time"

// LLMConfig 用户级 LLM 连接配置。API Key 加密保存，不明文返回前端。
type LLMConfig struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	UserID       int64     `gorm:"index" json:"user_id"`
	Name         string    `gorm:"size:64" json:"name"`
	Provider     string    `gorm:"size:32" json:"provider"`
	BaseURL      string    `gorm:"size:256" json:"base_url"`
	APIKeyCipher string    `gorm:"size:512" json:"-"` // 加密后存储，json 永不输出
	Model        string    `gorm:"size:64" json:"model"`
	Temperature  float64   `gorm:"default:0.7" json:"temperature"`
	MaxTokens    int       `gorm:"default:2048" json:"max_tokens"`
	Stream       bool      `gorm:"default:true" json:"stream"`
	IsDefault    bool      `gorm:"default:false" json:"is_default"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DataSourceConfig 已删除（S1）：该表自骨架期建立后从未接线（死表）。数据源健康
// 现由 datasource.HealthTracker 进程内滑窗承担（GET /api/admin/datasources），
// 无需落库。旧库中残留的 data_source_configs 物理表无害，可手工 DROP。

