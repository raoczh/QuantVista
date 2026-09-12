package service

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

func seedRiskReadAccount(t *testing.T, userID int64, kind, date string) (*model.PortfolioAccount, func() error) {
	t.Helper()
	account, err := NewPortfolioAccountService().Create(userID, PortfolioAccountInput{Name: "风险读取一致性", Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	if kind == model.PortfolioKindPaper {
		svc := &PaperService{}
		if _, err := svc.TradeByAccount(t.Context(), userID, account.ID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 10, Quantity: 1000}); err != nil {
			t.Fatal(err)
		}
		return account, func() error {
			_, err := svc.TradeByAccount(t.Context(), userID, account.ID, TradeInput{Symbol: "510300", Market: "cn", Side: "buy", Price: 20, Quantity: 1000})
			return err
		}
	}
	if _, err := CreatePortfolioCashFlow(userID, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 100000, TradeDate: date, IdempotencyKey: "initial"}); err != nil {
		t.Fatal(err)
	}
	p := seedHoldingWithLedger(t, userID, "510300", 10, 1000, 5, 0, date)
	if err := common.DB.Model(&p).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
		t.Fatal(err)
	}
	return account, func() error {
		_, err := (&PositionService{}).AddTrade(userID, p.ID, PositionTradeInput{Side: "buy", Price: 20, Quantity: 1000, Fee: 5, TradeDate: date})
		return err
	}
}

func seedRiskReadBars(t *testing.T, now time.Time) {
	t.Helper()
	for i, close := range []float64{9, 11, 10, 10} {
		date := now.AddDate(0, 0, i-3).Format("2006-01-02")
		if err := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}).Error; err != nil {
			t.Fatal(err)
		}
		if err := common.DB.Create(&model.DailyBar{Symbol: "510300", Market: "cn", TradeDate: date, Open: close, High: close, Low: close, Close: close, Source: "eastmoney"}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPortfolioReadUsesOneLedgerDuringQuoteFetch(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	seedRiskReadBars(t, now)
	userID := int64(1040)
	for _, kind := range []string{model.PortfolioKindReal, model.PortfolioKindPaper} {
		for _, action := range []string{"overview", "stress", "rebalance", "risk"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				userID++
				account, trade := seedRiskReadAccount(t, userID, kind, now.Format("2006-01-02"))
				var once sync.Once
				traded := false
				svc := NewPortfolioRiskService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {
					once.Do(func() {
						if err := trade(); err != nil {
							t.Error(err)
						}
						traded = true
					})
				}})), nil)
				const initialAssets = 99995.0
				switch action {
				case "overview":
					view, err := svc.Overview(t.Context(), userID, account.ID)
					if err != nil {
						t.Fatal(err)
					}
					if view.TotalAssets.Status != RiskStatusAvailable || view.TotalAssets.Value != initialAssets || view.MarketValue != 10000 || view.Cash.Value != 89995 {
						t.Fatalf("取行情期间加仓不能把新现金与旧股数合计：%+v", view)
					}
				case "stress":
					view, err := svc.Stress(t.Context(), userID, account.ID, StressScenario{Type: "market", ShockPct: -10})
					if err != nil {
						t.Fatal(err)
					}
					if view.BaseValue != initialAssets || view.EstimatedLossAmount != -1000 {
						t.Fatalf("压力损失与总资产分母必须使用同一账本：%+v", view)
					}
				case "rebalance":
					rev, err := svc.SaveTargets(userID, account.ID, []TargetAllocationItem{{Type: "symbol", Key: "510300", TargetWeightPct: 50, Enabled: true}})
					if err != nil {
						t.Fatal(err)
					}
					view, err := svc.Rebalance(t.Context(), userID, account.ID, rev.Revision)
					if err != nil {
						t.Fatal(err)
					}
					if view.TotalAssets.Value != initialAssets || len(view.Items) != 1 || view.Items[0].QuantityChange != 3900 || view.Items[0].AmountChange != 39997.5 {
						t.Fatalf("再平衡目标金额与现有股数必须使用同一账本：%+v", view)
					}
				case "risk":
					view, err := svc.Risk(t.Context(), userID, account.ID, NewPortfolioRiskParameters(30, 252, 0, "", ""))
					if err != nil {
						t.Fatal(err)
					}
					if len(view.RiskContribution.Items) != 1 || math.Abs(view.RiskContribution.Items[0].WeightPct-10) > 0.01 {
						t.Fatalf("风险权重不能使用交易前市值和交易后现金：%+v", view.RiskContribution)
					}
				}
				if !traded {
					t.Fatal("测试没有在取行情期间执行加仓")
				}
			})
		}
	}
}

