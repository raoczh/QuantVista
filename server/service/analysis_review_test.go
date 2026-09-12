package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestPanelAsyncAttributesOnlyItsBuiltinPrompt(t *testing.T) {
	prompts := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		var text strings.Builder
		for _, message := range req.Messages {
			text.WriteString(message.Content)
		}
		prompts <- text.String()
		content := `{"roles":[{"role":"technical","rating":"neutral","summary":"数据有限"},{"role":"momentum","rating":"neutral","summary":"数据有限"},{"role":"risk","rating":"neutral","summary":"数据有限"},{"role":"contrarian","rating":"neutral","summary":"数据有限"}],"consensus":"本地观点","disagreement":"数据不足"}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 3}})
	}))
	defer srv.Close()
	const userID int64 = 1188
	seedReportEnv(t, userID, srv.URL)
	custom := strings.Repeat("只供标准分析使用的自定义模板标记。", 6)
	if _, _, err := NewPromptService().Upsert(userID, PromptInput{Module: model.AnalysisModuleStock, Content: custom, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	quoteTime := time.Now().In(time.Local)
	previous := quoteTime.AddDate(0, 0, -1)
	if err := common.DB.Create(&model.TradingCalendar{Market: "cn", TradeDate: previous.Format("2006-01-02"), IsOpen: true}).Error; err != nil {
		t.Fatal(err)
	}
	if quoteTime.Hour()*60+quoteTime.Minute() < sessionQuoteReadyMin {
		quoteTime = time.Date(previous.Year(), previous.Month(), previous.Day(), 15, 0, 0, 0, time.Local)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(compareReviewAdapter{quoteTime: quoteTime}))
	svc := NewAnalysisService(market, nil, nil, NewLLMService(), nil)
	view, err := svc.AnalyzeAsync(userID, true, AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "510300", Mode: model.AnalysisModePanel})
	if err != nil {
		t.Fatal(err)
	}
	runs := waitTerminalJobRuns(t, userID, JobResultAnalysis, view.ID, 1)
	if runs[0].Status != model.JobStatusSuccess {
		t.Fatalf("本地 panel 应执行成功：%+v", runs)
	}
	if strings.Contains(<-prompts, custom) {
		t.Fatal("多角色固定编排不应使用标准分析模板")
	}
	var record model.AnalysisRecord
	if err := common.DB.First(&record, view.ID).Error; err != nil {
		t.Fatal(err)
	}
	if record.PromptVersion != analysisPromptVersion || strings.Contains(record.LlmRunJSON, "-custom.") {
		t.Fatalf("未使用自定义模板时，业务与调用归因不能标自定义版本：version=%s manifest=%s", record.PromptVersion, record.LlmRunJSON)
	}
}

func TestTradePlanRepairsRoundedPriceConflicts(t *testing.T) {
	setupTestDB(t)
	for _, tc := range []struct {
		name                    string
		low, high, target, stop float64
	}{
		{"rounded_stop_meets_quote", 10.001, 10.002, 12, 9.999},
		{"rounded_stop_meets_buy", 9.004, 9.006, 12, 9.003},
		{"rounded_target_meets_quote", 9, 9.5, 10.004, 8.5},
		{"rounded_price_becomes_zero", 0.004, 9.5, 12, 0.003},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				plan := tradePlan{BuyLow: tc.low, BuyHigh: tc.high, TargetPrice: tc.target, StopPrice: tc.stop,
					HorizonDays: 10, Checklist: []string{"核对当前行情"}}
				if calls > 1 {
					plan.BuyLow, plan.BuyHigh, plan.TargetPrice, plan.StopPrice = 9, 9.5, 12, 8.5
				}
				content, _ := json.Marshal(plan)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}},
					"usage":   map[string]int{"total_tokens": 3},
				})
			}))
			defer srv.Close()
			result := &AnalysisResult{Rating: model.AnalysisRatingBullish}
			usage, _ := (&AnalysisService{}).attachTradePlan(context.Background(), 0,
				&model.LLMConfig{BaseURL: srv.URL, Model: "review"}, "local", true,
				AnalyzeRequest{Module: model.AnalysisModuleStock},
				map[string]any{"quote": map[string]any{"price": 10.0}, "freshness_status": freshStatusFresh}, result, "", "")
			if calls != 2 || usage.TotalTokens != 6 {
				t.Fatalf("最终两位小数价位违纪必须修复，不可直接交付：calls=%d usage=%+v plan=%+v", calls, usage, result.TradePlan)
			}
			if result.TradePlan == nil || result.TradePlan.StopPrice != 8.5 {
				t.Fatalf("应该接受第二次合法计划：%+v", result.TradePlan)
			}
		})
	}
}

func TestTradePlanDisciplineAtExactTwoToOne(t *testing.T) {
	for _, tc := range []struct{ target, want float64 }{{10.6, 40}, {10.59, 20}} {
		plan := &tradePlan{BuyLow: 10, BuyHigh: 10, StopPrice: 9.7, TargetPrice: tc.target}
		position := &positionAdvice{PositionPct: 40}
		applyPlanDiscipline(plan, position)
		if position.PositionPct != tc.want {
			t.Fatalf("合法两位价格 0.60/0.30 恰好 2:1 不应因浮点尾差降仓：target=%v plan=%+v position=%+v", tc.target, plan, position)
		}
	}
}

func TestETFTradePlanRetainsThreeDecimalTicks(t *testing.T) {
	setupTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := `{"buy_low":4.020,"buy_high":4.025,"target_price":4.060,"stop_price":4.010,"horizon_days":10,"checklist":["核对当前行情"]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 3}})
	}))
	defer srv.Close()
	result := &AnalysisResult{Rating: model.AnalysisRatingBullish}
	(&AnalysisService{}).attachTradePlan(t.Context(), 0, &model.LLMConfig{BaseURL: srv.URL, Model: "review"}, "local", true,
		AnalyzeRequest{Module: model.AnalysisModuleStock, Market: "cn", Symbol: "510300"},
		map[string]any{"quote": map[string]any{"price": 4.037}, "freshness_status": freshStatusFresh}, result, "", "")
	if result.TradePlan == nil || result.TradePlan.BuyHigh != 4.025 {
		t.Fatalf("ETF 的合法三位价格不应按股票两位价格改变：%+v", result.TradePlan)
	}
}

