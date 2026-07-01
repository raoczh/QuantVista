package model

import "time"

// 主动推送通道：提醒命中时把消息推给用户配置的外部通道（不改变"查询即提示"，仅额外主动通知）。
const (
	NotifyKindServerChan = "serverchan" // Server酱：target 为 sendkey
	NotifyKindWebhook    = "webhook"    // 自定义 webhook：target 为完整 URL，POST JSON {title,content}
)

// NotifyChannel 用户配置的推送通道。target（sendkey/url）加密落库，不回传前端明文。
type NotifyChannel struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index:idx_notify_user" json:"user_id"`
	Kind   string `gorm:"size:16" json:"kind"`
	Name   string `gorm:"size:64" json:"name"`

	TargetCipher string `gorm:"size:512" json:"-"` // 加密的 sendkey/url，绝不回传
	Enabled      bool   `json:"enabled"`

	LastSentAt *time.Time `json:"last_sent_at"`
	LastError  string     `gorm:"size:256" json:"last_error"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
