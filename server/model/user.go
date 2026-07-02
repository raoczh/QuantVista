package model

import "time"

// User 账号。支持两种登录：用户名+密码（管理员引导建立、可选）与 GitHub OAuth。
// 首个建立的账号强制为 admin（系统拥有者）。
type User struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	Username    string    `gorm:"uniqueIndex;size:64" json:"username"`
	Password    string    `gorm:"size:128" json:"-"`          // bcrypt 哈希；空表示未设密码（纯 OAuth）
	DisplayName string    `gorm:"size:64" json:"display_name"`
	GithubID    string    `gorm:"index;size:64" json:"github_id"` // 非唯一索引（空串可多条），唯一性在应用层保证
	Email       string    `gorm:"size:128" json:"email"`
	AvatarURL   string    `gorm:"size:256" json:"avatar_url"`
	Role        string    `gorm:"size:16;default:user" json:"role"`        // user / admin
	Status      string    `gorm:"size:16;default:enabled" json:"status"`   // enabled / disabled
	// TokenVersion 令牌版本号。签发的 access token 内嵌当时版本；禁用用户/改密码时 +1，
	// 使 JWTAuth 能即时废止旧 access token（不必等其 2h 自然过期）。精确、无时间粒度问题。
	TokenVersion int       `gorm:"default:0" json:"-"`
	LastLoginAt  time.Time `json:"last_login_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
)

// UserPreference 用户偏好，1:1 关联 User。
type UserPreference struct {
	ID              int64  `gorm:"primaryKey" json:"id"`
	UserID          int64  `gorm:"uniqueIndex" json:"user_id"`
	RiskLevel       string `gorm:"size:16;default:balanced" json:"risk_level"` // conservative/balanced/aggressive
	DefaultMarket   string `gorm:"size:8;default:cn" json:"default_market"`    // cn/us/hk
	HorizonPref     string `gorm:"size:16;default:long_term" json:"horizon_pref"`
	DefaultRecCount int    `gorm:"default:3" json:"default_rec_count"`
	EnableNotify    bool   `gorm:"default:false" json:"enable_notify"`

	// 候选池回避规则（批次G）：黑名单 [{symbol,market,reason}] + 最低日成交额门槛。
	// MinCandidateAmount 单位元；0=不设流动性门槛（建新行时由服务层给默认 1e8）。
	BlacklistJSON      string  `gorm:"type:text" json:"blacklist_json"`
	MinCandidateAmount float64 `gorm:"type:decimal(20,2);default:100000000" json:"min_candidate_amount"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
