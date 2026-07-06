package model

import "time"

// News 新闻/快讯条目（N1 新闻舆情地基）。纯行情类缓存表，可由采集任务重建。
// 去重兜底：content_hash = MD5(标题+正文前500字) 唯一索引，重复插入静默忽略；
// 进程内两层轻量去重见 service/news.go。
// Sentiment/SentimentScore/RelatedSectors 本批只建列不填值，N2 情绪增强再写入。
type News struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	Title       string `gorm:"size:512" json:"title"`
	Content     string `gorm:"type:text" json:"content"` // 截断 3000 字
	Summary     string `gorm:"size:1024" json:"summary"`
	URL         string `gorm:"size:512" json:"url"`
	Source      string `gorm:"size:32;index" json:"source"`     // cls / eastmoney
	SourceID    string `gorm:"size:64" json:"source_id"`        // 源内唯一 ID（游标与进程内去重用）
	Category    string `gorm:"size:16;index" json:"category"`   // telegraph / flash / stock
	PublishTime time.Time `gorm:"index" json:"publish_time"`
	CollectTime time.Time `json:"collect_time"`

	// 关联标的：JSON 数组字符串，元素为本项目 symbol 口径（6 位纯代码），
	// 已经 normalizeNewsSymbol 归一；无关联为空串或 "[]"。
	RelatedSymbols string `gorm:"size:512;index" json:"related_symbols"`

	SourcePriority int    `json:"source_priority"` // 1~5，1 最高（cls 电报=1，东财快讯=2，个股新闻=3）
	ContentHash    string `gorm:"size:32;uniqueIndex" json:"content_hash"`

	// N2 情绪增强字段（本批占位）。
	Sentiment      string  `gorm:"size:16" json:"sentiment"` // positive / negative / neutral
	SentimentScore float64 `json:"sentiment_score"`
	RelatedSectors string  `gorm:"size:512" json:"related_sectors"` // JSON 数组
	ImportantMark  bool    `json:"important_mark"`                  // 源侧重要标记（cls level A/B）

	CreatedAt time.Time `json:"created_at"`
}
