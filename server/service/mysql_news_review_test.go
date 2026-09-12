package service

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/model"

	"gorm.io/gorm"
)

func TestMySQLNewsReviewWindowReadSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.News{})
	now := time.Now().Truncate(time.Second)
	rows := []model.News{
		{Title: "来源甲", ContentHash: "source-one", Source: "cls", Sentiment: "positive", PublishTime: now.Add(-time.Minute), CollectTime: now, RelatedSymbols: `["600519"]`},
		{Title: "来源乙", ContentHash: "source-two", Source: "eastmoney", Sentiment: "positive", PublishTime: now.Add(-2 * time.Minute), CollectTime: now, RelatedSymbols: `["600519"]`},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var updated atomic.Bool
	const hook = "review_news_snapshot_update"
	if err := db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "news" && strings.Contains(strings.Join(tx.Statement.Selects, ","), "title, sentiment") && updated.CompareAndSwap(false, true) {
			if err := db.Model(&model.News{}).Where("id = ?", rows[1].ID).Update("sentiment", "negative").Error; err != nil {
				tx.AddError(err)
			}
			if err := db.Create(&model.News{Title: "随后入库", ContentHash: "source-three", Source: "eastmoney", Sentiment: "negative", PublishTime: now.Add(-3 * time.Minute), CollectTime: now, RelatedSymbols: `["600519"]`}).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(hook) })
	briefs, meta := latestNewsWindowAt("600519", 5, now)
	if !updated.Load() {
		t.Fatal("必须在标题读取和窗口统计之间完成真实并发提交")
	}
	if len(briefs) != 2 || meta.TotalInWindow != 2 || meta.SourceAlignment != newsAlignAligned || meta.SourceQueryStatus != "ok" {
		t.Fatalf("标题、总数、情绪对齐必须来自同一数据库快照：briefs=%+v meta=%+v", briefs, meta)
	}
}
