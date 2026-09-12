package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestPromptMutationPreservesBaselineAfterMigrationFailure(t *testing.T) {
	setupTestDB(t)
	for i, action := range []string{"edit", "delete", "unchanged"} {
		t.Run(action, func(t *testing.T) {
			legacy := model.PromptTemplate{UserID: int64(1090 + i), Module: model.PromptModuleRecommend, Content: "升级前的研究纪律", Enabled: true}
			if err := common.DB.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			const callback = "review_prompt_baseline_migration_failure"
			failed := false
			if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "prompt_template_revisions" {
					failed = true
					tx.AddError(errors.New("本地注入基线迁移失败"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			_ = model.MigratePromptTemplateBaselines()
			common.DB.Callback().Create().Remove(callback)
			if !failed {
				t.Fatal("测试未触发基线迁移故障")
			}
			svc := NewPromptService()
			var err error
			if action == "delete" {
				err = svc.Delete(legacy.UserID, legacy.ID)
			} else {
				content := "新的研究纪律"
				if action == "unchanged" {
					content = legacy.Content
				}
				_, _, err = svc.Upsert(legacy.UserID, PromptInput{Module: legacy.Module, Content: content, Enabled: true})
			}
			if err != nil {
				t.Fatal(err)
			}
			var history []model.PromptTemplateRevision
			if err := common.DB.Where("template_id = ?", legacy.ID).Order("revision").Find(&history).Error; err != nil {
				t.Fatal(err)
			}
			if len(history) == 0 || history[0].Content != legacy.Content || history[0].ContentHash != model.PromptContentHash(legacy.Content) {
				t.Fatalf("迁移失败后的首次 %s 不能永久丢失旧模板原文：%+v", action, history)
			}
			if action == "edit" && (len(history) != 2 || history[1].Revision <= history[0].Revision) {
				t.Fatalf("修改后应同时保留旧基线和新内容版本：%+v", history)
			}
		})
	}
}

func TestPromptMutationFailsAtomicallyWhenBaselineCannotBeSaved(t *testing.T) {
	setupTestDB(t)
	for i, action := range []string{"edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			legacy := model.PromptTemplate{UserID: int64(1094 + i), Module: model.PromptModuleRecommend, Content: "必须保留的原文", Enabled: true}
			if err := common.DB.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			const callback = "review_prompt_old_baseline_write_failure"
			injected := errors.New("本地注入旧基线保存失败")
			if err := common.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				row, ok := tx.Statement.Dest.(*model.PromptTemplateRevision)
				if ok && row.TemplateID == legacy.ID && row.Content == legacy.Content {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Create().Remove(callback) })
			var err error
			if action == "delete" {
				err = NewPromptService().Delete(legacy.UserID, legacy.ID)
			} else {
				_, _, err = NewPromptService().Upsert(legacy.UserID, PromptInput{Module: legacy.Module, Content: "替换后的正文", Enabled: true})
			}
			if !errors.Is(err, injected) {
				t.Errorf("旧基线保存失败必须中止 %s：%v", action, err)
			}
			var stored model.PromptTemplate
			if err := common.DB.First(&stored, legacy.ID).Error; err != nil || stored.Content != legacy.Content || stored.Revision != legacy.Revision {
				t.Fatalf("失败操作不能删除或修改原模板：stored=%+v err=%v", stored, err)
			}
		})
	}
}

func TestPromptBaselineMigrationRechecksTemplateAfterInitialScan(t *testing.T) {
	setupTestDB(t)
	legacy := model.PromptTemplate{UserID: 1099, Module: model.PromptModuleRecommend, Content: "迁移开始时的旧正文", Enabled: true}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	const callback = "review_prompt_migration_interleaved_edit"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]model.PromptTemplate); !ok || changed {
			return
		}
		changed = true
		if _, _, err := NewPromptService().Upsert(legacy.UserID, PromptInput{Module: legacy.Module, Content: "另一实例已保存的新正文", Enabled: true}); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	if err := model.MigratePromptTemplateBaselines(); err != nil {
		t.Fatal(err)
	}
	var stored model.PromptTemplate
	if err := common.DB.First(&stored, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !changed || stored.Content != "另一实例已保存的新正文" || stored.ContentHash != model.PromptContentHash(stored.Content) || stored.Revision != 2 {
		t.Fatalf("旧迁移扫描结果不能把新模板重新标为旧 hash 和版本：%+v changed=%v", stored, changed)
	}
}

