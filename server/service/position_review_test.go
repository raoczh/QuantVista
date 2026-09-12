package service

import (
	"context"
	"math"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestPositionNoteEditPreservesCumulativeLedger(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithLedger(t, 921, "600071", 10, 1000, 0, 0, "2026-06-01")
	svc := &PositionService{}
	before, err := svc.AddTrade(921, p.ID, PositionTradeInput{Side: "sell", Price: 12, Quantity: 500, TradeDate: "2026-06-02"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := svc.Update(921, p.ID, PositionInput{
		BuyPrice: before.BuyPrice, BuyDate: before.BuyDate, Quantity: before.Quantity,
		BuyFee: before.BuyFee, BuyTax: before.BuyTax, PositionType: before.PositionType,
		UserNote: "只更新复盘备注",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ledgerFromPosition(after) != ledgerFromPosition(before) {
		t.Fatalf("只改备注不能改变累计投入、历史数量或剩余成本：before=%+v after=%+v", ledgerFromPosition(before), ledgerFromPosition(after))
	}
	closed, err := svc.Close(921, p.ID, CloseInput{SellPrice: 11, SellDate: "2026-06-03"})
	if err != nil {
		t.Fatal(err)
	}
	view := computeView(*closed, 0, false)
	if view.ProfitAmount != 1500 || view.Cost != 10000 || view.ProfitPct != 15 {
		t.Fatalf("平仓收益应为原始投入 10000 上的 1500 / 15%%：%+v", view)
	}
}

type reviewQuoteHookAdapter struct {
	hook      func()
	quoteTime time.Time
}

func (a reviewQuoteHookAdapter) Name() string { return "review" }
func (a reviewQuoteHookAdapter) GetQuote(_ context.Context, market, symbol string) (*datasource.Quote, error) {
	a.hook()
	quoteTime := a.quoteTime
	if quoteTime.IsZero() {
		quoteTime = time.Now()
	}
	return &datasource.Quote{Symbol: symbol, Market: market, Name: "测试持仓", Price: 10, DataTime: quoteTime, Source: "review"}, nil
}
func (a reviewQuoteHookAdapter) GetDailyBars(context.Context, string, string, int) ([]datasource.Bar, error) {
	return nil, datasource.ErrNoData
}

func TestCreatePositionRechecksAccountAfterMarketFetch(t *testing.T) {
	setupTestDB(t)
	for i, action := range []string{"archive", "delete"} {
		t.Run(action, func(t *testing.T) {
			uid := int64(930 + i)
			accounts := NewPortfolioAccountService()
			if _, err := accounts.Create(uid, PortfolioAccountInput{Name: "默认组合", Kind: "real"}); err != nil {
				t.Fatal(err)
			}
			account, err := accounts.Create(uid, PortfolioAccountInput{Name: "正在操作的组合", Kind: "real"})
			if err != nil {
				t.Fatal(err)
			}
			called := false
			adapter := reviewQuoteHookAdapter{hook: func() {
				called = true
				var err error
				if action == "archive" {
					_, err = accounts.Archive(uid, account.ID)
				} else {
					err = accounts.Delete(uid, account.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(adapter)))
			_, err = svc.CreateByAccount(context.Background(), uid, account.ID, PositionInput{
				Symbol: "600079", Market: "cn", PositionType: model.PositionTypeLongTerm,
				BuyPrice: 10, BuyDate: "2026-06-01", Quantity: 100,
			})
			if !called {
				t.Fatal("测试未触发取行情期间的账户状态变化")
			}
			if err == nil {
				t.Errorf("取行情期间账户已 %s，建仓必须被拒绝", action)
			}
			var positions int64
			if err := common.DB.Model(&model.Position{}).Where("account_id = ?", account.ID).Count(&positions).Error; err != nil {
				t.Fatal(err)
			}
			if positions != 0 {
				t.Fatalf("账户 %s 后留下了不可归属的持仓：%d", action, positions)
			}
		})
	}
}

func TestArchivedPositionRejectsUserMutations(t *testing.T) {
	setupTestDB(t)
	account := model.PortfolioAccount{UserID: 942, Kind: "real", Name: "已归档", Status: model.PortfolioStatusArchived}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PositionService{}
	for _, action := range []string{"update", "buy", "close", "delete", "link"} {
		t.Run(action, func(t *testing.T) {
			p := seedHoldingWithLedger(t, 942, "600070", 10, 100, 0, 0, "2026-06-01")
			if err := common.DB.Model(&p).Update("account_id", account.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "update":
				_, err = svc.Update(942, p.ID, PositionInput{BuyPrice: 10, Quantity: 100, BuyDate: p.BuyDate, PositionType: p.PositionType, UserNote: "不应写入"})
			case "buy":
				_, err = svc.AddTrade(942, p.ID, PositionTradeInput{Side: "buy", Price: 10, Quantity: 100})
			case "close":
				_, err = svc.Close(942, p.ID, CloseInput{SellPrice: 10})
			case "delete":
				err = svc.Delete(942, p.ID)
			case "link":
				_, err = svc.LinkRecommendation(942, p.ID, 0)
			}
			if err == nil {
				t.Errorf("归档账户的 %s 必须拒绝", action)
			}
			var after model.Position
			if err := common.DB.First(&after, p.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.Quantity != 100 || after.Status != model.PositionStatusHolding || after.UserNote != "" {
				t.Fatalf("归档持仓被改写：%+v", after)
			}
		})
	}
}

func TestPositionTradeValuesMatchStoredPrecision(t *testing.T) {
	setupTestDB(t)
	p := seedHoldingWithLedger(t, 943, "600072", 10, 1, 0, 0, "2026-07-01")
	svc := &PositionService{}
	for _, input := range []PositionTradeInput{
		{Side: "buy", Price: 10, Quantity: 0.00001},
		{Side: "buy", Price: 0.00001, Quantity: 100},
		{Side: "buy", Price: math.NaN(), Quantity: 100},
		{Side: "buy", Price: 10, Quantity: math.Inf(1)},
		{Side: "buy", Price: 10, Quantity: 100, Fee: math.Inf(1)},
		{Side: "buy", Price: 1e10, Quantity: 1e10},
	} {
		if _, err := svc.AddTrade(943, p.ID, input); err == nil {
			t.Errorf("归零、非有限或超出存储范围的数值不能入账：%+v", input)
		}
	}
	after, err := svc.AddTrade(943, p.ID, PositionTradeInput{Side: "buy", Price: 10.123456, Quantity: 1.234567, Fee: 0.123456})
	if err != nil {
		t.Fatal(err)
	}
	var trade model.PositionTrade
	if err := common.DB.Where("position_id = ?", p.ID).Order("id DESC").First(&trade).Error; err != nil {
		t.Fatal(err)
	}
	if trade.Price != 10.1235 || trade.Quantity != 1.2346 || trade.Fee != 0.1235 || after.TotalBuyCost != 22.622 || after.Quantity != 2.2346 {
		t.Fatalf("按最终四位精度计算应为数量 2.2346、累计成本 22.622：trade=%+v position=%+v", trade, after)
	}
	in := PositionInput{PositionType: model.PositionTypeLongTerm, BuyPrice: 10, Quantity: 100,
		BuyDate: time.Now().AddDate(0, 0, 1).Format("2006-01-02")}
	if err := validateBuy(&in); err == nil {
		t.Error("未来买入日期不能记成实际持仓")
	}
}

func TestCreatePositionDoesNotLinkAnotherSymbol(t *testing.T) {
	setupTestDB(t)
	_, rec := seedLinkFixture(t, 944, "600073", model.RecTypeShortTerm, model.RecStatusSuccess)
	svc := NewPositionService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{hook: func() {}})))
	p, err := svc.Create(context.Background(), 944, PositionInput{
		Symbol: "600074", Market: "cn", PositionType: model.PositionTypeLongTerm,
		BuyPrice: 10, Quantity: 100, RecommendationID: rec.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.RecommendationID != 0 {
		t.Fatalf("买入 600074 不能关联 600073 的推荐：%+v", p)
	}
}

func TestLinkedRecommendationUsesEarliestRemainingPosition(t *testing.T) {
	setupTestDB(t)
	const uid = 945
	batch, rec := seedLinkFixture(t, uid, "600075", model.RecTypeShortTerm, model.RecStatusSuccess)
	st := model.RecommendationStatus{RecommendationID: rec.ID, BatchID: batch.ID, UserID: uid,
		Symbol: rec.Symbol, Market: "cn", Type: batch.Type, Action: rec.Action,
		RefPrice: 10, CurrentPrice: 12, Outcome: model.RecOutcomeExpired}
	if err := common.DB.Create(&st).Error; err != nil {
		t.Fatal(err)
	}
	a := seedHoldingWithLedger(t, uid, rec.Symbol, 10, 100, 0, 0, "2026-08-01")
	b := seedHoldingWithLedger(t, uid, rec.Symbol, 11, 100, 0, 0, "2026-08-02")
	svc := &PositionService{}
	check := func(price float64) {
		t.Helper()
		var after model.RecommendationStatus
		if err := common.DB.First(&after, st.ID).Error; err != nil {
			t.Fatal(err)
		}
		if after.ActualBuyPrice != price || (price == 0 && after.ActualReturnPct != nil) {
			t.Errorf("实际执行事实应跟随仍关联的最早持仓：want=%v got=%+v", price, after)
		}
	}
	for _, p := range []model.Position{a, b} {
		if _, err := svc.LinkRecommendation(uid, p.ID, rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	check(10)
	if _, err := svc.LinkRecommendation(uid, b.ID, 0); err != nil {
		t.Fatal(err)
	}
	check(10)
	if _, err := svc.LinkRecommendation(uid, b.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LinkRecommendation(uid, a.ID, 0); err != nil {
		t.Fatal(err)
	}
	check(11)
	if err := svc.Delete(uid, b.ID); err != nil {
		t.Fatal(err)
	}
	check(0)
}
