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

// TestPromptExtendedModules 新四模块（推荐/日报/问答/复核）可增改、override 读取链生效、版本 -custom 归因。
func TestPromptExtendedModules(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM prompt_templates")
	t.Cleanup(func() { common.DB.Exec("DELETE FROM prompt_templates") })
	svc := &PromptService{}

	for _, m := range []string{model.PromptModuleRecommend, model.PromptModuleDaily, model.PromptModuleQa, model.PromptModuleReview} {
		if _, err := svc.Upsert(7, PromptInput{Module: m, Content: "自定义" + m + "指引 {{market}}", Enabled: true}); err != nil {
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

	// 推荐 buildMessages：系统提示应含自定义角色段而非默认铁律段。
	rec := &RecommendationService{}
	strat := &strategyTemplate{Key: "momentum", Name: "动量突破", guide: "看量价"}
	msgs := rec.buildMessages(7, model.RecTypeShortTerm, strat, "cn", 3, nil, RecFilters{}, nil)
	if !strings.Contains(msgs[0].Content, "自定义recommend指引 cn") || strings.Contains(msgs[0].Content, "铁律") {
		t.Fatalf("推荐系统提示应被自定义模板替换: %.80s", msgs[0].Content)
	}
	// 未自定义用户走默认铁律段。
	msgs = rec.buildMessages(8, model.RecTypeShortTerm, strat, "cn", 3, nil, RecFilters{}, nil)
	if !strings.Contains(msgs[0].Content, "铁律") {
		t.Fatal("无自定义时应回退默认 recRoleIntro")
	}

	// 问答 buildMessages：自定义 qa 角色段 + 版本 -custom。
	qa := &QaService{}
	conv := model.AiConversation{UserID: 7, Symbol: "600519", Name: "贵州茅台", Market: "cn", DataSnapshot: "{}"}
	qmsgs := qa.buildMessages(conv, nil, "怎么看？")
	if !strings.Contains(qmsgs[0].Content, "自定义qa指引") {
		t.Fatalf("问答系统提示应被自定义模板替换")
	}
	if v := qaPromptVersionFor(7); !strings.HasSuffix(v, "-custom") {
		t.Fatalf("启用 qa 模板时版本应带 -custom: %s", v)
	}
	if v := qaPromptVersionFor(8); strings.HasSuffix(v, "-custom") {
		t.Fatalf("未启用不应带 -custom: %s", v)
	}

	// Modules 含新模块且默认值/占位符齐备。
	seen := map[string]PromptModuleInfo{}
	for _, m := range svc.Modules() {
		seen[m.Module] = m
	}
	for _, m := range []string{model.PromptModuleRecommend, model.PromptModuleDaily, model.PromptModuleQa, model.PromptModuleReview} {
		info, ok := seen[m]
		if !ok || info.Default == "" || info.Label == "" || len(info.Placeholders) == 0 {
			t.Fatalf("模块 %s 信息缺失: %+v", m, info)
		}
	}
}