func TestPromptReadFailureDoesNotSilentlyUseDefaults(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"summary":"本地测试","verdict":"pass","comment":"本地测试","confidence":50}`}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 1}})
	}))
	t.Cleanup(upstream.Close)
	const userID int64 = 1100
	seedReportEnv(t, userID, upstream.URL)
	llm := NewLLMService()
	cfg, apiKey, err := llm.ResolveForUse(userID, 0)
	if err != nil {
		t.Fatal(err)
	}
	conv := model.AiConversation{UserID: userID, Symbol: "600000", Market: "cn", DataSnapshot: `{}`}
	if err := common.DB.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"analysis", "recommend", "qa", "daily", "review"} {
		t.Run(action, func(t *testing.T) {
			module := action
			if action == "analysis" {
				module = model.AnalysisModuleStock
			}
			if _, _, err := NewPromptService().Upsert(userID, PromptInput{Module: module, Content: "必须使用已保存的研究纪律", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("本地注入模板读取失败")
			failed := false
			const callback = "review_prompt_runtime_read_failure"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "prompt_templates" {
					failed = true
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			before := calls.Load()
			var err error
			switch action {
			case "analysis":
				_, err = (&AnalysisService{llm: llm}).prepareAnalysis(userID, true, AnalyzeRequest{Module: model.AnalysisModuleStock, Symbol: "600000", Market: "cn"})
			case "recommend":
				_, err = (&RecommendationService{llm: llm}).prepareGeneration(userID, true, RecommendRequest{Type: model.RecTypeShortTerm}, true)
			case "qa":
				_, err = NewQaService(nil, llm).prepareAsk(t.Context(), userID, QaAskRequest{ConversationID: conv.ID, Question: "检查风险"})
			case "daily":
				_, _, _, err = (&DailyReportService{}).callReview(t.Context(), userID, "2026-09-08", loadPromptRuntime(userID, module), cfg, apiKey, true, `{}`, "local-prompt-audit")
			case "review":
				review, _, run := (&AnalysisService{}).reviewAnalysis(t.Context(), userID, cfg, apiKey, true, model.AnalysisModuleStock, map[string]any{}, &AnalysisResult{}, "local-prompt-audit", "")
				if review != nil || run == nil || run.DegradedReason == "" {
					t.Errorf("读取失败的复核不能给出默认模板下的通过结论：review=%+v run=%+v", review, run)
				}
			}
			if action != "review" && !errors.Is(err, injected) {
				t.Errorf("%s 模板读取失败必须传递原错误，不能按默认模板继续：%v", action, err)
			}
			if !failed || calls.Load() != before {
				t.Fatalf("模板不可读时不得额外请求模型：injected=%v calls=%d", failed, calls.Load()-before)
			}
		})
	}
}

func TestPromptRuntimeAttributesActualContentDespiteStaleMetadata(t *testing.T) {
	setupTestDB(t)
	row := model.PromptTemplate{UserID: 1102, Module: model.PromptModuleRecommend, Content: "实际使用的正文", ContentHash: model.PromptContentHash("错误的旧正文"), Revision: 1, Enabled: true}
	if err := common.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	runtime := loadPromptRuntime(row.UserID, row.Module)
	if runtime.ReadError != nil || runtime.Hash != model.PromptContentHash(row.Content) {
		t.Fatalf("运行归因必须以实际原文为准：%+v", runtime)
	}
	var stored model.PromptTemplate
	if err := common.DB.First(&stored, row.ID).Error; err != nil || stored.ContentHash != row.ContentHash {
		t.Fatalf("读取不得顺便重写存量元数据：%+v err=%v", stored, err)
	}
}

func TestPromptBaselineMigrationRepairsRevisionPointingToOtherContent(t *testing.T) {
	setupTestDB(t)
	oldContent, currentContent := "旧版本正文", "当前版本正文"
	tpl := model.PromptTemplate{UserID: 1103, Module: model.PromptModuleRecommend, Content: currentContent,
		ContentHash: model.PromptContentHash(oldContent), Revision: 1, Enabled: true}
	if err := common.DB.Create(&tpl).Error; err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{oldContent, currentContent} {
		if err := common.DB.Create(&model.PromptTemplateRevision{TemplateID: tpl.ID, UserID: tpl.UserID, Module: tpl.Module,
			Revision: i + 1, Content: content, ContentHash: model.PromptContentHash(content)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := model.MigratePromptTemplateBaselines(); err != nil {
		t.Fatal(err)
	}
	var stored model.PromptTemplate
	if err := common.DB.First(&stored, tpl.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 2 || stored.ContentHash != model.PromptContentHash(currentContent) {
		t.Fatalf("迁移不能只修 hash 而继续指向其他正文的 revision：%+v", stored)
	}
	var history []model.PromptTemplateRevision
	if err := common.DB.Where("template_id = ?", tpl.ID).Order("revision").Find(&history).Error; err != nil || len(history) != 2 || history[0].Content != oldContent || history[1].Content != currentContent {
		t.Fatalf("历史快照必须保留原样：%+v err=%v", history, err)
	}
}
