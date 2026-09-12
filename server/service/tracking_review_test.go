package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestTrackingRefreshReportsStorageFailures(t *testing.T) {
	for _, table := range []string{"recommendation_batches", "recommendations", "recommendation_statuses", "positions"} {
		t.Run(table, func(t *testing.T) {
			setupTestDB(t)
			batch, _ := seedLinkFixture(t, 8891, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
			failure := errors.New("本地追踪读取故障")
			const callback = "review_tracking_read_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{})))
			if _, err := svc.RefreshBatch(t.Context(), batch.UserID, batch.ID); !errors.Is(err, failure) {
				t.Errorf("追踪读取失败被报告为成功或不存在: table=%s err=%v", table, err)
			}
		})
	}
}

func TestTrackingRefreshHonorsCanceledRequest(t *testing.T) {
	setupTestDB(t)
	batch, _ := seedLinkFixture(t, 8892, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{})))
	if _, err := svc.RefreshBatch(ctx, batch.UserID, batch.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("取消的追踪请求未停止: %v", err)
	}
	var count int64
	if err := common.DB.Model(&model.RecommendationStatus{}).Where("batch_id = ?", batch.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("取消后仍写入了 %d 条追踪状态", count)
	}
}

func TestTrackingLateCommitPreservesTerminal(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8893, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	stored := model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
		Symbol: rec.Symbol, Market: rec.Market, Outcome: model.RecOutcomeStopLoss, ReturnPct: -10,
		CurrentPrice: 9, LastEvalDate: "2026-09-09", ReviewAck: true}
	if err := common.DB.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	late := stored
	late.ID, late.Outcome, late.ReturnPct, late.CurrentPrice = 0, model.RecOutcomeActive, 20, 12
	if err := (&TrackingService{}).upsertStatus(&late); err != nil {
		t.Fatal(err)
	}
	var after model.RecommendationStatus
	if err := common.DB.First(&after, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Outcome != stored.Outcome || after.ReturnPct != -10 || !after.ReviewAck {
		t.Errorf("迟到评估覆盖已冻结结局: %+v", after)
	}
}

func TestTrackingCommitDoesNotReviveDeletedBatch(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8894, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	if err := (&RecommendationService{}).Delete(batch.UserID, batch.ID); err != nil {
		t.Fatal(err)
	}
	late := model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
		Symbol: rec.Symbol, Market: rec.Market, Outcome: model.RecOutcomeActive, ReturnPct: 20}
	if err := (&TrackingService{}).upsertStatus(&late); err == nil {
		t.Error("已删除推荐仍接受追踪状态")
	}
	var count int64
	if err := common.DB.Model(&model.RecommendationStatus{}).Where("batch_id = ?", batch.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("已删除批次重新出现 %d 条追踪状态", count)
	}
}

func TestTrackingOlderObservationDoesNotReplaceNewer(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8895, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	stored := model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
		Symbol: rec.Symbol, Market: rec.Market, Outcome: model.RecOutcomeActive, ReturnPct: 20,
		CurrentPrice: 12, LastEvalDate: "2026-09-09", UpdatedAt: time.Now()}
	if err := common.DB.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	late := stored
	late.ID, late.LastEvalDate, late.ReturnPct, late.CurrentPrice = 0, "2026-09-08", 10, 11
	if err := (&TrackingService{}).upsertStatus(&late); err != nil {
		t.Fatal(err)
	}
	var after model.RecommendationStatus
	if err := common.DB.First(&after, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.LastEvalDate != stored.LastEvalDate || after.ReturnPct != 20 {
		t.Errorf("旧日行情覆盖了新追踪结果: %+v", after)
	}
}

func TestTrackingPerformanceRequiresBatchClassification(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8896, "600000", model.RecTypeShortTerm, model.RecStatusDegraded)
	if err := common.DB.Create(&model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: rec.UserID,
		Outcome: model.RecOutcomeTakeProfit, Action: model.RecActionBuy, ReturnPct: 50}).Error; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("本地批次分类读取故障")
	const callback = "review_tracking_performance_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_batches" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	if result, err := (&TrackingService{}).Performance(rec.UserID, ""); !errors.Is(err, failure) || result != nil {
		t.Errorf("批次分类读取失败后将降级结果混入正式胜率: result=%+v err=%v", result, err)
	}
}

func TestTrackingSettlementStopsAtFirstTerminalDate(t *testing.T) {
	first := []datasource.Bar{bar("2026-09-01", 10, 10.2, 9.8, 10), bar("2026-09-02", 10, 12.2, 9.9, 12)}
	in := trackInput{RefPrice: 10, TakeProfit: 12, StopLoss: 9, ValidDays: 5, IsShort: true, ElapsedTradeDays: 2, Bars: first}
	want := evaluateTracking(in)
	in.Bars = append(append([]datasource.Bar{}, first...), bar("2026-09-03", 12, 20, 5, 6))
	in.ElapsedTradeDays = 3
	got := evaluateTracking(in)
	if got.LastDate != want.LastDate || got.ReturnPct != want.ReturnPct || got.PeriodHigh != want.PeriodHigh || got.PeriodLow != want.PeriodLow {
		t.Errorf("延迟第一次刷新使终态收益吸收后续行情: first=%+v late=%+v", want, got)
	}
}

