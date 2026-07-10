package model

import "time"

// 机构观点（P3a）：研报评级 + 机构调研两张缓存表。
// 按需拉取+缓存（个股详情/AI 快照首次访问触发，不做全市场普查），可由上游重建。

// ReportRating 卖方研报评级（reportapi.eastmoney.com/report/list，qType=0）。
// 唯一键 (symbol, market, info_code)——infoCode 上游全局唯一，重拉靠 upsert 幂等。
// ratingChange 上游枚举：0=上调 1=下调 2=首次覆盖 3=维持 -1=缺失/无评级（落库前已归一）。
// target_price 元，0=该份研报未给目标价（覆盖率约 1/4，消费方须容错）。
type ReportRating struct {
	ID       int64  `gorm:"primaryKey" json:"id"`
	Symbol   string `gorm:"size:16;uniqueIndex:idx_reprating_key;index:idx_reprating_sym_date" json:"symbol"`
	Market   string `gorm:"size:8;uniqueIndex:idx_reprating_key" json:"market"`
	InfoCode string `gorm:"size:32;uniqueIndex:idx_reprating_key" json:"info_code"`

	ReportDate   string  `gorm:"size:10;index:idx_reprating_sym_date" json:"report_date"` // 发布日 YYYY-MM-DD
	OrgName      string  `gorm:"size:64" json:"org_name"`                                 // 机构简称
	Researcher   string  `gorm:"size:64" json:"researcher"`
	Title        string  `gorm:"size:255" json:"title"`
	Rating       string  `gorm:"size:16" json:"rating"`       // 东财归一评级（买入/增持/中性/减持/卖出，可空）
	LastRating   string  `gorm:"size:16" json:"last_rating"`  // 上次评级（可空，缺失≠首次覆盖）
	RatingChange int     `json:"rating_change"`               // 0=上调 1=下调 2=首次 3=维持 -1=缺失
	TargetPrice  float64 `json:"target_price"`                // 目标价（元，0=未给）

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OrgSurvey 机构调研按日聚合（datacenter RPT_ORG_SURVEYNEW 明细一机构一行，
// 落库前按 (symbol, survey_date) 聚合：org_count=当日参与机构行数）。
type OrgSurvey struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	Symbol     string `gorm:"size:16;uniqueIndex:idx_orgsurvey_key" json:"symbol"`
	Market     string `gorm:"size:8;uniqueIndex:idx_orgsurvey_key" json:"market"`
	SurveyDate string `gorm:"size:10;uniqueIndex:idx_orgsurvey_key" json:"survey_date"`

	NoticeDate string `gorm:"size:10" json:"notice_date"`
	OrgCount   int    `json:"org_count"`                    // 当日参与机构家数
	OrgNames   string `gorm:"size:255" json:"org_names"`    // 参与机构取样（前若干家，逗号分隔）
	ReceiveWay string `gorm:"size:128" json:"receive_way"`  // 接待方式说明

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
