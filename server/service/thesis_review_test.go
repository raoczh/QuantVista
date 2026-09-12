package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

type reviewThesisAdapter struct {
	bars      []datasource.Bar
	quoteTime time.Time
}

func (a reviewThesisAdapter) Name() string { return "review-thesis" }
func (a reviewThesisAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	return &datasource.Quote{Symbol: symbol, Market: market, Name: "逻辑样本", Price: 10, DataTime: a.quoteTime}, nil
}
func (a reviewThesisAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	if len(a.bars) == 0 {
		return nil, datasource.ErrNoData
	}
	return a.bars, nil
}

func TestThesisReadFailureIsNotReportedAsMissing(t *testing.T) {
	setupTestDB(t)
	const callback = "review:thesis_read_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table == "thesis_cards" {
			db.AddError(errors.New("注入逻辑卡查询失败"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	svc := NewThesisService(NewMarketService(datasource.NewManagerWithAdapters(reviewThesisAdapter{quoteTime: time.Now()})))
	if _, err := svc.GetBySymbol(990, "600094", "cn"); err == nil {
		t.Error("数据库错误不能等同于没有逻辑卡")
	}
	if _, err := svc.Upsert(context.Background(), 990, ThesisUpsertRequest{Symbol: "600094", Market: "cn", Thesis: "不能在读取失败时新建"}); err == nil {
		t.Error("读取失败不能提交新卡")
	}
	common.DB.Callback().Query().Remove(callback)
	var count int64
	if err := common.DB.Model(&model.ThesisCard{}).Where("user_id = ?", 990).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("读取失败留下了未确认的新卡：%d", count)
	}
}

func TestThesisCheckupDoesNotInventZeroHistory(t *testing.T) {
	setupTestDB(t)
	quoteTime := reviewSnapshotClock(t)
	if quoteTime.Hour()*60+quoteTime.Minute() < sessionOpenMin {
		previous := quoteTime.AddDate(0, 0, -1)
		quoteTime = time.Date(previous.Year(), previous.Month(), previous.Day(), 15, 0, 0, 0, time.Local)
	}
	card := model.ThesisCard{UserID: 991, Symbol: "600095", Market: "cn", Thesis: "检查历史缺口", Status: model.ThesisStatusActive}
	if err := common.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 10, 21} {
		bars := wideGenBars(wideGenDates(size, quoteTime), 10)
		svc := NewThesisService(NewMarketService(datasource.NewManagerWithAdapters(reviewThesisAdapter{quoteTime: quoteTime, bars: bars})))
		items, err := svc.CheckUp(context.Background(), 991)
		if err != nil || len(items) != 1 {
			t.Fatalf("体检失败：%+v %v", items, err)
		}
		if !items[0].QuoteOK {
			t.Fatal("测试需保证实时报价成功，独立验证历史失败")
		}
		raw, err := json.Marshal(items[0])
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if size >= 21 {
			if body["change_pct_20d"] != float64(0) || len(items[0].Signals) != 0 {
				t.Errorf("完整且持平的历史应保留有效 0%%：%s", raw)
			}
			continue
		}
		if body["change_pct_20d"] != nil {
			t.Errorf("只有 %d 根历史，不能报告已知 20 日涨跌：%s", size, raw)
		}
		if len(items[0].Signals) == 0 {
			t.Error("历史缺口必须提示，不能称为无异常")
		}
	}
}

func TestThesisUpsertRetainsIdentityAndName(t *testing.T) {
	setupTestDB(t)
	svc := NewThesisService(NewMarketService(datasource.NewManagerWithAdapters(reviewThesisAdapter{quoteTime: time.Now()})))
	first, err := svc.Upsert(context.Background(), 994, ThesisUpsertRequest{Symbol: "600098", Market: "cn", Thesis: "旧假设"})
	if err != nil {
		t.Fatal(err)
	}
	svc.market = nil
	second, err := svc.Upsert(context.Background(), 994, ThesisUpsertRequest{Symbol: "600098", Market: "cn", Thesis: "新假设", KeyEvidence: "新证据"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != first.Name || second.Thesis != "新假设" || second.KeyEvidence != "新证据" {
		t.Fatalf("同标的 upsert 应保持身份与已知名称，并写入新内容：%+v", second)
	}
}

func TestThesisStatusChangeDoesNotRecreateDeletedCard(t *testing.T) {
	setupTestDB(t)
	card := model.ThesisCard{UserID: 992, Symbol: "600096", Market: "cn", Thesis: "删除竞态", Status: model.ThesisStatusActive}
	if err := common.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ThesisService{}
	const callback = "review:delete_thesis_before_update"
	called := false
	if err := common.DB.Callback().Update().Before("gorm:update").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table != "thesis_cards" || called {
			return
		}
		called = true
		if err := svc.Delete(992, card.ID); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Update().Remove(callback) })
	_, _ = svc.SetStatus(992, card.ID, model.ThesisStatusArchived, "")
	var count int64
	if err := common.DB.Model(&model.ThesisCard{}).Where("id = ?", card.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !called || count != 0 {
		t.Fatalf("状态更新复活已删除卡：called=%v count=%d", called, count)
	}
}

func TestThesisArchivePreservesInvalidationReason(t *testing.T) {
	setupTestDB(t)
	card := model.ThesisCard{UserID: 993, Symbol: "600097", Market: "cn", Thesis: "旧逻辑", Status: model.ThesisStatusInvalidated, InvalidReason: "已被事实证伪"}
	if err := common.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ThesisService{}
	after, err := svc.SetStatus(993, card.ID, model.ThesisStatusArchived, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.InvalidReason != card.InvalidReason {
		t.Fatalf("归档不能抹除失效依据：%+v", after)
	}
	if _, err := svc.SetStatus(993, card.ID, model.ThesisStatusInvalidated, strings.Repeat("字", 256)); err == nil {
		t.Error("失效原因超出字段容量应拒绝")
	}
}
