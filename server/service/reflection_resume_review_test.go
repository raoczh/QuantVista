package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm"
)

func reflectionReviewResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
	})
}

func seedReflectionReviewGeneration(t *testing.T, beforeResponse func() error) model.RecommendationLabel {
	t.Helper()
	setReflectionFlag(t, true)
	cleanReflectionTables(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if beforeResponse != nil {
			if err := beforeResponse(); err != nil {
				t.Errorf("修改本地来源夹具：%v", err)
			}
		}
		reflectionReviewResponse(w, `{"reflections":[{"idx":0,"lesson":"检查放量持续性"}]}`)
	}))
	t.Cleanup(srv.Close)
	seedReflectionAdmin(t, srv.URL)
	seedMaturedBulk(t, 30)
	return seedMaturedLabel(t, 1, "600901", model.RecTypeShortTerm, "momentum", 10, 3, false, false)
}

func TestReflectionRejectsMissingAndAmbiguousIndex(t *testing.T) {
	for _, invalid := range []string{
		`{"lesson":"缺少序号"}`,
		`{"idx":null,"lesson":"空序号"}`,
		`{"idx":0,"lesson":"第一条"},{"idx":0,"lesson":"冲突条目"}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			setReflectionFlag(t, true)
			cleanReflectionTables(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reflectionReviewResponse(w, `{"reflections":[`+invalid+`,{"idx":1,"lesson":"有效条目"}]}`)
			}))
			defer srv.Close()
			seedReflectionAdmin(t, srv.URL)
			var cfg model.LLMConfig
			if err := common.DB.First(&cfg).Error; err != nil {
				t.Fatal(err)
			}
			items, _, err := callReflectionLLM(t.Context(), cfg.UserID, &cfg, "local-test", make([]reflectionCandidate, 2))
			if err != nil || len(items) != 1 || items[1] != "有效条目" {
				t.Fatalf("无序号或重复序号不能绑定首条推荐，合法条目仍须保留：items=%v err=%v", items, err)
			}
		})
	}
}

func TestReflectionPromptDistinguishesMissingBenchmark(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)
	inputs := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("读取本地模型请求：%v", err)
		}
		if len(body.Messages) > 1 {
			inputs <- body.Messages[1].Content
		}
		reflectionReviewResponse(w, `{"reflections":[{"idx":0,"lesson":"基准缺失时只评价绝对收益"}]}`)
	}))
	defer srv.Close()
	seedReflectionAdmin(t, srv.URL)
	var cfg model.LLMConfig
	if err := common.DB.First(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	cands := []reflectionCandidate{{label: model.RecommendationLabel{HasBench: false, AlphaPct: 9}}, {label: model.RecommendationLabel{HasBench: true, AlphaPct: 3}}}
	if _, _, err := callReflectionLLM(t.Context(), cfg.UserID, &cfg, "local-test", cands); err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-inputs:
		if !strings.Contains(input, `"alpha_pct":null`) || !strings.Contains(input, `"has_bench":false`) || !strings.Contains(input, `"alpha_pct":3`) {
			t.Fatalf("缺基准不能把 alpha 零值或旧值当成事实：%s", input)
		}
	default:
		t.Fatal("没有收到本地模型请求")
	}
}

func TestReflectionCommitRechecksConsumedSource(t *testing.T) {
	for _, change := range []string{"delete", "relabel", "reason"} {
		t.Run(change, func(t *testing.T) {
			var label model.RecommendationLabel
			label = seedReflectionReviewGeneration(t, func() error {
				switch change {
				case "delete":
					return common.DB.Delete(&model.Recommendation{}, label.RecommendationID).Error
				case "relabel":
					// 不更新时间戳，证明核对的是已消费的事实，而不只是 updated_at。
					return common.DB.Model(&model.RecommendationLabel{}).Where("id = ?", label.ID).UpdateColumn("net_return_pct", -9).Error
				default:
					return common.DB.Model(&model.Recommendation{}).Where("id = ?", label.RecommendationID).UpdateColumn("summary", "来源理由已订正").Error
				}
			})
			n, err := GenerateRecommendationReflections(t.Context())
			if err != nil || n != 0 {
				t.Errorf("模型调用期间来源变化，应跳过过期教训：n=%d err=%v", n, err)
			}
			var count int64
			if err := common.DB.Model(&model.RecommendationReflection{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("过期来源不能产生反思：count=%d err=%v", count, err)
			}
		})
	}
}

func TestReflectionCommitCancellationAndStorageFailure(t *testing.T) {
	for _, fail := range []string{"cancel", "write_error", "read_error"} {
		t.Run(fail, func(t *testing.T) {
			seedReflectionReviewGeneration(t, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("反思存储故障")
			const callback = "review_reflection_storage_boundary"
			if fail == "read_error" {
				if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "recommendations" {
						tx.AddError(want)
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
			} else {
				if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "recommendation_reflections" {
						if fail == "cancel" {
							cancel()
						} else {
							tx.AddError(want)
						}
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = common.DB.Callback().Create().Remove(callback) })
			}
			n, err := GenerateRecommendationReflections(ctx)
			if fail == "cancel" {
				want = context.Canceled
			}
			if n != 0 || !errors.Is(err, want) {
				t.Errorf("取消或存储故障必须显式返回：n=%d err=%v", n, err)
			}
			var count int64
			if err := common.DB.Model(&model.RecommendationReflection{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("失败提交不得产生反思：count=%d err=%v", count, err)
			}
		})
	}
}

func TestReflectionCandidatesExcludeFutureAndForcedFacts(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)
	valid := seedMaturedLabel(t, 1, "600901", model.RecTypeShortTerm, "momentum", 10, 2, false, false)
	for i, field := range []string{"updated_at", "signal_date", "exit_date", "forced"} {
		row := seedMaturedLabel(t, 1, "600"+padNum(910+i), model.RecTypeShortTerm, "momentum", 10, 3, false, false)
		var value any = time.Now().AddDate(0, 0, 3).Format("2006-01-02")
		if field == "updated_at" {
			value = time.Now().Add(time.Hour)
		} else if field == "forced" {
			value = true
		}
		if err := common.DB.Model(&row).UpdateColumn(field, value).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := loadReflectionCandidates(10)
	if err != nil || len(rows) != 1 || rows[0].label.ID != valid.ID {
		t.Fatalf("未来及强平估价不能进入已结算反思：count=%d err=%v", len(rows), err)
	}
}

func TestReflectionForcedSamplesDoNotMeetThreshold(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		reflectionReviewResponse(w, `{"reflections":[{"idx":0,"lesson":"教训"}]}`)
	}))
	defer srv.Close()
	seedReflectionAdmin(t, srv.URL)
	seedMaturedBulk(t, 29)
	if err := common.DB.Model(&model.RecommendationLabel{}).Where("horizon_days = 5").Update("forced", true).Error; err != nil {
		t.Fatal(err)
	}
	seedMaturedLabel(t, 1, "600901", model.RecTypeShortTerm, "momentum", 10, 2, false, false)
	if n, err := GenerateRecommendationReflections(t.Context()); err != nil || n != 0 || calls.Load() != 0 {
		t.Fatalf("强平估价不能凑足反思样本门槛：n=%d calls=%d err=%v", n, calls.Load(), err)
	}
}

func TestReflectionShadowStorageFailureDoesNotInventZeroTotal(t *testing.T) {
	setReflectionFlag(t, true)
	cleanReflectionTables(t)
	row := model.RecommendationReflection{RecommendationID: 901, UserID: 1, Symbol: "600901", HorizonDays: 10,
		Strategy: "momentum", RecType: model.RecTypeShortTerm, Outcome: "win", Lesson: "历史教训", AvailableFrom: time.Now().Add(-time.Hour)}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	const callback = "review_reflection_count_failure"
	if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_reflections" {
			if _, ok := tx.Statement.Dest.(*int64); ok {
				tx.AddError(errors.New("历史总数读取故障"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = common.DB.Callback().Query().Remove(callback) })
	if got := reflectionShadowJSON(1, model.RecTypeShortTerm, "momentum", []candidate{{Symbol: "600901"}}); got != "" {
		t.Fatalf("查询故障不能发布命中 1 条却候选总数为 0 的快照：%s", got)
	}
}

func TestMySQLReflectionShadowSharesOneSnapshot(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.RecommendationReflection{}, &model.Option{})
	oldFlag, oldLayers := setting.LLMReflectionShadow(), setting.LLMLayeredContext()
	if err := setting.SetLLMReflectionShadow(true); err != nil {
		t.Fatal(err)
	}
	if err := setting.SetLLMLayeredContext(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setting.SetLLMReflectionShadow(oldFlag); _ = setting.SetLLMLayeredContext(oldLayers) })
	row := model.RecommendationReflection{RecommendationID: 901, UserID: 1, Symbol: "600901", HorizonDays: 10,
		Strategy: "momentum", RecType: model.RecTypeShortTerm, Outcome: "win", ReturnPct: 2, Lesson: "历史教训",
		LabelMaturedAt: time.Now().Add(-2 * time.Hour), AvailableFrom: time.Now().Add(-time.Hour)}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	wrote := false
	const callback = "review_reflection_snapshot"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "recommendation_reflections" && !wrote && tx.Error == nil {
			wrote = true
			later := row
			later.ID, later.RecommendationID, later.Symbol = 0, 902, "600902"
			if err := db.Create(&later).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	raw := reflectionShadowJSON(1, model.RecTypeShortTerm, "momentum", []candidate{{Symbol: "600901"}})
	var snap reflectionShadowSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil || !wrote || len(snap.Matched) != 1 || snap.Layers == nil || snap.Layers.CandidatesTotal != 1 || snap.Layers.Tier3Stats == nil || snap.Layers.Tier3Stats.Total != 1 {
		t.Fatalf("同标的、同策略和统计必须共享一个数据库时点：raw=%s wrote=%v err=%v", raw, wrote, err)
	}
}
