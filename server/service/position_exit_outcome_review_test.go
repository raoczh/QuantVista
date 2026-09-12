package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func seedExitOutcomeReview(t *testing.T, dates []string, base, forward float64) model.PositionExitAssessment {
	t.Helper()
	row := model.PositionExitAssessment{UserID: 12040, PositionID: 41, Symbol: "600901", Market: "cn",
		TradeDate: dates[0], Session: model.PositionExitSessionClose, Level: model.PositionExitLevelReview,
		PrimarySignal: "ma20_break", ParamsHash: "outcome-review", EventKey: "outcome-review", EvaluatedAt: time.Now()}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	for i, date := range dates {
		price := forward
		if i == 0 {
			price = base
		}
		bar := model.DailyBar{Symbol: row.Symbol, Market: row.Market, TradeDate: date,
			Open: price, High: price, Low: price, Close: price, Source: "eastmoney"}
		if err := common.DB.Create(&bar).Error; err != nil {
			t.Fatal(err)
		}
	}
	return row
}

func TestPositionExitOutcomeCompletedWindowOnly(t *testing.T) {
	for _, scenario := range []struct {
		name string
		now  time.Time
	}{
		{"future", time.Date(2026, 8, 10, 18, 0, 0, 0, time.Local)},
		{"intraday", time.Date(2026, 8, 11, 11, 0, 0, 0, time.Local)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			setupTestDB(t)
			seedExitOutcomeReview(t, []string{"2026-08-04", "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-10", "2026-08-11"}, 10, 9)
			if created, err := backfillPositionExitOutcomesAt(t.Context(), scenario.now); err != nil || created != 0 {
				t.Fatalf("未来或盘中日线不能让五日窗口提前成熟：created=%d err=%v", created, err)
			}
			if created, err := backfillPositionExitOutcomesAt(t.Context(), time.Date(2026, 8, 11, 18, 0, 0, 0, time.Local)); err != nil || created != 1 {
				t.Fatalf("收盘后的完整五日窗口应可回填：created=%d err=%v", created, err)
			}
		})
	}
}

func TestPositionExitOutcomeMissingExtremesRemainPending(t *testing.T) {
	setupTestDB(t)
	row := seedExitOutcomeReview(t, []string{"2026-08-04", "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-10", "2026-08-11"}, 10, 9)
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", row.Symbol, "2026-08-06").Update("low", 0).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 18, 0, 0, 0, time.Local)
	if created, err := backfillPositionExitOutcomesAt(t.Context(), now); err != nil || created != 0 {
		t.Fatalf("缺最低价不能用收盘冒充真实 MAE 并永久写入结果：created=%d err=%v", created, err)
	}
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", row.Symbol, "2026-08-06").Update("low", 7).Error; err != nil {
		t.Fatal(err)
	}
	if created, err := backfillPositionExitOutcomesAt(t.Context(), now); err != nil || created != 1 {
		t.Fatalf("完整价格补齐后应重新评估：created=%d err=%v", created, err)
	}
	var outcome model.PositionExitOutcome
	if err := common.DB.Where("assessment_id = ?", row.ID).First(&outcome).Error; err != nil || outcome.MaePct != -30 {
		t.Fatalf("补齐后应保留真实最大不利偏移：outcome=%+v err=%v", outcome, err)
	}
}

func checkPositionExitOutcomeSmallLoss(t *testing.T, base, forward float64) {
	t.Helper()
	seedExitOutcomeReview(t, []string{"2026-08-04", "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-10", "2026-08-11"}, base, forward)
	if created, err := backfillPositionExitOutcomesAt(context.Background(), time.Date(2026, 8, 12, 18, 0, 0, 0, time.Local)); err != nil || created != 1 {
		t.Fatalf("回填五日结果：created=%d err=%v", created, err)
	}
	report, err := PositionExitOutcomeReport()
	if err != nil || report.Total != 1 || len(report.Levels) != 1 {
		t.Fatalf("读取前向统计：report=%+v err=%v", report, err)
	}
	if math.Abs(report.Levels[0].DownRatioPct-100) > 1e-9 {
		t.Fatalf("价格从 %.4f 降至 %.4f，展示舍入不能把唯一负收益样本统计为零：%+v", base, forward, report.Levels[0])
	}
}

func TestPositionExitOutcomeSmallLossKeepsDirection(t *testing.T) {
	for _, prices := range [][2]float64{{10, 9.9999}, {3000, 2999.9999}} {
		t.Run(fmt.Sprintf("base_%g", prices[0]), func(t *testing.T) {
			setupTestDB(t)
			checkPositionExitOutcomeSmallLoss(t, prices[0], prices[1])
		})
	}
}

func TestMySQLPositionExitOutcomeSmallLossKeepsDirection(t *testing.T) {
	setupMySQLReviewDB(t, &model.PositionExitAssessment{}, &model.PositionExitOutcome{}, &model.DailyBar{}, &model.DailyBarWriteLock{})
	checkPositionExitOutcomeSmallLoss(t, 3000, 2999.9999)
}

func TestPositionExitOutcomeIncompleteBatchDoesNotStarveValidRows(t *testing.T) {
	setupTestDB(t)
	row := seedExitOutcomeReview(t, []string{"2026-08-04", "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-10", "2026-08-11"}, 10, 9)
	if err := common.DB.Model(&model.DailyBar{}).Where("symbol = ? AND trade_date = ?", row.Symbol, "2026-08-06").Update("low", 0).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < exitOutcomeBatchLimit; i++ {
		copy := row
		copy.ID, copy.EventKey = 0, fmt.Sprintf("incomplete-%d", i)
		if err := common.DB.Create(&copy).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 后一条评估的窗口从缺口后开始，不能被前一批 500 条不完整样本挡住。
	valid := row
	valid.ID, valid.EventKey, valid.TradeDate = 0, "complete-after-gap", "2026-08-06"
	if err := common.DB.Create(&valid).Error; err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{"2026-08-12", "2026-08-13"} {
		if err := common.DB.Create(&model.DailyBar{Market: "cn", Symbol: row.Symbol, TradeDate: date, Open: 9, Close: 9, Low: 9, High: 9}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if created, err := backfillPositionExitOutcomesAt(t.Context(), time.Date(2026, 8, 14, 18, 0, 0, 0, time.Local)); err != nil || created != 1 {
		t.Fatalf("不完整批次不能阻塞后续有效窗口：created=%d err=%v", created, err)
	}
	var outcome model.PositionExitOutcome
	if err := common.DB.First(&outcome).Error; err != nil || outcome.AssessmentID != valid.ID {
		t.Fatalf("只能保存缺口之后的完整窗口：outcome=%+v err=%v", outcome, err)
	}
}
