package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestLabelResumeRejectsNullPlan(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	rec := model.Recommendation{UserID: 8970, Symbol: "600001", Market: "cn", DetailJSON: "null"}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := labelBarriers(t.Context(), &model.RecommendationLabel{RecommendationID: rec.ID}); err == nil {
		t.Fatal("null 计划不能当作没有止盈止损")
	}
}

func TestTrackingResumeRejectsNullPlan(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	seedTrackingReviewCalendar(t, "2026-09-01")
	batch, rec := seedLinkFixture(t, 8971, "600001", model.RecTypeShortTerm, model.RecStatusSuccess)
	rec.DetailJSON = "null"
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&trackingReviewAdapter{bars: []datasource.Bar{bar("2026-09-02", 10, 10.2, 9.8, 10)}})))
	if state, err := svc.evaluateOne(t.Context(), batch, rec, "2026-09-01", nil, nil); err == nil || state != nil {
		t.Fatalf("损坏计划不能生成可持久化追踪结论: state=%+v err=%v", state, err)
	}
}

func TestTrackingResumeRejectsClosedDayBar(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	seedTrackingReviewCalendar(t, "2026-09-01")
	batch, rec := seedLinkFixture(t, 8972, "600001", model.RecTypeShortTerm, model.RecStatusSuccess)
	rec.DetailJSON = `{"valid_days":5,"take_profit":12,"stop_loss":9}`
	bars := []datasource.Bar{bar("2026-09-05", 10, 12.5, 9.8, 12), bar("2026-09-07", 10, 10.2, 9.8, 10)}
	svc := NewTrackingService(NewMarketService(datasource.NewManagerWithAdapters(&trackingReviewAdapter{bars: bars})))
	if state, err := svc.evaluateOne(t.Context(), batch, rec, "2026-09-04", nil, nil); err == nil || state != nil {
		t.Fatalf("周六假日线不能触发已止盈的永久结论: state=%+v err=%v", state, err)
	}
}

func TestLabelResumeMissingPriceAnchorPreservesPending(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	rec := model.Recommendation{UserID: 8973, Symbol: "600001", Market: "cn", DetailJSON: `{"take_profit":21,"stop_loss":18}`}
	if err := common.DB.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	label := model.RecommendationLabel{RecommendationID: rec.ID, Symbol: rec.Symbol, Market: "cn", SignalDate: "2026-09-01",
		HorizonDays: 1, EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelPending, RefDate: "2026-08-31", RefClose: 20}
	bars := []datasource.Bar{bar("2026-09-01", 10, 10.2, 9.8, 10), bar("2026-09-02", 10, 10.2, 9.8, 10), bar("2026-09-03", 10, 10.3, 9.9, 10.1)}
	if changed, err := advanceOneLabel(t.Context(), &label, bars, "本地股票", nil, nil, "2026-09-04"); err == nil || changed || label.MaturityStatus != model.LabelPending {
		t.Fatalf("缺少锚点不能拿旧计划价结算复权日线: %+v changed=%v err=%v", label, changed, err)
	}
}

