package service

import (
	"context"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func reviewPositionAccount(t *testing.T, p *model.Position) model.PortfolioAccount {
	t.Helper()
	account := model.PortfolioAccount{UserID: p.UserID, Name: "归档边界回归", Kind: model.PortfolioKindReal,
		Status: model.PortfolioStatusActive, Currency: "CNY"}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	p.AccountID = account.ID
	if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	return account
}

func archiveReviewAccount(t *testing.T, account model.PortfolioAccount) {
	t.Helper()
	if _, err := NewPortfolioAccountService().Archive(account.UserID, account.ID); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedPositionRejectsDelayedRiskFacts(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	date := now.Format("2006-01-02")
	p := seedHoldingWithPeak(t, 998, "600101", "归档仓位", 10, 100, 10, previousDate(date))
	account := reviewPositionAccount(t, p)
	rule := model.AlertRule{UserID: p.UserID, Market: "cn", Kind: model.AlertKindCostDrawdown,
		Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	row := evaluatePositionExit(positionExitInput{position: *p, quote: freshExitQuote(now, 8, 8, 8),
		rules: []model.AlertRule{rule}, now: now, session: model.PositionExitSessionIntraday}, defaultPositionExitParams)
	archiveReviewAccount(t, account)
	h := positionAlertHit{Position: *p, Value: 20, Message: "旧评估命中", TradeDate: date}
	created, active, err := persistPositionAlertEvaluation(context.Background(), rule, []positionAlertHit{h}, 20, true, date, now)
	if err != nil || len(created) != 0 || active {
		t.Errorf("归档后不能落入途提醒：created=%v active=%v err=%v", created, active, err)
	}
	if inserted, notify, err := persistPositionExitAssessment(context.Background(), &row); err != nil || inserted || notify {
		t.Errorf("归档后不能落新卖出风险：inserted=%v notify=%v err=%v", inserted, notify, err)
	}
	review := model.SellReview{UserID: p.UserID, PositionID: p.ID, Symbol: p.Symbol, Market: p.Market,
		Status: model.SellReviewStatusOpen, Trigger: "ma_break", TradeDate: date}
	if inserted, err := upsertSellReview(&review); err != nil || inserted {
		t.Errorf("归档后不能落新待复核：inserted=%v err=%v", inserted, err)
	}
}

func TestArchivedPositionHistoryDoesNotRemainActionable(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 999, "600102", "历史风险", 10, 100, 10, "2026-08-01")
	account := reviewPositionAccount(t, p)
	event := model.AlertEvent{UserID: p.UserID, PositionID: p.ID, Status: model.AlertEventUnread}
	review := model.SellReview{UserID: p.UserID, PositionID: p.ID, Status: model.SellReviewStatusOpen}
	if err := common.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Create(&review).Error; err != nil {
		t.Fatal(err)
	}
	archiveReviewAccount(t, account)
	alerts := &AlertService{}
	if rows, err := alerts.TriggeredForUser(p.UserID); err != nil || len(rows) != 0 {
		t.Errorf("归档提醒不得仍在待办中：rows=%v err=%v", rows, err)
	}
	if rows, err := ListSellReviews(p.UserID, model.SellReviewStatusOpen); err != nil || len(rows) != 0 {
		t.Errorf("归档复核不得仍在待办中：rows=%v err=%v", rows, err)
	}
	if rows, err := ListSellReviews(p.UserID, "all"); err != nil || len(rows) != 1 {
		t.Errorf("归档历史必须保留：rows=%v err=%v", rows, err)
	}
	if _, err := alerts.SetEventStatus(p.UserID, event.ID, model.AlertEventUnread); err == nil {
		t.Error("归档后不得恢复活动提醒")
	}
	if _, err := SetSellReviewStatus(p.UserID, review.ID, model.SellReviewStatusOpen); err == nil {
		t.Error("归档后不得恢复待复核")
	}
}

func TestArchivedPositionSkippedBeforeQuotesAndPeakWrites(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	date := now.Format("2006-01-02")
	p := seedHoldingWithPeak(t, 1000, "600103", "停止后台更新", 10, 100, 10, "2026-08-01")
	account := reviewPositionAccount(t, p)
	if err := common.DB.Create(&model.DailyBar{Symbol: p.Symbol, Market: p.Market, TradeDate: previousDate(date),
		Open: 12, High: 13, Low: 11, Close: 12, Source: "eastmoney"}).Error; err != nil {
		t.Fatal(err)
	}
	archiveReviewAccount(t, account)
	quotes := 0
	svc := &AlertService{market: &fakeAlertMarket{getFreshQuote: func(context.Context, string, string) (*datasource.Quote, quoteFreshInfo, error) {
		quotes++
		return &datasource.Quote{Price: 8, High: 8, Low: 8, DataTime: now}, quoteFreshInfo{Status: freshStatusFresh}, nil
	}}}
	rule := model.AlertRule{UserID: p.UserID, Market: "cn", Kind: model.AlertKindCostDrawdown, Op: model.AlertOpGTE, Threshold: 10, Status: model.AlertStatusActive}
	if err := common.DB.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.evaluatePositionRules(context.Background(), p.UserID, []model.AlertRule{rule}); err != nil {
		t.Fatal(err)
	}
	if quotes != 0 {
		t.Errorf("归档仓位仍请求行情 %d 次", quotes)
	}
	if _, err := syncPositionPeaksBefore(context.Background(), p.UserID, []model.Position{*p}, date); err != nil {
		t.Fatal(err)
	}
	var stored model.Position
	if err := common.DB.First(&stored, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.PeakPrice != 10 {
		t.Errorf("归档后峰值仍被后台改写：%v", stored.PeakPrice)
	}
	if ids, err := sellReviewUserIDs(context.Background()); err != nil || len(ids) != 0 {
		t.Errorf("归档仓位仍在复核候选中：ids=%v err=%v", ids, err)
	}
	if ids := guardCandidateUserIDs(); len(ids) != 0 {
		t.Errorf("归档仓位仍在守护候选中：%v", ids)
	}
}

func TestArchivedLegacyPositionReadsDoNotBackfillLedger(t *testing.T) {
	setupTestDB(t)
	p := &model.Position{UserID: 1001, Symbol: "600104", Market: "cn", Status: model.PositionStatusHolding, BuyPrice: 10, Quantity: 100}
	if err := common.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	account := reviewPositionAccount(t, p)
	archiveReviewAccount(t, account)
	if backfillPositionLedgers(p.UserID, []model.Position{*p}) {
		t.Error("归档历史列表读取不应补写账本")
	}
	if rows, err := (&PositionService{}).ListTrades(p.UserID, p.ID); err != nil || len(rows) != 0 {
		t.Errorf("应只返回归档账户已有流水：rows=%v err=%v", rows, err)
	}
	var stored model.Position
	if err := common.DB.First(&stored, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.TotalBuyCost != 0 {
		t.Errorf("归档成本汇总被补写：%v", stored.TotalBuyCost)
	}
}

func TestArchivedPositionRejectsDelayedGuardAndPeak(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithPeak(t, 1002, "600105", "迟到守护", 10, 100, 10, "2026-08-01")
	account := reviewPositionAccount(t, p)
	archiveReviewAccount(t, account)
	hit := guardHit{Symbol: p.Symbol, Market: p.Market, Kind: model.GuardKindPosMove, Price: 12}
	if inserted, _ := recordGuardEventWithID(p.UserID, "2026-09-08", hit, *p); inserted {
		t.Error("归档后仍提交守护事件")
	}
	if wrote, err := commitPositionPeak(context.Background(), *p, map[string]any{"peak_price": 12}); err != nil || wrote {
		t.Errorf("归档后仍提交峰值：wrote=%v err=%v", wrote, err)
	}
}
