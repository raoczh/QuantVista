package service

import (
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestMissedOpportunityDoesNotTreatShareSplitAsAvoidedLoss(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	passedAt := now.AddDate(0, 0, -1)
	item := model.WatchlistItem{UserID: 8962, WatchlistID: 1, Symbol: "600001", Market: "cn", ResearchStage: model.StagePassed, PassedPrice: 20, StageAt: &passedAt}
	if err := common.DB.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&model.CorporateAction{Symbol: item.Symbol, Market: "cn", ExDate: now.Format("2006-01-02"), BonusRatio: 10,
		Progress: model.CorpActionProgressImplemented, PlanNoticeDate: passedAt.Format("2006-01-02")}).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewWatchlistService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})))
	rows, err := svc.MissedOpportunities(t.Context(), item.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].QuoteOK {
		t.Fatalf("夹具必须提供新鲜的除权后价格：%+v", rows)
	}
	if rows[0].Verdict == "avoided_loss" || rows[0].ChangeSincePct != 0 {
		t.Fatalf("送股导致名义价格折半，不能宣称回避了50%%亏损：%+v", rows[0])
	}
}

func TestWatchlistRepeatedPassedStageKeepsBaseline(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	passedAt := now.AddDate(0, 0, -1)
	item := model.WatchlistItem{UserID: 8963, WatchlistID: 1, Symbol: "600001", Market: "cn", ResearchStage: model.StagePassed, PassedPrice: 20, StageAt: &passedAt}
	if err := common.DB.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewWatchlistService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})))
	updated, err := svc.SetItemStage(t.Context(), item.UserID, item.ID, model.StagePassed, "补充放弃原因")
	if err != nil {
		t.Fatal(err)
	}
	if updated.PassedPrice != 20 || updated.StageAt == nil || !updated.StageAt.Equal(passedAt) || updated.PassedReason != "补充放弃原因" {
		t.Fatalf("重复标记放弃不能重置已经记录的参照价格与时间：%+v", updated)
	}
}
