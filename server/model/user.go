package model

import "time"

// User GitHub OAuth 用户。骨架阶段仅含登录与隔离所需字段。
type User struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	GithubID  string    `gorm:"uniqueIndex;size:64" json:"github_id"`
	Username  string    `gorm:"size:64" json:"username"`
	Email     string    `gorm:"size:128" json:"email"`
	AvatarURL string    `gorm:"size:256" json:"avatar_url"`
	Role      string    `gorm:"size:16;default:user" json:"role"` // user / admin
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UserPreference 用户偏好，1:1 关联 User。
type UserPreference struct {
	ID               int64     `gorm:"primaryKey" json:"id"`
	UserID           int64     `gorm:"uniqueIndex" json:"user_id"`
	RiskLevel        string    `gorm:"size:16;default:balanced" json:"risk_level"` // conservative/balanced/aggressive
	DefaultMarket    string    `gorm:"size:8;default:cn" json:"default_market"`    // cn/us/hk
	HorizonPref      string    `gorm:"size:16;default:long_term" json:"horizon_pref"`
	DefaultRecCount  int       `gorm:"default:3" json:"default_rec_count"`
	EnableNotify     bool      `gorm:"default:false" json:"enable_notify"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
