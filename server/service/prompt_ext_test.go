package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestRenderPromptTemplate 占位符宽容渲染：已知替换、未知保留、允许空白、非占位符花括号不动。
func TestRenderPromptTemplate(t *testing.T) {
	vars := map[string]string{"symbol": "600519", "market": "cn"}
	cases := []struct{ in, want string }{
		{"分析 {{symbol}} 于 {{market}} 市场", "分析 600519 于 cn 市场"},
		{"{{ symbol }} 带空白也替换", "600519 带空白也替换"},
		{"未知 {{unknown}} 原样保留", "未知 {{unknown}} 原样保留"},
		{"单花括号 {symbol} 不是占位符", "单花括号 {symbol} 不是占位符"},
		{"JSON 示例 {\"a\":1} 不受影响", "JSON 示例 {\"a\":1} 不受影响"},
		{"", ""},
	}
	for _, c := range cases {
		if got := renderPromptTemplate(c.in, vars); got != c.want {
			t.Fatalf("render(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// vars 为空时原样返回。
	if got := renderPromptTemplate("{{symbol}}", nil); got != "{{symbol}}" {
		t.Fatalf("空 vars 应原样: %q", got)
	}
}

// TestPromptExtendedModules 新四模块（推荐/日报/问答/复核）可增改、override 读取链生效、
// 版本 -custom.<hash8> 归因。P0-6：自定义是 L3 任务段，模块契约（铁律/要求/schema）恒由
// 系统追加不可覆盖——断言语义与整段替换时代相反。
func TestPromptExtendedModules(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM prompt_templates")
	common.DB.Exec("DELETE FROM prompt_template_revisions")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM prompt_templates")
		common.DB.Exec("DELETE FROM prompt_template_revisions")
	})
	svc := &PromptService{}

	for _, m := range []string{model.PromptModuleRecommend, model.PromptModuleDaily, model.PromptModuleQa, model.PromptModuleReview} {
		if _, _, err := svc.Upsert(7, PromptInput{Module: m, Content: "自定义" + m + "指引 {{market}}", Enabled: true}); err != nil {
			t.Fatalf("新模块 %s upsert 失败: %v", m, err)
		}
	}

	// promptOverrideFor 渲染占位符。
	got, ok := promptOverrideFor(7, model.PromptModuleRecommend, map[string]string{"market": "cn"})
	if !ok || got != "自定义recommend指引 cn" {
		t.Fatalf("recommend override = %q ok=%v", got, ok)
	}
	// 未提供 vars 的占位符保留原样（宽容）。
	got, _ = promptOverrideFor(7, model.PromptModuleDaily, nil)
	if !strings.Contains(got, "{{market}}") {
		t.Fatalf("未提供值的占位符应保留: %q", got)
	}
	// 其他用户不受影响。
	if _, ok := promptOverrideFor(8, model.PromptModuleRecommend, nil); ok {
		t.Fatal("用户8 不应有 override")
	}

	// 推荐 buildMessages：自定义任务段生效，且铁律契约段仍被系统追加（P0-6 不可覆盖）。
	rec := &RecommendationService{}
	strat := &strategyTemplate{Key: "momentum", Name: "动量突破", guide: "看量价"}
	msgs := rec.buildMessages(7, model.RecTypeShortTerm, strat, "cn", 3, nil, RecFilters{}, nil)
	if !strings.Contains(msgs[0].Content, "自定义recommend指引 cn") {
		t.Fatalf("推荐系统提示应含自定义任务段: %.80s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "铁律") || !strings.Contains(msgs[0].Content, promptContractHeader) {
		t.Fatalf("自定义时铁律契约段应被系统追加（不可覆盖）")
	}
	// 未自定义用户走默认整段（无契约分界头）。
	msgs = rec.buildMessages(8, model.RecTypeShortTerm, strat, "cn", 3, nil, RecFilters{}, nil)
	if !strings.Contains(msgs[0].Content, "铁律") {
		t.Fatal("无自定义时应回退默认 recRoleIntro")
	}
	if strings.Contains(msgs[0].Content, promptContractHeader) {
		t.Fatal("默认路径不应出现契约分界头（逐字节等于拆分前）")
	}

	// 问答 buildMessages：自定义 qa 任务段 + 要求契约段恒在 + 版本 -custom.<hash8>。
	qa := &QaService{}
	conv := model.AiConversation{UserID: 7, Symbol: "600519", Name: "贵州茅台", Market: "cn", DataSnapshot: "{}"}
	qmsgs := qa.buildMessages(conv, nil, "怎么看？")
	if !strings.Contains(qmsgs[0].Content, "自定义qa指引") {
		t.Fatalf("问答系统提示应被自定义任务段替换")
	}
	if !strings.Contains(qmsgs[0].Content, "要求：") || !strings.Contains(qmsgs[0].Content, promptContractHeader) {
		t.Fatalf("自定义时 qa 要求契约段应被系统追加")
	}
	v := qaPromptVersionFor(7)
	if !strings.Contains(v, "-custom.") || len(v) != len(qaPromptVersion)+len("-custom.")+8 {
		t.Fatalf("启用 qa 模板时版本应带 -custom.<hash8>: %s", v)
	}
	if v2 := qaPromptVersionFor(8); strings.Contains(v2, "-custom") {
		t.Fatalf("未启用不应带 -custom: %s", v2)
	}

	// Modules 含新模块且默认值（任务段）/契约段/占位符齐备。
	seen := map[string]PromptModuleInfo{}
	for _, m := range svc.Modules() {
		seen[m.Module] = m
	}
	for _, m := range []string{model.PromptModuleRecommend, model.PromptModuleDaily, model.PromptModuleQa, model.PromptModuleReview} {
		info, ok := seen[m]
		if !ok || info.Default == "" || info.Label == "" || len(info.Placeholders) == 0 {
			t.Fatalf("模块 %s 信息缺失: %+v", m, info)
		}
		if info.Contract == "" {
			t.Fatalf("分层模块 %s 应返回系统契约段供前端展示", m)
		}
		if strings.Contains(info.Default, "只输出") || strings.Contains(info.Default, "schema") {
			t.Fatalf("模块 %s 的默认任务段不应含输出契约（会诱导用户拷贝后与系统追加段重复）: %.60s", m, info.Default)
		}
	}
}