func TestPaperRiskRejectsCashFromForeignTradeHistory(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	seedRiskReadBars(t, now)
	account, _ := seedRiskReadAccount(t, 1050, model.PortfolioKindPaper, now.Format("2006-01-02"))
	legacy := model.PaperTrade{UserID: account.UserID, AccountID: account.ID, Symbol: "AAPL", Market: "us", Side: "sell", Price: 100, Quantity: 1, Amount: 100, TradeDate: now.Format("2006-01-02")}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewPortfolioRiskService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})), nil)
	view, err := svc.Risk(context.Background(), account.UserID, account.ID, NewPortfolioRiskParameters(30, 252, 0, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if view.RiskContribution.PredictedVolatility.Status != RiskStatusUnavailable {
		t.Fatalf("旧外币交易导致现金口径不明时不能给出确定风险贡献：%+v", view.RiskContribution)
	}
}

func TestStressUsesEachPositionStopLoss(t *testing.T) {
	setupTestDB(t)
	now := reviewSnapshotClock(t)
	for i, tc := range []struct {
		name             string
		secondStop, loss float64
		missing          bool
	}{{"两笔不同止损", 5, -600, false}, {"一笔止损已高于现价", 12, -100, false}, {"一笔未设置止损", 0, -100, true}} {
		t.Run(tc.name, func(t *testing.T) {
			userID := int64(1070 + i)
			account, err := NewPortfolioAccountService().Create(userID, PortfolioAccountInput{Name: tc.name, Kind: model.PortfolioKindReal})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CreatePortfolioCashFlow(userID, account.ID, CashFlowInput{Type: model.CashFlowDeposit, Amount: 10000, TradeDate: now.Format("2006-01-02"), IdempotencyKey: "initial"}); err != nil {
				t.Fatal(err)
			}
			for _, stop := range []float64{9, tc.secondStop} {
				p := seedHoldingWithLedger(t, userID, "510300", 10, 100, 0, 0, now.Format("2006-01-02"))
				if err := common.DB.Model(&p).Updates(map[string]any{"account_id": account.ID, "plan_stop_loss": stop}).Error; err != nil {
					t.Fatal(err)
				}
				if err := common.DB.Model(&model.PositionTrade{}).Where("position_id = ?", p.ID).Update("account_id", account.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			svc := NewPortfolioRiskService(NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{quoteTime: now, hook: func() {}})), nil)
			view, err := svc.Stress(t.Context(), userID, account.ID, StressScenario{Type: "plan_stop_loss"})
			if err != nil {
				t.Fatal(err)
			}
			missing := false
			for _, reason := range view.Unknown {
				missing = missing || strings.Contains(reason, "缺少计划止损价")
			}
			if view.EstimatedLossAmount != tc.loss || view.BaseValue != 10000 || len(view.Contributions) != 1 || view.Contributions[0].LossAmount != tc.loss || missing != tc.missing {
				t.Fatalf("同一股票不能把第一笔止损价套给所有仓位：view=%+v wantLoss=%v missing=%v", view, tc.loss, tc.missing)
			}
		})
	}
}
