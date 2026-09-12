package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestCalendarReadFailureDoesNotSettleReturns(t *testing.T) {
	for _, kind := range []string{"plan", "selection"} {
		t.Run(kind, func(t *testing.T) {
			setupSelectionEvalTestDB(t)
			fixture := seedSelectionEvalBatch(t, 2051, model.RecTypeShortTerm, model.RecStatusSuccess, true,
				time.Date(2025, 1, 2, 15, 30, 0, 0, time.Local),
				[]selectionEvalCandidateFixture{{Symbol: "600901", Rank: 1, Order: 1, Picked: true}})
			dates := []string{"2025-01-02", "2025-01-03", "2025-01-06", "2025-01-07", "2025-01-08", "2025-01-09", "2025-01-10", "2025-01-13"}
			for i, date := range dates {
				mustCreateSelectionEvalFixture(t, &model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true})
				if i == 1 { // 真正的计划买入日停牌，不能改在复牌日入场。
					continue
				}
				mustCreateSelectionEvalFixture(t, &model.DailyBar{Symbol: "600901", Market: "cn", TradeDate: date,
					Open: 10, High: 10.1, Low: 9.9, Close: 10, Source: "test"})
			}
			label := model.RecommendationLabel{RecommendationID: fixture.Picks[0].ID, BatchID: fixture.Batch.ID,
				Symbol: "600901", Market: "cn", HorizonDays: 5, EntryMode: model.EntryModeNextOpen,
				SignalDate: "2025-01-02", MaturityStatus: model.LabelPending, LabelVersion: labelVersion}
			outcome := model.RecommendationSelectionOutcome{BatchID: fixture.Batch.ID, Symbol: "600901", HorizonDays: 5,
				OutcomeVersion: model.SelectionOutcomeVersion, SchemaVersion: model.SelectionOutcomeSchemaVersion,
				RankingVersion: candidateRankingVersion, MaturityStatus: model.LabelPending}
			if kind == "plan" {
				mustCreateSelectionEvalFixture(t, &label)
			} else {
				mustCreateSelectionEvalFixture(t, &outcome)
			}
			fault := errors.New("临时交易日历查询故障")
			const callback = "review_calendar_execution_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "trading_calendars" {
					tx.AddError(fault)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			advance := func() error {
				if kind == "plan" {
					_, err := AdvanceRecommendationLabels(context.Background(), nil)
					return err
				}
				_, err := RunSelectionEval(context.Background(), nil)
				return err
			}
			if err := advance(); !errors.Is(err, fault) {
				t.Errorf("交易日历故障必须保留为错误，不能换用个股日线继续结算：%v", err)
			}
			status := func() string {
				if kind == "plan" {
					if err := common.DB.First(&label, label.ID).Error; err != nil {
						t.Fatal(err)
					}
					return label.MaturityStatus
				}
				if err := common.DB.First(&outcome, outcome.ID).Error; err != nil {
					t.Fatal(err)
				}
				return outcome.MaturityStatus
			}
			if got := status(); got != model.LabelPending {
				t.Errorf("读取故障不得写入终态：status=%s", got)
			}
			_ = common.DB.Callback().Query().Remove(callback)
			if err := advance(); err != nil {
				t.Fatal(err)
			}
			if got := status(); got != model.LabelSkipped {
				t.Errorf("日历恢复后应按买入日停牌跳过，不能保留错误收益：status=%s", got)
			}
		})
	}
}
