package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestCanceledCorpSyncCannotAdjustPaperLedger(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 14120
	account, err := (&PaperService{}).GetOrCreateAccount(userID)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	holding := model.PaperHolding{UserID: userID, AccountID: account.AccountID, Symbol: "600061", Market: "cn", Quantity: 100, AvgCost: 10}
	for _, row := range []any{
		&holding,
		&model.PaperTrade{UserID: userID, AccountID: account.AccountID, Symbol: holding.Symbol, Market: "cn", Side: model.PaperSideBuy, Price: 10, Quantity: 100, Amount: 1000, TradeDate: previousDate(today)},
		&model.CorporateAction{Symbol: holding.Symbol, Market: "cn", ReportDate: "2026-06-30", PlanNoticeDate: previousDate(today), ExDate: today, RecordDate: previousDate(today), Progress: model.CorpActionProgressImplemented, TransferRatio: 10},
	} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// 不注入东财适配器，全部同步路径均无网络访问。
	if (&CorpActionService{}).RunCorpActionSync(ctx) {
		t.Error("已取消的同步不能宣称成功")
	}
	if err := common.DB.First(&holding, holding.ID).Error; err != nil || holding.Quantity != 100 || holding.AvgCost != 10 {
		t.Fatalf("已取消同步不能继续改变模拟盘账本：holding=%+v err=%v", holding, err)
	}
}

func TestCanceledCorpStoresPreserveExistingFacts(t *testing.T) {
	setupTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	from, to := "2026-09-10", "2026-09-12"
	corp := model.CorporateAction{Symbol: "600061", Market: "cn", ReportDate: "2026-06-30", ExDate: from, PlanNoticeDate: from, DividendPretax: 1}
	lift := model.RestrictedRelease{Symbol: corp.Symbol, Market: "cn", FreeDate: from, FreeShares: 1000}
	ipo := model.IpoSubscription{Kind: model.IpoKindStock, Code: corp.Symbol, ApplyCode: "730061", ApplyDate: from}
	for _, row := range []any{&corp, &lift, &ipo} {
		if err := common.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	changed := corp
	changed.DividendPretax = 2
	for name, run := range map[string]func() error{
		"方案": func() error { return storeCorporateActions([]model.CorporateAction{changed}, ctx) },
		"解禁": func() error { return storeRestrictedReleases(nil, from, to, ctx) },
		"申购": func() error { return storeIpoSubscriptions(model.IpoKindStock, nil, from, to, ctx) },
	} {
		if err := run(); !errors.Is(err, context.Canceled) {
			t.Errorf("取消后不得覆盖或删除%s事实：%v", name, err)
		}
	}
	if err := common.DB.First(&corp, corp.ID).Error; err != nil || corp.DividendPretax != 1 {
		t.Fatalf("方案发生了迟到覆盖：%+v err=%v", corp, err)
	}
	for _, row := range []any{&lift, &ipo} {
		if err := common.DB.First(row).Error; err != nil {
			t.Fatalf("取消同步错误清理了窗口中的存量事实：%v", err)
		}
	}
}

func TestCanceledUnmaterializedShareActionDoesNotBlockPosition(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	p, action := seedAdjustCase(t, 14121, today, 0, 10, 0)
	if err := common.DB.Model(action).Update("progress", "取消分配").Error; err != nil {
		t.Fatal(err)
	}
	unconfirmed, err := positionsWithUnconfirmedShareAction(t.Context(), p.UserID, []int64{p.ID}, today)
	if err != nil || unconfirmed[p.ID] {
		t.Fatalf("已取消且从未生成待确认记录的方案不能制造送转障碍：%v err=%v", unconfirmed, err)
	}
}
