package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestIntradayPartialSourceFailurePreservesStoredFacts(t *testing.T) {
	setupTestDB(t)
	cleanIntradayTables(t)
	for _, symbol := range []string{"600901", "600902"} {
		if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: symbol}).Error; err != nil {
			t.Fatal(err)
		}
	}
	prior := model.IntradayFactorDaily{Market: "cn", Symbol: "600902", TradeDate: "2026-09-09", Vwap: 18, BarCount: 48}
	if err := common.DB.Create(&prior).Error; err != nil {
		t.Fatal(err)
	}
	svc := &IntradayService{fetchMin5: func(_ context.Context, _, symbol string, _ int) ([]datasource.Min5Bar, error) {
		if symbol == "600902" {
			return nil, errors.New("temporary upstream timeout")
		}
		return fullDay48("20260909", func(string) float64 { return 10 }, func(string) int64 { return 100 }), nil
	}}
	n, err := svc.SyncIntradayFactors(t.Context(), []string{"2026-09-09"})
	if err == nil || n != 1 {
		t.Errorf("部分源故障应保留有效新增并明确报告缺口：n=%d err=%v", n, err)
	}
	var stored model.IntradayFactorDaily
	if err := common.DB.First(&stored, prior.ID).Error; err != nil || stored.Vwap != 18 {
		t.Fatalf("单股临时源故障不能删掉已有因子：row=%+v err=%v", stored, err)
	}
}

func TestIntradayCancellationAtCommitPreservesPriorFacts(t *testing.T) {
	setupTestDB(t)
	cleanIntradayTables(t)
	if err := common.DB.Create(&model.MarketSyncState{Market: "cn", Symbol: "600901"}).Error; err != nil {
		t.Fatal(err)
	}
	prior := model.IntradayFactorDaily{Market: "cn", Symbol: "600901", TradeDate: "2026-09-09", Vwap: 18, BarCount: 48}
	if err := common.DB.Create(&prior).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const callback = "review_intraday_cancel_write"
	if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "intraday_factor_dailies" {
			cancel()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
	svc := &IntradayService{fetchMin5: func(context.Context, string, string, int) ([]datasource.Min5Bar, error) {
		return fullDay48("20260909", func(string) float64 { return 10 }, func(string) int64 { return 100 }), nil
	}}
	if n, err := svc.SyncIntradayFactors(ctx, []string{"2026-09-09"}); n != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("提交前取消不能报告同步成功：n=%d err=%v", n, err)
	}
	var stored model.IntradayFactorDaily
	if err := common.DB.First(&stored, prior.ID).Error; err != nil || stored.Vwap != 18 {
		t.Fatalf("取消改变了旧事实：%+v %v", stored, err)
	}
}

func TestIntradayCompletenessCannotBeFilledByDuplicateBars(t *testing.T) {
	full := fullDay48("20260909", func(string) float64 { return 10 }, func(string) int64 { return 100 })
	for _, kind := range []string{"duplicate", "bad_ohlc", "negative_volume"} {
		bars := append([]datasource.Min5Bar(nil), full...)
		switch kind {
		case "duplicate":
			bars[2], bars[3], bars[4] = bars[1], bars[1], bars[1]
		case "bad_ohlc":
			bars[5].High = 9
		case "negative_volume":
			bars[6].Volume = -10
		}
		if got, ok := computeIntradayFactors(bars); ok {
			t.Errorf("%s 不能生成完整日因子：%+v", kind, got)
		}
	}
}

func TestIntradayFutureRowsDoNotHideCurrentSignals(t *testing.T) {
	setupTestDB(t)
	cleanIntradayTables(t)
	today := time.Now().Format("2006-01-02")
	pinCalendarTo(t, today)
	for _, date := range []string{today, time.Now().AddDate(0, 0, 1).Format("2006-01-02")} {
		if err := common.DB.Create(&model.IntradayFactorDaily{Market: "cn", Symbol: "600901", TradeDate: date, Vwap: 10, BarCount: 48}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if rows := intradaySignalsFor([]string{"600901"}); len(rows) != 1 || rows["600901"].TradeDate != today {
		t.Fatalf("未来因子不能挡住当期信号：%v", rows)
	}
	if got := IntradayAccumulatedDays(); got != 1 {
		t.Fatalf("未来日期不能计入已积累交易日：%d", got)
	}
}

func TestBoardValuationCancellationAndHistoryBoundary(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		setupTestDB(t)
		cleanBoardValTables(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := aggregateBoardValuation(ctx, &fakeBoardLister{boards: []datasource.BoardListItem{{Code: "BK1036", Name: "半导体"}}},
			[]datasource.SpotRow{{Symbol: "600901", Industry: "半导体", PETTM: 20}}, "2026-09-09")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("取消聚合必须停止：%v", err)
		}
		var count int64
		if err := common.DB.Model(&model.BoardValuationDaily{}).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("取消聚合仍写库：%d %v", count, err)
		}
	})
	t.Run("future_and_sample_count", func(t *testing.T) {
		setupTestDB(t)
		cleanBoardValTables(t)
		today := time.Now().Format("2006-01-02")
		for i, date := range []string{time.Now().AddDate(0, 0, -2).Format("2006-01-02"), time.Now().AddDate(0, 0, -1).Format("2006-01-02"), today, time.Now().AddDate(0, 0, 1).Format("2006-01-02")} {
			pe, count := 10+float64(i), 1
			if i == 1 {
				pe, count = 0, 0
			}
			if err := common.DB.Create(&model.BoardValuationDaily{Kind: "industry", BoardCode: "BK1036", BoardName: "半导体", TradeDate: date, MedianPETTM: pe, PosPECount: count}).Error; err != nil {
				t.Fatal(err)
			}
		}
		v := boardValuationFor("BK1036")
		if v == nil || v.TradeDate != today || v.HistDays != 2 {
			t.Fatalf("分位必须使用截至今天的有效 PE 分母：%+v", v)
		}
	})
}