type trackingReviewAdapter struct {
	recPreheatMarketAdapter
	bars []datasource.Bar
}

func (a *trackingReviewAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	return append([]datasource.Bar{}, a.bars...), nil
}

func (a *trackingReviewAdapter) GetQuote(context.Context, string, string) (*datasource.Quote, error) {
	return nil, datasource.ErrNoData
}

func seedTrackingReviewCalendar(t *testing.T, from string) {
	t.Helper()
	start, err := time.ParseInLocation("2006-01-02", from, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	var rows []model.TradingCalendar
	for day := start; !day.After(time.Now()); day = day.AddDate(0, 0, 1) {
		rows = append(rows, model.TradingCalendar{Market: "cn", TradeDate: day.Format("2006-01-02"), IsOpen: day.Weekday() >= time.Monday && day.Weekday() <= time.Friday})
	}
	if err := common.DB.CreateInBatches(rows, 200).Error; err != nil {
		t.Fatal(err)
	}
}

func TestTrackingCalendarFailurePreventsSettlement(t *testing.T) {
	setupTestDB(t)
	seedTrackingReviewCalendar(t, "2026-09-01")
	batch, rec := seedLinkFixture(t, 8897, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	rec.DetailJSON = `{"valid_days":2}`
	failure := errors.New("本地追踪日历故障")
	const callback = "review_tracking_calendar_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "trading_calendars" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&trackingReviewAdapter{bars: []datasource.Bar{bar("2026-09-02", 10, 10.2, 9.8, 10)}})))
	if result, err := svc.evaluateOne(t.Context(), batch, rec, "2026-09-01", nil, nil); !errors.Is(err, failure) || result != nil {
		t.Errorf("日历读取失败仍生成可落库状态: result=%+v err=%v", result, err)
	}
}

func TestTrackingSuspensionDoesNotExtendValidity(t *testing.T) {
	setupTestDB(t)
	seedTrackingReviewCalendar(t, "2026-09-01")
	batch, rec := seedLinkFixture(t, 8898, "600000", model.RecTypeShortTerm, model.RecStatusSuccess)
	rec.DetailJSON = `{"valid_days":2,"take_profit":12,"stop_loss":9}`
	bars := []datasource.Bar{bar("2026-09-03", 10, 10.3, 9.8, 10.2), bar("2026-09-04", 10.2, 12.5, 10, 12.2)}
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&trackingReviewAdapter{bars: bars})))
	result, err := svc.evaluateOne(t.Context(), batch, rec, "2026-09-01", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != model.RecOutcomeExpired || result.HitTakeProfit || result.LastEvalDate != "2026-09-03" {
		t.Errorf("首日停牌把有效期从两个市场交易日延长为两根个股日线: %+v", result)
	}
}

func TestTrackingNodeReturnUsesMarketDate(t *testing.T) {
	setupTestDB(t)
	seedTrackingReviewCalendar(t, "2026-09-01")
	batch, rec := seedLinkFixture(t, 8899, "600000", model.RecTypeLongTerm, model.RecStatusSuccess)
	var bars []datasource.Bar
	for i, date := range []string{"2026-09-03", "2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11"} {
		price := float64(11 + i)
		bars = append(bars, bar(date, price, price, price, price))
	}
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&trackingReviewAdapter{bars: bars})))
	result, err := svc.evaluateOne(t.Context(), batch, rec, "2026-09-01", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Return7d == nil || *result.Return7d != 60 {
		t.Errorf("第七个市场交易日应为9月10日，不能顺延到第七根日线: %+v", result)
	}
}

func TestTrackingMatureDrawdownUsesSameSample(t *testing.T) {
	setupTestDB(t)
	rows := []model.RecommendationStatus{
		{RecommendationID: 1, UserID: 8900, Action: model.RecActionBuy, Outcome: model.RecOutcomeExpired, MaxDrawdownPct: 10},
		{RecommendationID: 2, UserID: 8900, Action: model.RecActionBuy, Outcome: model.RecOutcomeActive, MaxDrawdownPct: 90},
		{RecommendationID: 3, UserID: 8900, Action: model.RecActionWatch, Outcome: model.RecOutcomeExpired, MaxDrawdownPct: 50},
	}
	if err := common.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := (&TrackingService{}).Performance(8900, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["buy_avg_max_drawdown_pct"] != float64(10) {
		t.Errorf("成熟买入摘要缺少同一分母的回撤，全部样本的回撤为50%%: %s", raw)
	}
}
