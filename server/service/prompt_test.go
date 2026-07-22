package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestPromptUpsertAndOverride upsert 唯一/校验 + userPromptOverride 生效条件 + 隔离 + 删除。
func TestPromptUpsertAndOverride(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM prompt_templates")
	svc := &PromptService{}

	// 非法模块 / 空内容。
	if _, _, err := svc.Upsert(1, PromptInput{Module: "unknown", Content: "x"}); err == nil {
		t.Fatalf("非法模块应报错")
	}
	if _, _, err := svc.Upsert(1, PromptInput{Module: model.AnalysisModuleStock, Content: "   "}); err == nil {
		t.Fatalf("空内容应报错")
	}

	// 创建（未启用）→ override 应为空（回退默认）。
	tpl, _, err := svc.Upsert(1, PromptInput{Module: model.AnalysisModuleStock, Content: "只看均线与量能。", Enabled: false})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if userPromptOverride(1, model.AnalysisModuleStock) != "" {
		t.Fatalf("未启用时 override 应为空")
	}

	// 更新为启用（同模块应 upsert 而非新增）→ override 生效。
	if _, _, err := svc.Upsert(1, PromptInput{Module: model.AnalysisModuleStock, Content: "只看均线与量能。", Enabled: true}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	var cnt int64
	common.DB.Model(&model.PromptTemplate{}).Where("user_id = ? AND module = ?", 1, model.AnalysisModuleStock).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("同用户同模块应唯一，得到 %d", cnt)
	}
	if got := userPromptOverride(1, model.AnalysisModuleStock); got != "只看均线与量能。" {
		t.Fatalf("启用后 override 应生效，得到 %q", got)
	}

	// analysisSystemPrompt 应包含自定义内容 + 通用身份与输出规范。
	sys := analysisSystemPrompt(1, model.AnalysisModuleStock, nil)
	if !containsStr(sys, "只看均线与量能。") || !containsStr(sys, "证券研究助理") {
		t.Fatalf("系统提示应含自定义指引与通用身份")
	}
	// 其它用户不受影响（回退默认，默认含"个股"字样）。
	if userPromptOverride(2, model.AnalysisModuleStock) != "" {
		t.Fatalf("用户2 不应有 override")
	}

	// 删除 → 恢复默认（override 空）。
	if err := svc.Delete(2, tpl.ID); err == nil {
		t.Fatalf("跨用户删除应失败")
	}
	if err := svc.Delete(1, tpl.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if userPromptOverride(1, model.AnalysisModuleStock) != "" {
		t.Fatalf("删除后应回退默认")
	}

	// Modules 返回 9 个模块（5 分析 + 推荐/日报/问答/复核）且带默认指引。
	mods := svc.Modules()
	if len(mods) != 9 {
		t.Fatalf("应有 9 个模块，得到 %d", len(mods))
	}
	for _, m := range mods {
		if m.Default == "" || m.Label == "" {
			t.Fatalf("模块信息缺失: %+v", m)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
