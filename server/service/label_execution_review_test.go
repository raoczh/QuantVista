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

func TestTruncatedMarketAxisDoesNotMoveHistoricalExecution(t *testing.T) {
	setupSelectionEvalTestDB(t)
	fixture := seedSelectionEvalBatch(t, 2052, model.RecTypeShortTerm, model.RecStatusSuccess, true,
		time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local),
		[]selectionEvalCandidateFixture{{Symbol: "600901", Rank: 1, Order: 1, Picked: true}})
	var dates []string
	for d := fixture.Batch.CreatedAt; len(dates) < wideBarLimit+20; d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			dates = append(dates, d.Format("2006-01-02"))
		}
	}
	var bars []datasource.Bar
	for _, date := range dates {
		mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true})
		mustCreateSelectionEvalFixture(t, &model.DailyBar{Symbol: "600901", Market: "cn", TradeDate: date,
			Open: 10, High: 10.1, Low: 9.9, Close: 10, Source: "test"})
		bars = append(bars, datasource.Bar{TradeDate: date, Open: 10, High: 10.1, Low: 9.9, Close: 10, Source: "test"})
	}
	// 基准/日历窗口已经从信号之后开始；保留日线因历史事实引用而更长。
	axis := dates[len(dates)-wideBarLimit:]
	next, sell := labelAxisDates(axis, dates[0], 5)
	out := simulateLabelHold(bars, 0, "600901", "甲", 5, labelPerCap, 0, 0, next, sell, dates[len(dates)-1])
	if out.Status != btTraded || out.BuyDate != dates[1] || out.SellDate != dates[6] || out.Forced {
		t.Errorf("左侧未覆盖不能把窗口起点当作旧信号次日，应沿用无轴兼容口径：%+v", out)
	}
	actual := settleFromActualEntry(bars, dates[1], 10, 5, actualSellDate(axis, dates[1], 5), dates[len(dates)-1])
	if actual.Status != btTraded || actual.SellDate != dates[6] {
		t.Errorf("实际建仓的到期日也不能被移动到日轴起点之后：%+v", actual)
	}
	selection := computeSelectionOutcome(fixture.Batch, fixture.Events["600901"], 5, bars, axis, nil,
		dates[len(dates)-1], dates[len(dates)-1])
	if selection.MaturityStatus != model.LabelMatured || selection.ExitDate != dates[6] {
		t.Errorf("选择评估不能永久保存错误停牌事实：%+v", selection)
	}
	result, err := NewBacktestService(nil).BatchBacktest(context.Background(), fixture.Batch.UserID,
		BatchBacktestRequest{BatchID: fixture.Batch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0].Holds["5"].Status != btTraded {
		t.Errorf("旧批次的日线完整时应正常回验：%+v", result.Rows)
	}
	// 左边界恰好命中和右侧尚未成熟仍必须使用现有市场轴。
	if n, s := labelAxisDates(axis, axis[0], 5); n != axis[1] || s != axis[6] {
		t.Errorf("日轴左边界不应误用回退：%s %s", n, s)
	}
	if _, s := labelAxisDates(axis, axis[len(axis)-1], 5); s != labelFarFuture {
		t.Errorf("未到期仍应保留未来哨兵：%s", s)
	}
}

