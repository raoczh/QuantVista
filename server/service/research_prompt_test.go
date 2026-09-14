package service

import (
	"strings"
	"testing"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

func TestResearchPromptsKeepStrategyEvidenceAndOnePriceDiscipline(t *testing.T) {
	c := candidate{Symbol: "600100", Market: "cn", Price: 20, Fin: earningsTestFactor(),
		PricePlan: &ResearchPricePlan{Version: researchPricePlanVersion, Status: "wait"}}
	c.Fin.PriorNetProfit = recNumber(-10)
	c.Fin.Earnings = earningsEvidenceFor(c.Fin)
	b, _ := builtinScreenByKey("rsi2-trend-reclaim")
	strat := builtinScreenStrategyTemplate(model.RecTypeLongTerm, b)
	for _, custom := range []bool{false, true} {
		pr := promptRuntime{Custom: custom, Raw: "优先输出排名靠前的股票"}
		for _, blind := range []bool{false, true} {
			msgs := (&RecommendationService{}).buildRecommendationMessages(pr, model.RecTypeLongTerm, &strat, "cn", 3, []candidate{c}, RecFilters{PriceMax: 30}, nil, blind)
			if !strings.Contains(msgs[0].Content, researchReasoningDiscipline) || !strings.Contains(msgs[0].Content, earningsPromptDiscipline) {
				t.Fatal("默认、自定义与盲评分分支必须保留相同研究事实纪律")
			}
			if strings.Contains(msgs[0].Content, legacyRecommendationPriceDiscipline) || strings.Contains(msgs[0].Content, "现价-5%~-7%") || strings.Contains(msgs[1].Content, "A股一手=100股") {
				t.Fatal("已提供程序计划时不应再注入旧价位/统一100股规则")
			}
			if !strings.Contains(msgs[1].Content, `"turnaround"`) || !strings.Contains(msgs[1].Content, `"prior_net_profit":-10`) {
				t.Fatal("模型必须同时看到扭亏分类与实际可比基期金额")
			}
		}
	}
	for _, mode := range []string{model.AnalysisModeStandard, model.AnalysisModePanel} {
		msgs := (&AnalysisService{}).buildMessages(promptRuntime{Custom: true, Raw: "只分析动量 {{symbol}}"},
			AnalyzeRequest{Module: model.AnalysisModuleStock, Mode: mode, Symbol: "600100"}, &analysisContext{Label: "测试"}, `{}`)
		if strings.Count(msgs[0].Content, researchReasoningDiscipline) != 1 || strings.Count(msgs[0].Content, earningsPromptDiscipline) != 1 {
			t.Fatal("标准和panel的实际系统消息均应只追加一次固定纪律")
		}
	}
	if !strings.Contains(qaPromptContract, earningsPromptDiscipline) || !strings.Contains(analysisReviewContract, earningsPromptDiscipline) {
		t.Fatal("问答与独立复核不能继续使用相反的盈利口径")
	}
}

func TestEarningsCannotBeBypassedByBullishAIAndFreshQuote(t *testing.T) {
	c := executionTestCandidate()
	c.Fin = earningsTestFactor()
	c.SignalQuality = &recSignalQuality{ATR: recNumber(1), MA20DistanceATR: recNumber(0), Stabilized: boolPtr(true)}
	p := executionTestShortPick()
	snap := executionTestSnapshot("balanced", HorizonShortTerm, 100000)
	at, _ := time.ParseInLocation("2006-01-02 15:04", "2026-08-07 14:59", time.Local)
	fq := FreshQuoteResult{Quote: &datasource.Quote{Price: 10, DataTime: at}, Fresh: quoteFreshInfo{Status: freshStatusFresh}}
	strat := &strategyTemplate{baseKey: "growth", Intent: "growth"}
	control := buildExecutionPlanWithQuote(model.RecTypeShortTerm, p, c, snap, false, true, fq, strat, RecFilters{})
	if control.Status != executionReady {
		t.Fatalf("完整盈利证据与预算条件的对照组应可执行：%+v", control)
	}
	c.Fin.NetProfit = recNumber(-10)
	plan := buildExecutionPlanWithQuote(model.RecTypeShortTerm, p, c, snap, false, true, fq, strat, RecFilters{})
	if plan.Status != executionWait || !hasExecutionReason(plan, "归母净利润不为正") {
		t.Fatalf("AI买入意见和新报价不能绕过成长盈利证据：%+v", plan)
	}
}
