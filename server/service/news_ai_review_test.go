package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestNewsAIReviewHistoricalCacheNeedsWholeDay(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	yesterday := time.Date(now.Year(), now.Month(), now.Day()-1, 9, 0, 0, 0, time.Local)
	day := yesterday.Format("2006-01-02")
	if err := common.DB.Create(&model.StockSentiment{Symbol: "600519", Date: day, Score: .8, NewsCount: 1, CreatedAt: yesterday, UpdatedAt: yesterday}).Error; err != nil {
		t.Fatal(err)
	}
	rows := []model.News{
		{Title: "早盘消息", RelatedSymbols: `["600519"]`, ContentHash: "morning", PublishTime: yesterday, SourcePriority: 1, Sentiment: "positive", SentimentScore: .8},
		{Title: "午后消息", RelatedSymbols: `["600519"]`, ContentHash: "afternoon", PublishTime: yesterday.Add(6 * time.Hour), SourcePriority: 1, Sentiment: "negative", SentimentScore: -1},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	score, count, ok := stockDailySentimentAt("600519", day, now)
	if !ok || count != 2 || score != -.1 {
		t.Fatalf("不能跨过午夜就把昨日早盘缓存冻结为全天结果：score=%v count=%d ok=%v", score, count, ok)
	}
}

func TestNewsAIReviewNeutralScoreStaysInNeutralBand(t *testing.T) {
	for _, score := range []float64{1, -1} {
		row := normalizeEnhance(newsEnhanceItem{Sentiment: "neutral", SentimentScore: score})
		if row.SentimentScore < -.1 || row.SentimentScore > .1 {
			t.Errorf("中性标注不能对候选股产生强方向分数：%+v", row)
		}
	}
}

func TestNewsAIReviewAlignmentRequiresLabeledSources(t *testing.T) {
	cases := []struct {
		name string
		rows []newsWindowStat
		want string
	}{
		{"另一来源未增强", []newsWindowStat{{Source: "cls", Sentiment: "positive"}, {Source: "eastmoney", Sentiment: ""}}, newsAlignSingleSource},
		{"另一来源身份未知", []newsWindowStat{{Source: "cls", Sentiment: "positive"}, {Source: "", Sentiment: "positive"}}, newsAlignSingleSource},
		{"只有未增强的单源", []newsWindowStat{{Source: "cls", Sentiment: ""}}, newsAlignUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeSourceAlignment(tc.rows); got != tc.want {
				t.Errorf("缺少独立已标注来源不能宣称多源一致：got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestNewsAIReviewCanceledRoundDoesNotPersistFallback(t *testing.T) {
	setupTestDB(t)
	row := model.News{Title: "央行宣布降准", ContentHash: "canceled", PublishTime: time.Now().Add(-time.Minute), SourcePriority: 1}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	NewNewsService().EnhanceNewsRound(ctx)
	if err := common.DB.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Sentiment != "" {
		t.Fatalf("已取消的增强轮次不能把待处理新闻永久标成规则结果：%s", row.Sentiment)
	}
}

func TestNewsAIReviewFutureNewsIsNotEnhanced(t *testing.T) {
	setupTestDB(t)
	row := model.News{Title: "央行宣布降准", ContentHash: "future", PublishTime: time.Now().Add(24 * time.Hour), SourcePriority: 1}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	NewNewsService().EnhanceNewsRound(t.Context())
	if err := common.DB.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Sentiment != "" {
		t.Fatalf("未来记录不能提前参与当期情绪增强：%s", row.Sentiment)
	}
}
