package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestNewsReviewCursorRetainsFailedItem(t *testing.T) {
	for _, source := range []string{newsSourceCls, newsSourceEM} {
		t.Run(source, func(t *testing.T) {
			setupTestDB(t)
			for _, table := range []any{&model.News{}, &model.Option{}} {
				if err := common.DB.Where("1 = 1").Delete(table).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().Truncate(time.Second)
			older, newer := now.Add(-12*time.Minute), now.Add(-time.Minute)
			svc := NewNewsService()
			key := optNewsCursorCls
			collect := svc.collectCls
			if source == newsSourceCls {
				svc.fetchCls = func(context.Context, int64, int) ([]datasource.ClsNewsItem, error) {
					return []datasource.ClsNewsItem{
						{SourceID: "retry", Title: "甲公司重大资产重组进展", PublishTime: older},
						{SourceID: "latest", Title: "今日银行间市场流动性状况", PublishTime: newer},
					}, nil
				}
			} else {
				key, collect = optNewsCursorEM, svc.collectEMFast
				svc.fetchEMFast = func(context.Context, string, int) ([]datasource.EMNewsItem, string, error) {
					return []datasource.EMNewsItem{
						{SourceID: "retry", Title: "甲公司重大资产重组进展", PublishTime: older},
						{SourceID: "latest", Title: "今日银行间市场流动性状况", PublishTime: newer},
					}, "", nil
				}
			}
			writeNewsCursor(key, now.Add(-30*time.Minute).Unix())
			fail := true
			const hook = "review_news_insert_failure"
			if err := common.DB.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
				if row, ok := tx.Statement.Dest.(*model.News); ok && row.SourceID == "retry" && fail {
					tx.AddError(errors.New("模拟单条新闻写入故障"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Create().Remove(hook) })
			if n := collect(t.Context()); n != 1 {
				t.Fatalf("首轮应只插入成功条目：%d", n)
			}
			if cursor := readNewsCursor(key); cursor > older.Unix()+300 {
				t.Errorf("游标越过失败条目的重叠窗：cursor=%d failed=%d", cursor, older.Unix())
			}
			fail = false
			if n := collect(t.Context()); n != 1 {
				t.Errorf("恢复后必须补回失败条目：inserted=%d", n)
			}
			var count int64
			if err := common.DB.Model(&model.News{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Errorf("不能因其他新闻推进游标永久漏采：count=%d", count)
			}
		})
	}
}

func TestNewsReviewDatabaseDuplicateAdvancesCursor(t *testing.T) {
	setupTestDB(t)
	now := time.Now().Truncate(time.Second)
	svc := NewNewsService()
	svc.fetchCls = func(context.Context, int64, int) ([]datasource.ClsNewsItem, error) {
		return []datasource.ClsNewsItem{{SourceID: "known", Title: "已在数据库中的新闻", PublishTime: now}}, nil
	}
	if err := common.DB.Create(&model.News{Title: "已在数据库中的新闻", ContentHash: newsContentHash("已在数据库中的新闻", ""), PublishTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	if n := svc.collectCls(t.Context()); n != 0 {
		t.Errorf("数据库重复不应计为新插入：%d", n)
	}
	if cursor := readNewsCursor(optNewsCursorCls); cursor != now.Unix() {
		t.Errorf("已落库重复也应推进游标：got=%d want=%d", cursor, now.Unix())
	}
}

func TestNewsReviewSameTitleExpires(t *testing.T) {
	svc := NewNewsService()
	title := "央行开展公开市场逆回购操作"
	svc.dedupeRegister(newsSourceCls, "old", title, time.Now().Add(-73*time.Hour))
	if svc.dedupeSeen(newsSourceEM, "new", title) {
		t.Error("72 小时外同标题的新事件不能一直被标题缓存过滤")
	}
	svc.dedupeRegister(newsSourceEM, "new", title, time.Now())
	if !svc.dedupeSeen(newsSourceCls, "same-event", title) {
		t.Error("窗口内相同标题仍应去重")
	}
}

func TestNewsReviewStockUniverseFiltersMarketBeforeLimit(t *testing.T) {
	setupTestDB(t)
	rows := make([]model.WatchlistItem, 0, 51)
	for i := 0; i < 50; i++ {
		rows = append(rows, model.WatchlistItem{UserID: 1, Symbol: fmt.Sprintf("%06d", i), Market: "us"})
	}
	rows = append(rows, model.WatchlistItem{UserID: 1, Symbol: "600519", Market: "cn"})
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewNewsService()
	var got []string
	svc.fetchStockNews = func(_ context.Context, symbol string, _ int) ([]datasource.EMNewsItem, error) {
		got = append(got, symbol)
		if symbol != "600519" {
			return nil, errors.New("非 A 股不应进入采集")
		}
		return nil, nil
	}
	svc.collectStockNews(t.Context())
	if len(got) != 1 || got[0] != "600519" {
		t.Errorf("非 A 股占据 LIMIT，挤掉了真正自选股：%v", got)
	}
}

func TestNewsReviewRejectsWildcardSymbol(t *testing.T) {
	setupTestDB(t)
	for _, symbol := range []string{"%", "______", "600_19", "600519%"} {
		if _, err := NewNewsService().ListNews(symbol, "", 60); err == nil {
			t.Errorf("无效代码 %q 不能当成扩大的新闻筛选", symbol)
		}
	}
}
