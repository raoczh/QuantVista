package service

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"quantvista/datasource"
)

func TestRecommendationMarketUsesCompletedBenchmarkAndKnownDates(t *testing.T) {
	now := time.Date(2026, 9, 11, 11, 0, 0, 0, time.Local)
	bars := make([]datasource.Bar, 201)
	for i := range bars {
		bars[i] = datasource.Bar{TradeDate: now.AddDate(0, 0, i-200).Format("2006-01-02"), Close: 3000 + float64(i)}
	}
	bars[200].Close = 1000 // 未完成的盘中急跌不能改写昨日收盘趋势
	before := append([]datasource.Bar(nil), bars...)
	ov := &Overview{Indices: []datasource.Index{{Name: "上证", Price: 3200, DataTime: now, ChangePct: 1}}, Breadth: &datasource.Breadth{TradeDate: "2026-09-11", Advances: 2000, Declines: 1000, LimitUp: 20, LimitDown: 5, DataTime: now}, FundFlow: &datasource.MarketFundFlow{TradeDate: "2026-09-11", MainNet: 0, DataTime: now}}
	mc, regime := buildRecommendationMarketFacts(ov, bars, now, "2026-09-11", "2026-09-10", marketStateTrading)
	if mc.BenchAsOf != "2026-09-10" || !strings.Contains(mc.BenchTrend, "MA200上方") || regime.BenchmarkAsOf != "2026-09-10" || regime.BreadthAsOf != "2026-09-11" {
		t.Fatalf("收盘与盘中依据必须分开: %+v %+v", mc, regime)
	}
	if !reflect.DeepEqual(before, bars) {
		t.Fatal("基准筛选不能排序或修改共享行情切片")
	}
	raw, _ := json.Marshal(mc)
	if mc.MainNetYi == nil || !strings.Contains(string(raw), `"main_fund_net_yi":0`) {
		t.Fatal("已知资金净流入为零不能冒充缺失")
	}
	ov.Breadth.TradeDate = "2026-09-10"
	ov.FundFlow.TradeDate = "2026-09-10"
	mc, regime = buildRecommendationMarketFacts(ov, bars, now, "2026-09-11", "2026-09-10", marketStateTrading)
	if mc.Breadth != nil || mc.MainNetYi != nil || regime.BreadthAsOf != "" || len(mc.Missing) < 2 {
		t.Fatal("不能把不同业务日期的市场宽度和资金流拼接成今日环境")
	}
	bars[198].Close = math.NaN()
	mc, _ = buildRecommendationMarketFacts(ov, bars, now, "2026-09-11", "2026-09-10", marketStateTrading)
	if mc.BenchTrend != "" || mc.BenchAsOf != "" {
		t.Fatal("无效基准不能输出趋势判断")
	}
}
