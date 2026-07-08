package model

import "time"

// ScreenerStrategy 用户自定义选股策略（M1 条件树选股）。
// 内置策略在代码内（service/screener_builtin.go），不落库；本表只存用户自建，
// user_id 隔离。TreeJSON 为条件树 DSL（{all/any:[...]} 与 {factor,op,value/ref} 叶子），
// 读取时经 validateCondTree 校验——历史行里因子被下线时扫描报错而非静默漏配。
type ScreenerStrategy struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index" json:"user_id"`
	Name   string `gorm:"size:64" json:"name"`
	Desc   string `gorm:"size:256" json:"desc"`
	// Period 适用周期：short（短线）/ swing（波段）/ mid（中线）。
	Period string `gorm:"size:16" json:"period"`
	// Risk 风险等级：low / mid / high。
	Risk      string    `gorm:"size:8" json:"risk"`
	TreeJSON  string    `gorm:"type:text" json:"tree_json"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
