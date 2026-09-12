package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestHistoricalAnalysisKeepsAdjustedPricePrecision(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)
	id := seedCalibAnalysis(t, "600903", model.AnalysisRatingNeutral, 80, "high", "")
	for _, date := range []string{"2026-05-29", "2026-06-01"} {
		if err := common.DB.Create(&model.DailyBar{Symbol: "600903", Market: "cn", TradeDate: date,
			Open: 4.0375, High: 4.0375, Low: 4.0375, Close: 4.0375, Source: "test"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, snapshot, err := buildStockSnapshotAsOf(t.Context(), "600903", "cn", "2026-06-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"price", "open", "high", "low", "prev_close"} {
		if snapshot["quote"].(map[string]any)[key] != 4.0375 {
			t.Errorf("历史前复权 %s 应与实际计算收益的四位基准一致：%v", key, snapshot["quote"].(map[string]any)[key])
		}
	}
	view, err := (&AnalysisService{}).Hindsight(t.Context(), 1, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if view.BasePrice != 4.0375 {
		t.Fatalf("事后核验交付的基准价应与实际计算收益一致：%v", view.BasePrice)
	}
}

func TestHindsightReviewRatingUsesActualPriceDirection(t *testing.T) {
	for _, rating := range []string{model.AnalysisRatingBullish, model.AnalysisRatingBearish} {
		t.Run(rating, func(t *testing.T) {
			setupTestDB(t)
			cleanCalibTables(t)
			id := seedCalibAnalysis(t, "600901", rating, 80, "high", "")
			base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local)
			change := 0.01
			if rating == model.AnalysisRatingBearish {
				change = -change
			}
			for i := 0; i <= 20; i++ {
				price := 1000.0
				if i == 20 {
					price += change
				}
				if err := common.DB.Create(&model.DailyBar{Symbol: "600901", Market: "cn", TradeDate: base.AddDate(0, 0, i).Format("2006-01-02"),
					Open: price, High: price, Low: price, Close: price, Source: "test"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			view, err := (&AnalysisService{}).Hindsight(context.Background(), 1, id, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if view.Returns["d20"] == nil || view.Returns["d20"].ReturnPct != 0 || view.RatingHit == nil || !*view.RatingHit {
				t.Fatalf("0.001%% 的真实方向命中不能因展示为 0.00%% 变成未命中：%+v", view)
			}
		})
	}
}

func TestCalibrationReviewDeduplicatesSameTradingBaseline(t *testing.T) {
	setupTestDB(t)
	cleanCalibTables(t)
	base := time.Date(2026, 6, 5, 0, 0, 0, 0, time.Local) // 周五。
	day := base
	for i := 0; i <= 20; i++ {
		if i > 0 {
			for {
				day = day.AddDate(0, 0, 1)
				if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
					break
				}
			}
		}
		price := 10 + float64(i)/10
		if err := common.DB.Create(&model.DailyBar{Symbol: "600902", Market: "cn", TradeDate: day.Format("2006-01-02"),
			Open: price, High: price, Low: price, Close: price, Source: "test"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, day := range []int{6, 7} { // 周六/周日分析都基于同一个周五收盘。
		id := seedCalibAnalysis(t, "600902", model.AnalysisRatingBullish, 80, "high", "")
		if err := common.DB.Model(&model.AnalysisRecord{}).Where("id = ?", id).
			Update("created_at", time.Date(2026, 6, day, 10, 0, 0, 0, time.Local)).Error; err != nil {
			t.Fatal(err)
		}
	}
	view, err := buildAnalysisCalibReport(context.Background())
	if err != nil || view == nil || view.Judged != 1 || view.DupSkipped != 1 {
		t.Fatalf("同一实际交易基准日的周末分析不能反复增加校准样本：view=%+v err=%v", view, err)
	}
}
