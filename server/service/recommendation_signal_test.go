package service

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

func qualityScenarioBars(extended bool) []datasource.Bar {
	bars := make([]datasource.Bar, 90)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for i := range bars {
		price := 10 + 0.03*math.Sin(float64(i))
		if extended && i >= 80 {
			price = 10 + float64(i-79)*0.2
		}
		bars[i] = datasource.Bar{TradeDate: start.AddDate(0, 0, i).Format("2006-01-02"), Open: price - 0.03, High: price + 0.1, Low: price - 0.1, Close: price, Volume: 100000, Amount: 1e8, TurnoverRate: 3}
	}
	if !extended {
		bars[89].Open = 10.03
		bars[89].Close = 10.42
		bars[89].High = 10.46
		bars[89].Low = 10.01
	}
	bars[89].Volume = 220000
	return bars
}

func qualityScenarioCandidate(bars []datasource.Bar) (candidate, *candFactors, ScoreResult) {
	c := candidate{Symbol: "600100", Market: "cn", Price: bars[len(bars)-1].Close, Amount: 1e8, TurnoverRate: 3, QuoteAsOf: bars[len(bars)-1].TradeDate + " 15:00"}
	c.SignalQuality = computeRecSignalQuality(c.Price, bars)
	c.Timing = buildRecTimeFacts(c, bars)
	f := computeCandFactors(c.Price, bars)
	c.Factors = f
	return c, f, computeScore(c.Price, bars)
}

func TestQualityScoringPrefersFreshBreakoutToExtendedRun(t *testing.T) {
	fresh, freshFactors, freshBase := qualityScenarioCandidate(qualityScenarioBars(false))
	late, lateFactors, lateBase := qualityScenarioCandidate(qualityScenarioBars(true))
	strat := &shortStrategies[0]
	a, _ := scoreQualityCandidate(model.RecTypeShortTerm, strat, fresh, freshFactors, freshBase)
	b, _ := scoreQualityCandidate(model.RecTypeShortTerm, strat, late, lateFactors, lateBase)
	if !a.Valid || !b.Valid || a.Total <= b.Total {
		t.Fatalf("刚确认且距离合理的突破应优先于连续延伸: fresh=%+v late=%+v", a, b)
	}
	if entry := entryQualityFor("momentum", "breakout", late, late.SignalQuality); entry.Status != "extended" {
		t.Fatalf("连续创高不得每天重置起点掩盖延伸: %+v %+v", entry, late.SignalQuality)
	}
	if fresh.SignalQuality.BreakoutConfirmed == nil || !*fresh.SignalQuality.BreakoutConfirmed {
		t.Fatal("应保留真实突破机会")
	}
}

func TestSignalQualityScaleInvarianceAndNoMutation(t *testing.T) {
	bars := qualityScenarioBars(false)
	before := append([]datasource.Bar(nil), bars...)
	a := computeRecSignalQuality(bars[89].Close, bars)
	scaled := append([]datasource.Bar(nil), bars...)
	for i := range scaled {
		scaled[i].Open *= 10
		scaled[i].High *= 10
		scaled[i].Low *= 10
		scaled[i].Close *= 10
	}
	b := computeRecSignalQuality(scaled[89].Close, scaled)
	for i, pair := range [][2]*float64{{a.MA20DistanceATR, b.MA20DistanceATR}, {a.BreakoutDistanceATR, b.BreakoutDistanceATR}, {a.Compression, b.Compression}, {a.CloseLocation, b.CloseLocation}, {a.Efficiency20, b.Efficiency20}} {
		if pair[0] == nil || pair[1] == nil || math.Abs(*pair[0]-*pair[1]) > 1e-5 {
			t.Fatalf("价格尺度改变不得改变质量因子 %d: %v", i, pair)
		}
	}
	if !reflect.DeepEqual(before, bars) {
		t.Fatal("因子计算不能改写日线")
	}
	short := computeRecSignalQuality(bars[9].Close, bars[:10])
	if short.ATR != nil || short.Compression != nil || len(short.Missing) == 0 {
		t.Fatal("窗口不足不应伪造完整指标")
	}
}

func TestQualityScoringAvoidsDuplicateTechnicalBonuses(t *testing.T) {
	c, f, sc := qualityScenarioCandidate(qualityScenarioBars(false))
	strat := &shortStrategies[0]
	a, _ := scoreQualityCandidate(model.RecTypeShortTerm, strat, c, f, sc)
	other := *f
	other.RSI14 = 68
	other.MACDXUp = true
	other.MACDGold = true
	other.MACDDif = 10
	other.MainNetDays = 8
	b, _ := scoreQualityCandidate(model.RecTypeShortTerm, strat, c, &other, sc)
	if a.Total != b.Total {
		t.Fatal("基础维度已反映的相关信号，不得在策略组再次层层加分")
	}
	poor := *f
	poor.BarCount = 5
	if result, _ := scoreQualityCandidate(model.RecTypeShortTerm, strat, c, &poor, sc); result.Valid {
		t.Fatal("只有中性占位维度不能获得有效评分")
	}
}

func TestQualityFinanceDoesNotRewardCheapLossOrMissingData(t *testing.T) {
	missing, _, state := qualityFinanceScore("value", candidate{PETTM: 3, PB: 0.3})
	if missing != 0 || state != "missing" {
		t.Fatal("缺财报时低 PE/PB 不能自行证明价值")
	}
	good := candidate{PETTM: 12, PB: 1.2, Fin: &candFin{Report: "年报", ROE: 12, RevenueYoY: 10, NetProfitYoY: 15}}
	loss := candidate{PETTM: -3, PB: 0.3, Fin: &candFin{Report: "年报", ROE: -4, RevenueYoY: -10, NetProfitYoY: -50}}
	for _, profile := range []string{"value", "growth", "leader"} {
		a, _, _ := qualityFinanceScore(profile, good)
		b, _, _ := qualityFinanceScore(profile, loss)
		if a <= b || b >= 0 {
			t.Fatalf("%s 应区分盈利质量与便宜亏损: %v %v", profile, a, b)
		}
	}
}

func TestEntryQualityIsExecutionConstraintAndScoreBlindDoesNotLeakRanks(t *testing.T) {
	c, f, sc := qualityScenarioCandidate(qualityScenarioBars(false))
	c.EntryQuality = &recEntryQuality{Version: "eq1", Status: "extended", Reasons: []string{"延伸较大，等待回踩"}}
	c.ScoreBreakdown, _ = scoreQualityCandidate(model.RecTypeShortTerm, &shortStrategies[0], c, f, sc)
	c.FinalCheck = &recFinalCheck{Price: c.Price, Passed: true, EntryQuality: c.EntryQuality}
	p := executionTestShortPick()
	p.Symbol = c.Symbol
	plan := buildExecutionPlan(model.RecTypeShortTerm, p, c, executionTestSnapshot("balanced", HorizonShortTerm, 100000), false, true, "fresh")
	if plan.Status != executionWait || !hasExecutionReason(plan, "等待回踩") || p.Action != model.RecActionBuy {
		t.Fatalf("质量等待与模型动作必须独立: %+v", plan)
	}
	raw, _ := json.Marshal(compactScoreBlindForLLM(model.RecTypeShortTerm, []candidate{c}))
	for _, key := range []string{"score_breakdown", "entry_quality", "scoring_comparison", "ranking_score", "preselection"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("盲化泄漏 %s", key)
		}
	}
	if !strings.Contains(string(raw), "signal_quality") || c.FinalCheck.EntryQuality == nil {
		t.Fatal("盲化必须保留真实形态观测且不改变原始快照")
	}
}
