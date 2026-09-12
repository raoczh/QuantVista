package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestRecommendationProfilesCoverCatalog(t *testing.T) {
	if len(builtinRecommendationProfiles) != len(builtinScreens) || len(retailRecommendationProfiles) != len(retailTemplates) {
		t.Fatal("评分配置必须与策略目录一一对应")
	}
	for _, recType := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		for _, builtin := range builtinScreens {
			profile, ok := builtinRecommendationProfiles[builtin.Key]
			got := builtinScreenStrategyTemplate(recType, builtin)
			if !ok || profile.intent == "" || !model.ValidStrategyScoreProfile(got.baseKey) || got.ScoreProfile != got.baseKey || got.Intent != profile.intent {
				t.Fatalf("%s/%s 配置缺失或不一致: %+v", builtin.Key, recType, got)
			}
		}
		for _, template := range retailTemplates {
			profile, ok := retailRecommendationProfiles[template.Key]
			got := retailTemplateStrategyTemplate(recType, template)
			if !ok || profile.intent == "" || !model.ValidStrategyScoreProfile(got.baseKey) || got.ScoreProfile != got.baseKey || got.Intent != profile.intent {
				t.Fatalf("%s/%s 模板配置缺失或不一致: %+v", template.Key, recType, got)
			}
		}
	}
}

func TestRecommendationProfileIndependentOfHoldingPeriod(t *testing.T) {
	for _, profile := range []string{"balanced", "momentum", "pullback", "active", "value", "growth", "leader"} {
		st, sm, sp, sv, sr := strategyDimWeights(model.RecTypeShortTerm, profile)
		lt, lm, lp, lv, lr := strategyDimWeights(model.RecTypeLongTerm, profile)
		if st != lt || sm != lm || sp != lp || sv != lv || sr != lr {
			t.Fatalf("%s 的维度权重不应被持有周期替换", profile)
		}
		wantFinance := profile == "value" || profile == "growth" || profile == "leader"
		if profileUsesFinance(profile) != wantFinance {
			t.Fatalf("%s 的财务需求不匹配", profile)
		}
	}
	// 改展示周期不改变形态的显式映射；未知自建条件也不凭周期推测成突破或价值。
	for _, period := range []string{"short", "swing", "mid"} {
		if screenBaseKey(model.RecTypeShortTerm, period, "macd-gold-water") != "momentum" || screenBaseKey(model.RecTypeLongTerm, period, "") != "balanced" {
			t.Fatalf("不应按 %s 猜测策略风格", period)
		}
	}
	// 短线选择价值配置时仍应执行估值分支。
	c := candidate{PETTM: 10, PB: 1}
	f := &candFactors{Pos60: 60}
	short, _ := strategyAdjust(model.RecTypeShortTerm, "value", c, f)
	long, _ := strategyAdjust(model.RecTypeLongTerm, "value", c, f)
	if short != 13 || short != long {
		t.Fatalf("估值配置应独立执行: short=%v long=%v", short, long)
	}
}

func TestRecommendationProfileRevisionAndQueuedSnapshot(t *testing.T) {
	setupTestDB(t)
	cleanScreenerStrategies(t)
	svc := NewScreenerService()
	tree := allOf(leafV("close", ">", 5))
	value, momentum := "value", "momentum"
	req := SaveStrategyRequest{Name: "评分版本", Period: "short", Risk: "mid", Tree: &tree, ScoreProfile: &value}
	v1, err := svc.SaveStrategy(7, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID, req.BaseRevisionID, req.ScoreProfile = v1.ID, v1.CurrentRevisionID, nil
	same, err := svc.SaveStrategy(7, req)
	if err != nil || same.CurrentRevisionID != v1.CurrentRevisionID || same.ScoreProfile != value {
		t.Fatalf("旧客户端省略评分字段必须保留原配置: %+v %v", same, err)
	}
	key := fmt.Sprintf("screen:u%d", v1.ID)
	frozen, err := resolveRecStrategyRevision(7, model.RecTypeShortTerm, key, v1.CurrentRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	req.ScoreProfile = &momentum
	v2, err := svc.SaveStrategy(7, req)
	if err != nil || v2.Revision != 2 || v2.ContentHash == v1.ContentHash {
		t.Fatalf("仅改评分也必须形成不同版本: %+v %v", v2, err)
	}
	if err := svc.DeleteStrategy(7, v1.ID); err != nil {
		t.Fatal(err)
	}
	for _, recType := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		queued, err := resolveRecStrategyRevision(7, recType, key, v1.CurrentRevisionID)
		if err != nil || queued.baseKey != value || queued.StrategyRevisionID != v1.CurrentRevisionID || frozen.baseKey != value {
			t.Fatalf("编辑或归档后排队任务仍使用冻结评分配置: %+v %v", queued, err)
		}
	}
	if _, err := resolveRecStrategyRevision(8, model.RecTypeShortTerm, key, v1.CurrentRevisionID); err == nil {
		t.Fatal("不能读取其他用户策略配置")
	}
	history, err := svc.StrategyHistory(7, v1.ID)
	if err != nil || len(history.Revisions) != 2 || history.Revisions[0].ScoreProfile != momentum || history.Revisions[1].ScoreProfile != value {
		t.Fatalf("历史必须携带各自的评分配置: %+v %v", history, err)
	}
	bad := "invented"
	if _, err := svc.SaveStrategy(7, SaveStrategyRequest{Name: "无效评分", Tree: &tree, ScoreProfile: &bad}); err == nil {
		t.Fatal("非法评分请求应拒绝")
	}
	corrupt := model.ScreenerStrategyRevision{UserID: 7, StrategyID: v1.ID, Revision: 3, Name: "坏快照", TreeJSON: `{"factor":"close","op":">","value":5}`, ScoreProfile: bad}
	if err := common.DB.Create(&corrupt).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRecStrategyRevision(7, model.RecTypeShortTerm, key, corrupt.ID); err == nil {
		t.Fatal("显式指定损坏历史版本也必须拒绝")
	}
}

func TestRecommendationPreheatUsesProfileFinance(t *testing.T) {
	setupTestDB(t)
	resetRecommendationPreheatState(t)
	oldF10 := fetchF10
	t.Cleanup(func() { fetchF10 = oldF10 })
	finCalls := 0
	fetchF10 = func(context.Context, string) ([]datasource.DcRow, error) { finCalls++; return nil, nil }
	svc := &RecommendationService{em: datasource.NewEastMoneyAdapter()}
	pool := []candidate{{Symbol: "600211", Market: "cn"}}
	bases := []recPreheatCandidate{{Idx: 0, Symbol: "600211", BaseScore: 70}}
	finBudget, flowBudget := 1, 0
	got := svc.preheatRecommendationRound(context.Background(), profileUsesFinance("pullback"), pool, bases, &finBudget, &flowBudget, time.Now())
	if finCalls != 0 || finBudget != 1 || len(got.FinanceAvailable) != 0 {
		t.Fatal("非财务评分不应消耗财务预算或声明财务缺失")
	}
	got = svc.preheatRecommendationRound(context.Background(), profileUsesFinance("value"), pool, bases, &finBudget, &flowBudget, time.Now())
	if finCalls == 0 || finBudget != 0 || len(got.FinFetchSymbols) != 1 {
		t.Fatalf("财务型配置应富化财务: calls=%d budget=%d got=%+v", finCalls, finBudget, got)
	}
}
