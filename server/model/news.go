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

	// N2 情绪增强字段：空 Sentiment = 尚未增强；增强后恒为 positive/negative/neutral
	//（含关键词规则兜底路径），据此挑选待增强行，幂等键即 news.id。
	Sentiment      string  `gorm:"size:16" json:"sentiment"` // positive / negative / neutral
	SentimentScore float64 `json:"sentiment_score"`          // -1 ~ 1
	RelatedSectors string  `gorm:"size:512" json:"related_sectors"` // JSON 数组（已对照本地板块白名单校验）
	ImpactScope    string  `gorm:"size:16" json:"impact_scope"`     // market / sector / stock
	PolicyLevel    int     `json:"policy_level"`                    // 0 无 / 3 交易所 / 4 部委 / 5 中央
	ImportantMark  bool    `json:"important_mark"`                  // 源侧重要标记（cls level A/B）

	CreatedAt time.Time `json:"created_at"`
}

// StockSentiment 个股当日聚合情绪分（N2）：由该股当日已增强新闻按来源优先级加权合成，
// (symbol, date) 唯一——一天只算一次，幂等键与逐条新闻增强（news.id）分开。
// 纯衍生缓存表，可由 news 表重建。
type StockSentiment struct {
	ID         int64   `gorm:"primaryKey" json:"id"`
	Symbol     string  `gorm:"size:16;uniqueIndex:idx_senti_sym_date" json:"symbol"`
	Date       string  `gorm:"size:10;uniqueIndex:idx_senti_sym_date" json:"date"` // 2006-01-02
	Score      float64 `json:"score"`      // -1 ~ 1
	NewsCount  int     `json:"news_count"` // 参与聚合的新闻条数（上限 12）
	DetailJSON string  `gorm:"type:text" json:"detail_json"` // 参与聚合的 {id,title,score} 明细，可复核

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
