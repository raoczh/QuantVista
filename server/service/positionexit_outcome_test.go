package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// 卖出评估前向结果台账：成熟判定、除权同源口径锚定、幂等与聚合纪律。
func TestPositionExitOutcomeBackfillMaturityIdempotencyAndReport(t *testing.T) {
	setupTestDB(t)
	cleanTables := func() {
		for _, table := range []string{"position_exit_outcomes", "position_exit_assessments", "daily_bars"} {
			if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
				t.Fatalf("清理 %s 失败: %v", table, err)
			}
		}
	}
	cleanTables()
	// 共享内存库：残留的评估行会触发 Todo 抑制逻辑，干扰后续测试，结束时清场。
	t.Cleanup(cleanTables)
	now := time.Now()
	// 12 个连续交易日：T=第 1 根（2044-06-01），其后 11 根，5/10 日窗口均可成熟。
	dates := []string{"2044-06-01", "2044-06-02", "2044-06-03", "2044-06-06", "2044-06-07",
		"2044-06-08", "2044-06-09", "2044-06-10", "2044-06-13", "2044-06-14", "2044-06-15", "2044-06-16"}
	for i, d := range dates {
		price := 10.0 + float64(i)*0.1
		if err := common.DB.Create(&model.DailyBar{Symbol: "600900", Market: "cn", TradeDate: d,
			Open: price, High: price + 0.5, Low: price - 0.5, Close: price, Volume: 1e6, Amount: 5e7}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mature := model.PositionExitAssessment{UserID: 1, PositionID: 11, Symbol: "600900", Market: "cn",
		TradeDate: dates[0], Session: model.PositionExitSessionClose, EvaluatedAt: now,
		Level: model.PositionExitLevelReview, PrimarySignal: "atr14_break", ParamsHash: "ph1",
		DataStatus: model.PositionExitDataReady, EventKey: "peo-mature", Version: model.PositionExitAssessmentVersion}
	// 只有 4 根后续 K 线：任何窗口都不成熟，必须留到下一轮而不是写坏数据。
	young := model.PositionExitAssessment{UserID: 1, PositionID: 12, Symbol: "600900", Market: "cn",
		TradeDate: dates[7], Session: model.PositionExitSessionClose, EvaluatedAt: now,
		Level: model.PositionExitLevelNormal, PrimarySignal: "normal", ParamsHash: "ph1",
		DataStatus: model.PositionExitDataReady, EventKey: "peo-young", Version: model.PositionExitAssessmentVersion}
	intraday := model.PositionExitAssessment{UserID: 1, PositionID: 13, Symbol: "600900", Market: "cn",
		TradeDate: dates[0], Session: model.PositionExitSessionIntraday, EvaluatedAt: now,
		Level: model.PositionExitLevelUrgent, PrimarySignal: "plan_stop", ParamsHash: "ph1",
		DataStatus: model.PositionExitDataReady, EventKey: "peo-intraday", Version: model.PositionExitAssessmentVersion}
	for _, row := range []*model.PositionExitAssessment{&mature, &young, &intraday} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	created, err := BackfillPositionExitOutcomes(context.Background())
	if err != nil || created != 2 {
		t.Fatalf("成熟评估应回填 5/10 两条，盘中槽位与未成熟行不回填: created=%d err=%v", created, err)
	}
	var outcomes []model.PositionExitOutcome
	if err := common.DB.Order("horizon ASC").Find(&outcomes).Error; err != nil || len(outcomes) != 2 {
		t.Fatalf("台账行数: %d err=%v", len(outcomes), err)
	}
	h5 := outcomes[0]
	if h5.AssessmentID != mature.ID || h5.Horizon != 5 || h5.BasePrice != 10 {
		t.Fatalf("锚定与窗口错误: %+v", h5)
	}
	// T close=10，T+5 close=10.5 → +5%；窗口内最低 10.1-0.5=9.6 → -4%。
	if h5.ForwardReturnPct != 5 || h5.MaePct != -4 {
		t.Fatalf("前向收益/MAE 计算错误: fwd=%v mae=%v", h5.ForwardReturnPct, h5.MaePct)
	}
	if again, err := BackfillPositionExitOutcomes(context.Background()); err != nil || again != 0 {
		t.Fatalf("重复回填必须幂等零新增: created=%d err=%v", again, err)
	}

	rep, err := PositionExitOutcomeReport()
	if err != nil || rep.Total != 2 {
		t.Fatalf("聚合报表: total=%d err=%v", rep.Total, err)
	}
	if len(rep.Levels) == 0 || rep.Levels[0].Evaluated {
		t.Fatalf("样本不足必须显式标未评估: %+v", rep.Levels)
	}
	foundSignal := false
	for _, b := range rep.Signals {
		if b.PrimarySignal == "atr14_break" && b.Horizon == 5 {
			foundSignal = true
			if b.Samples != 1 || b.AvgForwardPct != 5 {
				t.Fatalf("signal 层聚合错误: %+v", b)
			}
		}
	}
	if !foundSignal {
		t.Fatal("signal 层缺少 atr14_break 分组")
	}
}

func TestPositionExitOutcomeBackfillSkipsImmatureBatchAndUsesSuspensionAnchor(t *testing.T) {
	setupTestDB(t)
	for _, table := range []string{"position_exit_outcomes", "position_exit_assessments", "daily_bars"} {
		if err := common.DB.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"position_exit_outcomes", "position_exit_assessments", "daily_bars"} {
			_ = common.DB.Exec("DELETE FROM " + table).Error
		}
	})

	blocked := make([]model.PositionExitAssessment, exitOutcomeBatchLimit)
	for i := range blocked {
		blocked[i] = model.PositionExitAssessment{
			UserID: 2, PositionID: int64(2000 + i), Symbol: "NO_BARS", Market: "cn",
			TradeDate: "2044-01-01", Session: model.PositionExitSessionClose, EvaluatedAt: time.Now(),
			Level: model.PositionExitLevelNormal, PrimarySignal: "normal", ParamsHash: "ph1",
			DataStatus: model.PositionExitDataReady, EventKey: "blocked-" + time.Unix(int64(i), 0).Format("150405.000000000"), Version: model.PositionExitAssessmentVersion,
		}
	}
	if err := common.DB.CreateInBatches(&blocked, 100).Error; err != nil {
		t.Fatal(err)
	}
	dates := []string{"2044-05-31", "2044-06-03", "2044-06-04", "2044-06-05", "2044-06-06", "2044-06-07", "2044-06-10", "2044-06-11", "2044-06-12", "2044-06-13", "2044-06-14"}
	for i, date := range dates {
		price := 20 + float64(i)
		if err := common.DB.Create(&model.DailyBar{Symbol: "600901", Market: "cn", TradeDate: date, Open: price, High: price, Low: price, Close: price}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mature := model.PositionExitAssessment{
		UserID: 2, PositionID: 9999, Symbol: "600901", Market: "cn",
		TradeDate: "2044-06-01", Session: model.PositionExitSessionClose, EvaluatedAt: time.Now(),
		Level: model.PositionExitLevelReview, PrimarySignal: "ma20_break", ParamsHash: "ph2",
		DataStatus: model.PositionExitDataReady, EventKey: "mature-after-blocked", Version: model.PositionExitAssessmentVersion,
	}
	if err := common.DB.Create(&mature).Error; err != nil {
		t.Fatal(err)
	}
	created, err := BackfillPositionExitOutcomes(context.Background())
	if err != nil || created != 2 {
		t.Fatalf("未成熟批次不得阻塞成熟记录: created=%d err=%v", created, err)
	}
	var h5 model.PositionExitOutcome
	if err := common.DB.Where("assessment_id = ? AND horizon = ?", mature.ID, 5).First(&h5).Error; err != nil {
		t.Fatal(err)
	}
	if h5.BasePrice != 20 || h5.ForwardReturnPct != 25 {
		t.Fatalf("停牌评估应使用 T 日前最近收盘为同源锚点: %+v", h5)
	}
}

func TestPositionExitOutcomeReportSeparatesParameterHashes(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM position_exit_outcomes").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Exec("DELETE FROM position_exit_outcomes").Error })
	rows := []model.PositionExitOutcome{
		{AssessmentID: 10001, Horizon: 5, UserID: 1, PositionID: 1, Symbol: "600001", Market: "cn", TradeDate: "2044-01-01", Level: model.PositionExitLevelReview, PrimarySignal: "ma20_break", ParamsHash: "ph1", ForwardReturnPct: -2},
		{AssessmentID: 10002, Horizon: 5, UserID: 1, PositionID: 2, Symbol: "600002", Market: "cn", TradeDate: "2044-01-01", Level: model.PositionExitLevelReview, PrimarySignal: "ma20_break", ParamsHash: "ph2", ForwardReturnPct: 4},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	rep, err := PositionExitOutcomeReport()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]float64{}
	for _, bucket := range rep.Signals {
		seen[bucket.ParamsHash] = bucket.AvgForwardPct
	}
	if seen["ph1"] != -2 || seen["ph2"] != 4 {
		t.Fatalf("不同参数版本不得混合聚合: %+v", rep.Signals)
	}
}
