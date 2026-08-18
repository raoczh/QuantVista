package model

import "time"

const (
	BrowserNotifyCategoryExitRisk = "exit_risk"
	BrowserNotifyCategoryAlert    = "manual_alert"
	BrowserNotifyCategoryGuard    = "guard"
	BrowserNotifyCategorySystem   = "system"

	BrowserDeliveryPending   = "pending"
	BrowserDeliveryDelivered = "delivered"
	BrowserDeliveryFailed    = "failed"
)

// BrowserNotificationPreference 保存浏览器通知分类偏好。卖出风险和手工提醒默认开启，
// 智能守护由用户显式开启；所有分类仍受 UserPreference.EnableNotify 总闸控制。
type BrowserNotificationPreference struct {
	UserID      int64 `gorm:"primaryKey" json:"user_id"`
	ExitRisk    bool  `gorm:"not null;default:true" json:"exit_risk"`
	ManualAlert bool  `gorm:"not null;default:true" json:"manual_alert"`
	Guard       bool  `gorm:"not null;default:false" json:"guard"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BrowserNotificationDevice 是用户在某个浏览器配置文件中的稳定设备身份。
// DeviceKeyHash 由浏览器本地随机 key 计算，不保存或回传原始 key。
type BrowserNotificationDevice struct {
	ID            int64  `gorm:"primaryKey" json:"id"`
	UserID        int64  `gorm:"uniqueIndex:idx_browser_device_owner,priority:1;index" json:"user_id"`
	DeviceKeyHash string `gorm:"size:64;uniqueIndex:idx_browser_device_owner,priority:2" json:"-"`
	Name          string `gorm:"size:80" json:"name"`
	Enabled       bool   `gorm:"not null;default:true;index" json:"enabled"`
	HasWebPush    bool   `gorm:"not null;default:false" json:"has_web_push"`

	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// WebPushSubscription 保存每台设备的 Web Push 订阅。endpoint、p256dh、auth 均为
// 敏感数据，加密落库且 API 只返回 HasWebPush，不返回这些字段或 endpoint hash。
type WebPushSubscription struct {
	ID       int64 `gorm:"primaryKey" json:"id"`
	UserID   int64 `gorm:"index;uniqueIndex:idx_webpush_owner_device,priority:1" json:"user_id"`
	DeviceID int64 `gorm:"index;uniqueIndex:idx_webpush_owner_device,priority:2" json:"device_id"`

	EndpointHash   string `gorm:"size:64;uniqueIndex:idx_webpush_endpoint" json:"-"`
	// 加密后的 endpoint 会比原始 URL 增长约三分之一；原始输入允许到 2000
	// 字符，2048 在极长但合法的浏览器 endpoint 上不够容纳密文。
	EndpointCipher string `gorm:"size:4096" json:"-"`
	P256dhCipher   string `gorm:"size:512" json:"-"`
	AuthCipher     string `gorm:"size:512" json:"-"`
	Enabled        bool   `gorm:"not null;default:true;index" json:"enabled"`

	LastSuccessAt *time.Time `json:"last_success_at"`
	LastFailureAt *time.Time `json:"last_failure_at"`
	LastErrorCode string     `gorm:"size:48" json:"last_error_code"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// BrowserNotificationEvent 是与业务事实解耦的浏览器通知声明。FactKey 由稳定来源、
// 来源 ID、事实 hash 与等级组成，不通过标题或正文猜测去重。
type BrowserNotificationEvent struct {
	ID     int64 `gorm:"primaryKey" json:"id"`
	UserID int64 `gorm:"uniqueIndex:idx_browser_event_fact,priority:1;index" json:"user_id"`

	SourceType string `gorm:"size:32;index:idx_browser_event_source,priority:1" json:"source_type"`
	SourceID   int64  `gorm:"index:idx_browser_event_source,priority:2" json:"source_id"`
	FactKey    string `gorm:"size:128;uniqueIndex:idx_browser_event_fact,priority:2" json:"fact_key"`
	Category   string `gorm:"size:24;index" json:"category"`
	Level      string `gorm:"size:16" json:"level"`

	Title string `gorm:"size:160" json:"title"`
	Body  string `gorm:"size:768" json:"body"`
	Route string `gorm:"size:512" json:"route"`

	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// BrowserNotificationDelivery 逐设备记录投递状态。唯一键保证同一业务事件不会向
// 同一设备重复声明；不同设备、风险等级升级后的新事件可分别投递。
type BrowserNotificationDelivery struct {
	ID       int64 `gorm:"primaryKey" json:"id"`
	UserID   int64 `gorm:"index;uniqueIndex:idx_browser_delivery,priority:1" json:"user_id"`
	DeviceID int64 `gorm:"index;uniqueIndex:idx_browser_delivery,priority:2" json:"device_id"`
	EventID  int64 `gorm:"index;uniqueIndex:idx_browser_delivery,priority:3" json:"event_id"`

	Status          string     `gorm:"size:16;index" json:"status"`
	PushAttemptedAt *time.Time `json:"push_attempted_at"`
	PushDeliveredAt *time.Time `json:"push_delivered_at"`
	ForegroundAckAt *time.Time `json:"foreground_ack_at"`
	LastErrorCode   string     `gorm:"size:48" json:"last_error_code"`
	AttemptCount    int        `gorm:"not null;default:0" json:"attempt_count"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