func TestLabelResumeDoesNotUseFuturePrices(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	today := time.Now().In(time.Local).Format("2006-01-02")
	seedLabelBars(t, "600001", today, 3, 10)
	label := model.RecommendationLabel{CandidateEventID: 8974, Symbol: "600001", Market: "cn", SignalDate: today,
		HorizonDays: 1, EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelPending}
	if err := common.DB.Create(&label).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := AdvanceRecommendationLabels(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	var stored model.RecommendationLabel
	if err := common.DB.First(&stored, label.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.MaturityStatus != model.LabelPending || stored.ExitDate > today {
		t.Fatalf("未来日线让今天刚生成的标签提前成熟: %+v", stored)
	}
}

func TestLabelResumeLateWriteDoesNotReviveDeletedOrTerminal(t *testing.T) {
	for _, action := range []string{"delete", "settle"} {
		t.Run(action, func(t *testing.T) {
			setupTestDB(t)
			cleanLabelTables(t)
			date := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
			seedLabelBars(t, "600001", date, 3, 10)
			label := model.RecommendationLabel{CandidateEventID: 8975, Symbol: "600001", Market: "cn", SignalDate: date,
				HorizonDays: 1, EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelPending}
			if err := common.DB.Create(&label).Error; err != nil {
				t.Fatal(err)
			}
			const callback = "review_label_late_commit"
			ran := false
			if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "daily_bars" || ran {
					return
				}
				ran = true
				if action == "delete" {
					tx.AddError(common.DB.Delete(&label).Error)
				} else {
					tx.AddError(common.DB.Model(&label).Updates(map[string]any{"maturity_status": model.LabelMatured, "net_return_pct": 7.5}).Error)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			if _, err := AdvanceRecommendationLabels(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if !ran {
				t.Fatal("未进入结算交错夹具")
			}
			var rows []model.RecommendationLabel
			if err := common.DB.Where("id = ?", label.ID).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if action == "delete" && len(rows) != 0 {
				t.Errorf("迟到结算重新创建了已删除标签: %+v", rows)
			}
			if action == "settle" && (len(rows) != 1 || rows[0].NetReturnPct != 7.5) {
				t.Errorf("迟到结算覆盖了已成熟结果: %+v", rows)
			}
		})
	}
}

func TestLabelResumeActualEntryDoesNotRequireOldSignalBar(t *testing.T) {
	label := model.RecommendationLabel{Symbol: "600001", Market: "cn", SignalDate: "2025-01-01", EntryDate: "2026-09-01", ActualBuyPrice: 10,
		HorizonDays: 1, EntryMode: model.EntryModeActual, MaturityStatus: model.LabelPending}
	bars := []datasource.Bar{bar("2026-09-01", 10, 10.2, 9.8, 10), bar("2026-09-02", 10, 10.3, 9.9, 10.1)}
	if changed, err := advanceOneLabel(t.Context(), &label, bars, "本地股票", nil, nil, "2026-09-03"); err != nil || !changed || label.MaturityStatus != model.LabelMatured || label.ExitDate != "2026-09-02" {
		t.Fatalf("实际建仓行情完整时不能因原推荐过旧而无法结算: %+v changed=%v err=%v", label, changed, err)
	}
}

func TestLabelResumeActualPriceSplitIsUnknown(t *testing.T) {
	setupTestDB(t)
	cleanLabelTables(t)
	signal := time.Now().AddDate(0, 0, -10)
	seedLabelBars(t, "600001", signal.Format("2006-01-02"), 4, 10)
	entryDate := signal.AddDate(0, 0, 1).Format("2006-01-02")
	label := model.RecommendationLabel{CandidateEventID: 8976, PositionID: 1, Symbol: "600001", Market: "cn", SignalDate: signal.Format("2006-01-02"),
		EntryDate: entryDate, ActualBuyPrice: 20, HorizonDays: 1, EntryMode: model.EntryModeActual, MaturityStatus: model.LabelPending}
	if err := common.DB.Create(&label).Error; err != nil {
		t.Fatal(err)
	}
	other := model.RecommendationLabel{CandidateEventID: 8977, Symbol: label.Symbol, Market: "cn", SignalDate: label.SignalDate,
		HorizonDays: 1, EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelPending}
	if err := common.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	action := model.CorporateAction{Symbol: label.Symbol, Market: "cn", ExDate: signal.AddDate(0, 0, 2).Format("2006-01-02"), BonusRatio: 10,
		Progress: model.CorpActionProgressImplemented, PlanNoticeDate: label.SignalDate}
	if err := common.DB.Create(&action).Error; err != nil {
		t.Fatal(err)
	}
	settled, err := AdvanceRecommendationLabels(t.Context(), nil)
	if err == nil {
		t.Error("缺少实际成交价格的复权依据时应披露无法结算")
	}
	if settled != 1 {
		t.Errorf("只结算不依赖原始成交价的模拟标签，得到 %d", settled)
	}
	var stored model.RecommendationLabel
	if err := common.DB.First(&stored, label.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.MaturityStatus != model.LabelPending || stored.NetReturnPct != 0 {
		t.Fatalf("20元实际成交价与送股后10元前复权价格不可直接生成约50%%亏损: %+v", stored)
	}
}

func TestLabelResumeUsesSignalNameForLimit(t *testing.T) {
	for _, source := range []string{"picked", "shadow"} {
		for _, wasST := range []bool{false, true} {
			t.Run(source+map[bool]string{false: "_后来ST", true: "_后来摘帽"}[wasST], func(t *testing.T) {
				setupTestDB(t)
				cleanLabelTables(t)
				date := time.Now().AddDate(0, 0, -10)
				oldName, newName := "本地股票", "ST本地"
				if wasST {
					oldName, newName = newName, oldName
				}
				if err := common.DB.Create(&model.MarketSyncState{Symbol: "600001", Market: "cn", Name: newName}).Error; err != nil {
					t.Fatal(err)
				}
				for offset := 0; offset < 3; offset++ {
					price := 10.0
					if offset > 0 {
						price = 10.6
					}
					if err := common.DB.Create(&model.DailyBar{Symbol: "600001", Market: "cn", TradeDate: date.AddDate(0, 0, offset).Format("2006-01-02"), Open: price, High: price + 0.1, Low: price - 0.1, Close: price, Source: "test"}).Error; err != nil {
						t.Fatal(err)
					}
				}
				label := model.RecommendationLabel{UserID: 8978, Symbol: "600001", Market: "cn", SignalDate: date.Format("2006-01-02"), HorizonDays: 1, EntryMode: model.EntryModeNextOpen, MaturityStatus: model.LabelPending}
				if source == "picked" {
					rec := model.Recommendation{UserID: label.UserID, Symbol: label.Symbol, Market: "cn", Name: oldName, DetailJSON: `{}`}
					if err := common.DB.Create(&rec).Error; err != nil {
						t.Fatal(err)
					}
					label.RecommendationID = rec.ID
				} else {
					event := model.RecommendationCandidateEvent{UserID: label.UserID, Symbol: label.Symbol, Market: "cn", Name: oldName}
					if err := common.DB.Create(&event).Error; err != nil {
						t.Fatal(err)
					}
					label.CandidateEventID = event.ID
				}
				if err := common.DB.Create(&label).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := AdvanceRecommendationLabels(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
				var after model.RecommendationLabel
				if err := common.DB.First(&after, label.ID).Error; err != nil {
					t.Fatal(err)
				}
				want := model.LabelMatured
				if wasST {
					want = model.LabelSkipped
				}
				if after.MaturityStatus != want {
					t.Errorf("当前改名不能倒改历史入场判定: old=%s current=%s got=%s want=%s", oldName, newName, after.MaturityStatus, want)
				}
			})
		}
	}
}
