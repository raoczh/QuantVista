package model

import "time"

// F1 财报日历三类数据 + 公告。全部为可由上游重建的缓存表（datacenter/公告接口），
// 唯一键统一 (symbol, market, report_date)（对齐项目 symbol+market 自然键惯例）。
// 三类字段差异大，分开建表一表一用途；每日盘后按 NOTICE_DATE 增量刷新（见 service/finance.go）。

// EarningsForecast 业绩预告（RPT_PUBLIC_OP_NEWPREDICT）。全年零散发布，预增/预亏类型
// 供 earn_fcst 提醒与 AI 证据池消费。
type EarningsForecast struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Symbol     string `gorm:"size:16;uniqueIndex:idx_earnfc_key" json:"symbol"`
	Market     string `gorm:"size:8;uniqueIndex:idx_earnfc_key" json:"market"`
	ReportDate string `gorm:"size:10;uniqueIndex:idx_earnfc_key" json:"report_date"` // 报告期 2026-06-30
	Name       string `gorm:"size:64" json:"name"`

	NoticeDate     string  `gorm:"size:10;index" json:"notice_date"` // 公告发布日（增量刷新游标口径）
	PredictType    string  `gorm:"size:16" json:"predict_type"`      // 预增/预亏/略增/扭亏/续盈…
	PredictFinance string  `gorm:"size:32" json:"predict_finance"`   // 预测指标（净利润/每股收益）
	AmtLower       float64 `json:"amt_lower"`                        // 预测金额下限（元或元/股，随指标）
	AmtUpper       float64 `json:"amt_upper"`
	AmpLower       float64 `json:"amp_lower"` // 变动幅度下限 %
	AmpUpper       float64 `json:"amp_upper"`
	Content        string  `gorm:"size:512" json:"content"` // 预测内容原文
	Reason         string  `gorm:"size:1024" json:"reason"` // 变动原因

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EarningsExpress 业绩快报（RPT_FCI_PERFORMANCEE）。正式财报前的关键数字快照。
type EarningsExpress struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Symbol     string `gorm:"size:16;uniqueIndex:idx_earnex_key" json:"symbol"`
	Market     string `gorm:"size:8;uniqueIndex:idx_earnex_key" json:"market"`
	ReportDate string `gorm:"size:10;uniqueIndex:idx_earnex_key" json:"report_date"`
	Name       string `gorm:"size:64" json:"name"`

	NoticeDate   string  `gorm:"size:10;index" json:"notice_date"`
	EPS          float64 `json:"eps"`                            // BASIC_EPS（元）
	Revenue      float64 `json:"revenue"`                        // 营业总收入（元）
	RevenueYoY   float64 `json:"revenue_yoy"`                    // 营收同比 %（YSTZ）
	NetProfit    float64 `json:"net_profit"`                     // 归母净利润（元）
	NetProfitYoY float64 `json:"net_profit_yoy"`                 // 净利同比 %（JLRTBZCL）
	ROE          float64 `json:"roe"`                            // 加权平均 ROE %
	DataType     string  `gorm:"size:32" json:"data_type"`       // 「2026年 一季报」

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DisclosureSchedule 财报预约披露（RPT_PUBLIC_BS_APPOIN）。AppointDate 为上游合并
// 三次变更后的最新预约日（APPOINT_PUBLISH_DATE），earn_date 提醒与日报「明日披露名单」消费。
type DisclosureSchedule struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Symbol     string `gorm:"size:16;uniqueIndex:idx_disc_key" json:"symbol"`
	Market     string `gorm:"size:8;uniqueIndex:idx_disc_key" json:"market"`
	ReportDate string `gorm:"size:10;uniqueIndex:idx_disc_key" json:"report_date"`
	Name       string `gorm:"size:64" json:"name"`

	AppointDate    string `gorm:"size:10;index" json:"appoint_date"` // 最新预约披露日
	FirstDate      string `gorm:"size:10" json:"first_date"`         // 首次预约日
	ActualDate     string `gorm:"size:10" json:"actual_date"`        // 实际披露日（未披露为空）
	ReportTypeName string `gorm:"size:32" json:"report_type_name"`   // 「2026年 半年报」
	IsPublished    bool   `json:"is_published"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Announcement 个股公告（np-anotice-stock）。按自选∪持仓每日增量拉取；
// (symbol, art_code) 唯一（同一公告可挂多只关联标的，各自成行）。
type Announcement struct {
	ID      int64  `gorm:"primaryKey" json:"id"`
	Symbol  string `gorm:"size:16;uniqueIndex:idx_ann_key" json:"symbol"`
	Market  string `gorm:"size:8" json:"market"`
	ArtCode string `gorm:"size:32;uniqueIndex:idx_ann_key" json:"art_code"`
	Name    string `gorm:"size:64" json:"name"`

	Title      string `gorm:"size:512" json:"title"`
	NoticeType string `gorm:"size:64" json:"notice_type"`
	NoticeDate string `gorm:"size:10;index" json:"notice_date"`
	URL        string `gorm:"size:256" json:"url"`

	CreatedAt time.Time `json:"created_at"`
}
