package model

import "time"

// RefreshToken 落库的刷新令牌，用于换发 access token 并支持吊销/强制登出。
// 只存 token 的 sha256（TokenHash），原始值仅在签发时返回给客户端一次。
type RefreshToken struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	UserID    int64     `gorm:"index" json:"user_id"`
	TokenHash string    `gorm:"uniqueIndex;size:64" json:"-"`
	ExpiresAt time.Time `gorm:"index" json:"expires_at"`
	Revoked   bool      `gorm:"default:false" json:"revoked"`
	UserAgent string    `gorm:"size:256" json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
}
