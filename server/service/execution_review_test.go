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

func TestSimEntryBudgetIncludesFees(t *testing.T) {
	bars := []datasource.Bar{
		{TradeDate: "2026-09-01", Open: 10, High: 10, Low: 10, Close: 10},
		{TradeDate: "2026-09-02", Open: 10, High: 10, Low: 10, Close: 10},
	}
	_, quantity, cost, skip := simEntry(bars, 0, "600000", "普通股票", 20000, "2026-09-02")
	if skip != "" || quantity != 1900 || cost != 19005 {
		t.Fatalf("20000 元应含佣金买入 1900 股、花费 19005：quantity=%v cost=%v skip=%s", quantity, cost, skip)
	}
	for i := range bars {
		bars[i].Open, bars[i].High, bars[i].Low, bars[i].Close = 200, 200, 200, 200
	}
	if _, _, _, skip := simEntry(bars, 0, "600000", "普通股票", 20000, "2026-09-02"); skip != btSkipCash {
		t.Fatalf("一手含手续费超预算应跳过，得到 %s", skip)
	}
}

func TestSimulateLabelHoldOpeningBarrierPriority(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		open, high, low, close float64
		wantPrice              float64
		stop                   bool
	}{
		{"跳空止损", 9.2, 9.4, 9, 9.3, 9.2, true},
		{"跳空止盈", 11.2, 11.4, 11.1, 11.3, 11.2, false},
		{"开盘止盈后盘中双触", 11.2, 11.4, 9, 9.3, 11.2, false},
		{"盘中双触保守止损", 10, 11.4, 9, 10.3, 9.5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bars := []datasource.Bar{
				{TradeDate: "2026-09-01", Open: 10, High: 10, Low: 10, Close: 10},
				{TradeDate: "2026-09-02", Open: 10, High: 10.2, Low: 9.8, Close: 10},
				{TradeDate: "2026-09-03", Open: tc.open, High: tc.high, Low: tc.low, Close: tc.close},
			}
			out := simulateLabelHold(bars, 0, "600000", "普通股票", 1, 20000, 11, 9.5,
				"2026-09-02", "2026-09-03", "2026-09-03")
			if out.Status != btTraded || out.SellPrice != tc.wantPrice || out.HitStopLoss != tc.stop || out.HitTakeProfit == tc.stop {
				t.Fatalf("障碍成交价和开盘先触次序错误：%+v", out)
			}
		})
	}
}

func TestModelCannotSetRecommendationDegradedSource(t *testing.T) {
	content := `{"picks":[{"symbol":"600000","action":"buy","confidence":75,"degraded_source":"quant_fallback"}]}`
	picks, _, _, err := parseAndFilterPicks(content, testPool(), 3)
	if err != nil || len(picks) != 1 {
		t.Fatalf("正常模型样本未解析：%v", err)
	}
	if picks[0].DegradedSource != "" {
		t.Fatalf("模型不能把自身输出归类为规则降级：%q", picks[0].DegradedSource)
	}
}

func TestLabelReadFailurePreservesPending(t *testing.T) {
	setupTestDB(t)
	signal := time.Now().AddDate(0, 0, -100)
	label := model.RecommendationLabel{
		UserID: 901, CandidateEventID: 901, Symbol: "600000", Market: "cn",
		HorizonDays: 1, EntryMode: model.EntryModeNextOpen, SignalDate: signal.Format("2006-01-02"),
		MaturityStatus: model.LabelPending, LabelVersion: labelVersion,
	}
	if err := common.DB.Create(&label).Error; err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < 3; offset++ {
		bar := model.DailyBar{Symbol: "600000", Market: "cn", TradeDate: signal.AddDate(0, 0, offset).Format("2006-01-02"),
			Open: 10, High: 10.1, Low: 9.9, Close: 10, Source: "eastmoney"}
		if err := common.DB.Create(&bar).Error; err != nil {
			t.Fatal(err)
		}
	}
	const callback = "review_daily_bar_read_failure"
	fault := errors.New("临时数据库读取故障")
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "daily_bars" {
			tx.AddError(fault)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	settled, err := AdvanceRecommendationLabels(context.Background(), nil)
	if settled != 0 || !errors.Is(err, fault) {
		t.Fatalf("读取故障必须向上传递：settled=%d err=%v", settled, err)
	}
	var actual model.RecommendationLabel
	if err := common.DB.First(&actual, label.ID).Error; err != nil {
		t.Fatal(err)
	}
	if actual.MaturityStatus != model.LabelPending {
		t.Fatalf("读取失败不能永久写入无数据终态：%s", actual.MaturityStatus)
	}
}
