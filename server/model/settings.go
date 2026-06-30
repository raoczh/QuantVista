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

// DataSourceConfig 数据源配置与健康状态。
type DataSourceConfig struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	Kind          string    `gorm:"size:32;index" json:"kind"`   // quote/fundamental/news/macro
	Provider      string    `gorm:"size:32" json:"provider"`     // eastmoney/sina/tushare
	Enabled       bool      `gorm:"default:true" json:"enabled"`
	RefreshSec    int       `gorm:"default:60" json:"refresh_sec"`
	LastSyncAt    time.Time `json:"last_sync_at"`
	LastStatus    string    `gorm:"size:16" json:"last_status"` // ok/error
	LastError     string    `gorm:"size:256" json:"last_error"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
