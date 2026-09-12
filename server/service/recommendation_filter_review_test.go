package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestRecommendationFilterReadFailureStopsGeneration(t *testing.T) {
	for _, phase := range []string{"default_filters", "candidate_filter"} {
		t.Run(phase, func(t *testing.T) {
			const userID int64 = 8827
			seedRecEnv(t, userID, "http://127.0.0.1:1", 0)
			failure := errors.New("本机偏好读取故障")
			const callback = "review_recommendation_filter_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "user_preferences" && (phase == "candidate_filter" || len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "rec_filters_json") {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			market := NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{}))
			svc := NewRecommendationService(market, NewWatchlistService(market), NewLLMService())
			var err error
			if phase == "default_filters" {
				_, err = svc.prepareGeneration(userID, true, RecommendRequest{Type: model.RecTypeShortTerm}, true)
			} else {
				strategy, resolveErr := resolveRecStrategy(userID, model.RecTypeShortTerm, "momentum")
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				_, _, err = svc.buildPool(context.Background(), userID, "us", model.RecTypeShortTerm, strategy, RecFilters{})
			}
			if !errors.Is(err, failure) {
				t.Errorf("读取用户硬约束失败后仍按默认过滤继续: %v", err)
			}
		})
	}
}

func TestRecommendationInvalidSavedFiltersDoNotBecomeDefaults(t *testing.T) {
	for _, raw := range []string{"{broken", "null"} {
		t.Run(raw, func(t *testing.T) {
			const userID int64 = 8828
			seedRecEnv(t, userID, "http://127.0.0.1:1", 0)
			if err := common.DB.Create(&model.UserPreference{UserID: userID, RecFiltersJSON: raw}).Error; err != nil {
				t.Fatal(err)
			}
			svc := NewRecommendationService(nil, nil, NewLLMService())
			if _, err := svc.prepareGeneration(userID, true, RecommendRequest{Type: model.RecTypeShortTerm}, true); err == nil {
				t.Error("损坏的存量默认筛选被当成可用默认值")
			}
			if _, err := svc.prepareGeneration(userID, true, RecommendRequest{Type: model.RecTypeShortTerm, Filters: &RecFilters{PriceMax: 12}}, true); err != nil {
				t.Errorf("显式本次筛选仍应覆盖坏的存量默认值: %v", err)
			}
		})
	}
}

func TestRecommendationInvalidSavedBlacklistStopsPool(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.Create(&model.UserPreference{UserID: 8829, BlacklistJSON: "{broken"}).Error; err != nil {
		t.Fatal(err)
	}
	market := NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{}))
	svc := NewRecommendationService(market, NewWatchlistService(market), nil)
	strategy, err := resolveRecStrategy(8829, model.RecTypeShortTerm, "momentum")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.buildPool(context.Background(), 8829, "us", model.RecTypeShortTerm, strategy, RecFilters{}); err == nil {
		t.Error("损坏的黑名单被当成没有黑名单")
	}
}

func TestRecommendationRequestedFiltersNeedKnownValues(t *testing.T) {
	candidate := candidate{Symbol: "600000", Name: "本地样本", Price: 10}
	for _, filters := range []RecFilters{{FloatCapMinYi: 30}, {FloatCapMaxYi: 200}, {TurnoverMin: 1}, {TurnoverMax: 10}} {
		if reason := applyQuoteFilters(candidate, filters); reason == "" {
			t.Errorf("无法核验的市值/换手率被当成满足显式筛选: %+v", filters)
		}
	}
	if reason := applyQuoteFilters(candidate, RecFilters{}); reason != "" {
		t.Errorf("未设置约束时不应额外要求这些字段: %s", reason)
	}
}

func TestRecommendationReviewUsesOnlySelectedCandidates(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "only_unselected", true: "mixed"}[mixed], func(t *testing.T) {
			setupTestDB(t)
			reviews := []map[string]any{{"symbol": "000001", "verdict": "pass", "confidence": 90}}
			if mixed {
				reviews = append(reviews, map[string]any{"symbol": "600000", "verdict": "warn", "confidence": 60})
			}
			content, err := json.Marshal(map[string]any{"reviews": reviews})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}}})
			}))
			defer srv.Close()
			cfg := &model.LLMConfig{BaseURL: srv.URL, Model: "local-review", MaxTokens: 500}
			selected := []recPick{{Symbol: "600000", Action: model.RecActionBuy, Confidence: 70}}
			got, _, _, _ := (&RecommendationService{}).reviewPicks(t.Context(), 8833, cfg, "local", true, model.RecTypeShortTerm, selected, testPool(), "local-review", "")
			want := 0
			if mixed {
				want = 1
			}
			if len(got) != want || len(got) > 0 && got[0].Symbol != "600000" {
				t.Errorf("未入选、未提供给复核员的候选被当成有效复核: %+v", got)
			}
		})
	}
}