func TestAnalysisRestoresOnlyItsOriginalRequest(t *testing.T) {
	setupTestDB(t)
	record := model.AnalysisRecord{UserID: 955, Module: "stock", Market: "cn", Symbol: "600000",
		Status: model.AnalysisStatusFailed, ErrorCode: "stale_quote"}
	if err := common.DB.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	original := AnalyzeRequest{Module: "stock", Market: "cn", Symbol: "600000", Question: "原始问题", Verify: true, LLMConfigID: 27}
	for _, uid := range []int64{955, 956} {
		req := original
		if uid == 956 {
			req.Question = "另一用户的问题不能泄露"
		}
		raw, _ := json.Marshal(req)
		envelope, _ := json.Marshal(persistedJobSnapshot{Version: jobSnapshotVersion, Kind: JobKindAnalysis, Request: raw})
		run := model.JobRun{UserID: uid, Kind: JobKindAnalysis, Status: model.JobStatusFailed,
			ResultType: JobResultAnalysis, ResultID: &record.ID, RequestSnapshot: string(envelope)}
		if err := common.DB.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
	}
	view, err := (&AnalysisService{}).Get(record.UserID, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Request == nil || *view.Request != original {
		t.Fatalf("应按本人结果恢复完整原参数：%+v", view.Request)
	}
	if _, err := (&AnalysisService{}).Get(956, record.ID); err == nil {
		t.Fatal("他人作业引用不能突破分析记录归属")
	}
}
