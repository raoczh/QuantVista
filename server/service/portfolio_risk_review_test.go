package service

import (
	"context"
	"encoding/json"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRiskMetricKnownZeroSurvivesJSON(t *testing.T) {
	for _, metric := range []RiskMetric{available(0, 2), unavailable("没有样本", 0)} {
		raw, err := json.Marshal(metric)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		value, exists := body["value"]
		if metric.Status == RiskStatusAvailable && (!exists || value != float64(0)) {
			t.Fatalf("已知的零现金/零收益应以 0 返回，不能与缺失值混淆：%s", raw)
		}
		if metric.Status == RiskStatusUnavailable && exists {
			t.Fatalf("未知结果不能伪造为零：%s", raw)
		}
	}
}

func TestZeroCorrelationAndContributionSurviveJSON(t *testing.T) {
	series := map[string]map[string]float64{
		"a":    {"d1": -1, "d2": 0, "d3": 1},
		"b":    {"d1": 1, "d2": -2, "d3": 1},
		"flat": {"d1": 0, "d2": 0, "d3": 0},
	}
	checkZero := func(value any, keys ...string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if value, ok := body[key]; !ok || value != float64(0) {
				t.Fatalf("有效零值 %s 被省略：%s", key, raw)
			}
		}
	}
	correlation := CorrelationFromReturns(map[string]map[string]float64{"a": series["a"], "b": series["b"]}, 3, "d3")
	checkZero(correlation.Cells[0][1], "value")
	contribution := ComputeRiskContributions(series, map[string]float64{"a": 0.5, "flat": 0.5}, 252, 3, "d3")
	checkZero(contribution.Items[1], "marginal_volatility_pct", "component_volatility_pct", "risk_contribution_pct")
}

func TestTargetSaveRechecksArchivedAccountInTransaction(t *testing.T) {
	setupTestDB(t)
	accounts := NewPortfolioAccountService()
	if _, err := accounts.Create(985, PortfolioAccountInput{Name: "默认", Kind: "real"}); err != nil {
		t.Fatal(err)
	}
	account, err := accounts.Create(985, PortfolioAccountInput{Name: "待归档", Kind: "real"})
	if err != nil {
		t.Fatal(err)
	}
	const callback = "review:archive_after_account_validation"
	archived := false
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(db *gorm.DB) {
		row, ok := db.Statement.Dest.(*model.PortfolioAccount)
		if !ok || row.ID != account.ID || archived {
			return
		}
		archived = true
		if _, err := accounts.Archive(985, account.ID); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	_, err = NewPortfolioRiskService(nil, nil).SaveTargets(985, account.ID, []TargetAllocationItem{{Type: "symbol", Key: "600001", TargetWeightPct: 30, MaxWeightPct: 100, Enabled: true}})
	if !archived {
		t.Fatal("测试未触发外层验证后的归档")
	}
	if err == nil {
		t.Fatal("账户已归档，不得保存目标配置")
	}
	var count int64
	if err := common.DB.Model(&model.TargetAllocationRevision{}).Where("account_id = ?", account.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("归档后产生新目标版本：%d", count)
	}
}

func TestRiskForAccountWithoutSnapshotsReturnsUnavailable(t *testing.T) {
	setupTestDB(t)
	account, err := NewPortfolioAccountService().Create(986, PortfolioAccountInput{Name: "空账户", Kind: "real"})
	if err != nil {
		t.Fatal(err)
	}
	risk, err := NewPortfolioRiskService(nil, nil).Risk(context.Background(), 986, account.ID, NewPortfolioRiskParameters(252, 252, 0, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if risk.TWR.Status != RiskStatusUnavailable || len(risk.Curve) != 0 {
		t.Fatalf("没有资产快照时应返回样本不足：%+v", risk)
	}
}

func TestRebalanceZeroQuantityDoesNotChargeFees(t *testing.T) {
	holdings := []RebalanceHolding{{Symbol: "600000", Market: "cn", Value: 10000, Quantity: 1000, Price: 10, Fresh: true}}
	for _, target := range []float64{50, 51} {
		draft := BuildRebalanceDraft(holdings, []TargetAllocationItem{{Type: "symbol", Key: "600000", TargetWeightPct: target, Enabled: true}}, 20000)
		if len(draft) != 1 || draft[0].QuantityChange != 0 || draft[0].EstimatedFee != 0 || draft[0].EstimatedTax != 0 {
			t.Fatalf("无需调整或不足一手时不能计交易费用：%+v", draft)
		}
	}
}

func TestPortfolioRiskDoesNotAnnualizeMultiDayGaps(t *testing.T) {
	setupTestDB(t)
	account, err := NewPortfolioAccountService().Create(987, PortfolioAccountInput{Name: "有日期缺口", Kind: model.PortfolioKindPaper})
	if err != nil {
		t.Fatal(err)
	}
	dates := []string{"2026-08-03", "2026-08-04", "2026-08-05", "2026-08-06", "2026-08-07", "2026-08-10"}
	for _, date := range dates {
		cal := model.TradingCalendar{Market: "cn", TradeDate: date, IsOpen: true}
		if err := common.DB.Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"is_open"})}).Create(&cal).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, date := range []string{dates[0], dates[1], dates[3], dates[4], dates[5]} {
		snap := model.PortfolioSnapshot{UserID: 987, AccountID: account.ID, Kind: model.PortfolioKindPaper, TradeDate: date, Cash: 100 + float64(i)}
		if err := common.DB.Create(&snap).Error; err != nil {
			t.Fatal(err)
		}
		bar := model.DailyBar{Symbol: "600076", Market: "cn", TradeDate: date, Close: snap.Cash, Source: "eastmoney"}
		if err := common.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&bar).Error; err != nil {
			t.Fatal(err)
		}
	}
	risk, err := NewPortfolioRiskService(nil, nil).Risk(context.Background(), 987, account.ID, NewPortfolioRiskParameters(30, 252, 0, "600076", dates[5]))
	if err != nil {
		t.Fatal(err)
	}
	if len(risk.Curve) != 5 || risk.Curve[2].Return != nil || risk.AnnualizedVolatility.SampleCount != 3 {
		t.Errorf("不能把跨过 08-05 的收益当成 08-06 单日收益，也不能补造曲线点：%+v", risk)
	}
	if risk.TWR.Status != RiskStatusUnavailable || risk.MaxDrawdown.Metric.Status != RiskStatusPartial {
		t.Errorf("断开的收益区间不能称为完整窗口收益或完整回撤：twr=%+v dd=%+v", risk.TWR, risk.MaxDrawdown)
	}
	returns, err := localBarReturns("600076", "cn", 30, dates[5])
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := returns[dates[3]]; exists || len(returns) != 3 {
		t.Errorf("基准/持仓日收益也必须剔除跨多日区间：%+v", returns)
	}
}

func TestTWRDoesNotJoinDisconnectedSegments(t *testing.T) {
	points := []EquityPoint{{Assets: 100}, {Assets: 110}, {Partial: true}, {Assets: 150}, {Assets: 165}}
	if got := ComputeTWR(points); got.Status != RiskStatusUnavailable {
		t.Fatalf("两个完整片段相乘的 21%% 不能当成全窗口收益：%+v", got)
	}
}