func TestLabelRequiredInputFailurePreservesPending(t *testing.T) {
	for _, kind := range []string{"barriers", "invalid_plan", "stock_names"} {
		t.Run(kind, func(t *testing.T) {
			setupSelectionEvalTestDB(t)
			fixture := seedSelectionEvalBatch(t, 2053, model.RecTypeShortTerm, model.RecStatusSuccess, true,
				time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local),
				[]selectionEvalCandidateFixture{{Symbol: "600902", Rank: 1, Order: 1, Picked: true}})
			const plan = `{"take_profit":10.7,"stop_loss":9.4}`
			if err := common.DB.Model(&fixture.Picks[0]).Update("detail_json", plan).Error; err != nil {
				t.Fatal(err)
			}
			name := "甲"
			if kind == "stock_names" {
				name = "ST甲"
			}
			mustCreateSelectionEvalFixture(t, &model.MarketSyncState{Symbol: "600902", Market: "cn", Name: name})
			dates := []string{"2025-01-02", "2025-01-03", "2025-01-06", "2025-01-07", "2025-01-08", "2025-01-09", "2025-01-10"}
			for i, date := range dates {
				price := 10.0
				if i > 0 {
					price = 10.5
				}
				mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true})
				mustCreateSelectionEvalFixture(t, &model.DailyBar{Symbol: "600902", Market: "cn", TradeDate: date,
					Open: price, High: price + 0.3, Low: price - 0.1, Close: price, Source: "test"})
			}
			label := model.RecommendationLabel{RecommendationID: fixture.Picks[0].ID, BatchID: fixture.Batch.ID,
				Symbol: "600902", Market: "cn", HorizonDays: 5, EntryMode: model.EntryModeNextOpen,
				SignalDate: dates[0], MaturityStatus: model.LabelPending, LabelVersion: labelVersion}
			mustCreateSelectionEvalFixture(t, &label)
			fault := errors.New("临时标签输入查询故障")
			const callback = "review_label_required_input"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if (kind == "barriers" && tx.Statement.Table == "recommendations") ||
					(kind == "stock_names" && tx.Statement.Table == "market_sync_states") {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			if kind == "invalid_plan" {
				if err := common.DB.Model(&fixture.Picks[0]).Update("detail_json", "{").Error; err != nil {
					t.Fatal(err)
				}
			}
			_, err := AdvanceRecommendationLabels(context.Background(), nil)
			if (kind == "invalid_plan" && err == nil) || (kind != "invalid_plan" && !errors.Is(err, fault)) {
				t.Errorf("必需输入故障必须传回，不能按零障碍或非 ST 结算：%v", err)
			}
			if err := common.DB.First(&label, label.ID).Error; err != nil {
				t.Fatal(err)
			}
			if label.MaturityStatus != model.LabelPending {
				t.Errorf("故障期间必须保留 pending，得到 %s", label.MaturityStatus)
			}
			_ = common.DB.Callback().Query().Remove(callback)
			if err := common.DB.Model(&fixture.Picks[0]).Update("detail_json", plan).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := AdvanceRecommendationLabels(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if err := common.DB.First(&label, label.ID).Error; err != nil {
				t.Fatal(err)
			}
			if kind == "stock_names" {
				if label.MaturityStatus != model.LabelSkipped || label.SkipReason != btSkipLimitUp {
					t.Errorf("恢复后应正确识别 ST 次日涨停不可买：%+v", label)
				}
			} else if label.MaturityStatus != model.LabelMatured || !label.HitTakeProfit || label.ExitPrice != 10.7 {
				t.Errorf("恢复后应按真实止盈计划结算：%+v", label)
			}
		})
	}
}

func TestLabelBackfillFailureIsReported(t *testing.T) {
	for _, kind := range []string{"position_read", "label_insert"} {
		t.Run(kind, func(t *testing.T) {
			setupSelectionEvalTestDB(t)
			fixture := seedSelectionEvalBatch(t, 2054, model.RecTypeShortTerm, model.RecStatusSuccess, true,
				time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local),
				[]selectionEvalCandidateFixture{{Symbol: "600903", Rank: 1, Order: 1, Picked: true}})
			mustCreateSelectionEvalFixture(t, &model.RecommendationLabel{RecommendationID: fixture.Picks[0].ID,
				HorizonDays: model.LabelHorizons[0], EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelMatured})
			position := model.Position{UserID: fixture.Batch.UserID, RecommendationID: fixture.Picks[0].ID,
				Symbol: "600903", Market: "cn", BuyPrice: 10, BuyDate: "2025-01-03", Quantity: 100}
			mustCreateSelectionEvalFixture(t, &position)
			t.Cleanup(func() { common.DB.Delete(&position) })
			fault := errors.New("临时实际标签补建故障")
			const callback = "review_actual_label_backfill"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if kind == "position_read" && tx.Statement.Table == "positions" {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if kind == "label_insert" && tx.Statement.Table == "recommendation_labels" {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = common.DB.Callback().Query().Remove(callback)
				_ = common.DB.Callback().Create().Remove(callback)
			})
			if _, err := AdvanceRecommendationLabels(context.Background(), nil); !errors.Is(err, fault) {
				t.Errorf("补建未完成不能报告任务成功：%v", err)
			}
		})
	}
}

func TestStaleLabelForcedCloseUsesAffordableQuantity(t *testing.T) {
	bars := []datasource.Bar{
		{TradeDate: "2025-01-02", Open: 100, High: 100, Low: 100, Close: 100},
		{TradeDate: "2025-01-03", Open: 100, High: 100, Low: 100, Close: 100},
		{TradeDate: "2025-01-06", Open: 100, High: 100, Low: 100, Close: 100},
	}
	out := simulateLabelHold(bars, 0, "600904", "甲", 60, labelPerCap, 0, 0, "", "", "")
	if out.Status != btPending || out.BuyDate == "" {
		t.Fatalf("夹具应已按预算入场并等待到期：%+v", out)
	}
	label := model.RecommendationLabel{Symbol: "600904", EntryMode: model.EntryModeNextOpen}
	if !forceCloseStaleLabel(&label, &out, bars) {
		t.Fatal("超窗后应能按同一入场事实强平")
	}
	// 20000 元买不起 200 股及 5 元佣金，只能持有 100 股：两边佣金各 5，印花税 5。
	if out.NetPct != -0.15 || !out.Forced {
		t.Errorf("不能在强平时将 100 股变成超预算的 200 股并少算费率：%+v", out)
	}
}
