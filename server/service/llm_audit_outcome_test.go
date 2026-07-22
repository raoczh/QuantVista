package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// P0-8 修复批 audit outcome 测试：完整性门禁拒收（Chat length / Responses incomplete /
// 流式截断终态）时——业务侧必须收到失败（错误+机读码），审计侧必须保留上游真实结果
//（ResponseBody=真实正文、usage、finish_state_raw 原始终态、ErrorMsg 独立保存错误），
// run.record 也能拿到真实 raw state。两组断言（业务失败 + 审计完整）缺一不可。

// auditRowOf 取指定模块最新一条审计行。
func auditRowOf(t *testing.T, module string) model.LLMCallLog {
	t.Helper()
	var row model.LLMCallLog
	if err := common.DB.Where("module = ?", module).Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("查审计行失败(module=%s): %v", module, err)
	}
	return row
}

// TestChatLengthRejectKeepsAudit Chat 非流式 finish_reason=length：拒收（业务失败）+
// 审计保留半截正文/usage/原始终态 + run 元数据拿到真实 raw state。
func TestChatLengthRejectKeepsAudit(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"半截正文内容"},"finish_reason":"length"}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`))
	}))
	defer srv.Close()

	run := newLLMRun("t-audit", "", "analysis", "analysis.v1", "p17")
	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     run.chatMeta(1, nil, 1),
	})
	run.record(res, err)

	// 业务失败：错误非空且带机读码，半截正文绝不能被当成功消费。
	if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("length 截断必须业务失败: %v (code=%q)", err, RefusalCodeOf(err))
	}
	// audit outcome：res 带出真实上游结果。
	if res == nil || res.Content != "半截正文内容" || res.FinishReason != "length" || res.Usage.TotalTokens != 33 {
		t.Fatalf("拒收结果应带真实正文/usage/终态: %+v", res)
	}
	// run 元数据：raw state 是上游真值而非文案还原。
	if run.FinishStateRaw != "length" || run.FinishState != "length" {
		t.Fatalf("run 应记录真实终态: raw=%q state=%q", run.FinishStateRaw, run.FinishState)
	}
	// 审计行：错误状态下仍保留真实 ResponseBody/usage/finish_state_raw，ErrorMsg 独立。
	row := auditRowOf(t, "analysis")
	if row.Status != model.LLMCallStatusError || row.ErrorMsg == "" {
		t.Fatalf("审计应记错误状态与 ErrorMsg: %+v", row)
	}
	if row.ResponseBody != "半截正文内容" {
		t.Fatalf("审计 ResponseBody 应保留上游真实正文: %q", row.ResponseBody)
	}
	if row.TotalTokens != 33 || row.PromptTokens != 11 || row.CompletionTokens != 22 {
		t.Fatalf("审计应保留真实 usage: %+v", row)
	}
	if row.FinishStateRaw != "length" || row.FinishState != "length" {
		t.Fatalf("审计终态不符: raw=%q state=%q", row.FinishStateRaw, row.FinishState)
	}
	// RequestBody 为最终实际 payload（含 model 与 messages）。
	if !strings.Contains(row.RequestBody, `"model"`) || !strings.Contains(row.RequestBody, `"messages"`) {
		t.Fatalf("审计 RequestBody 应为最终 payload: %.120s", row.RequestBody)
	}
}

// TestResponsesIncompleteKeepsAudit Responses 非流式 status=incomplete（max_output_tokens
// 截断）：拒收 + 审计保留正文/usage/原始 status=incomplete。
func TestResponsesIncompleteKeepsAudit(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"截断的部分输出"}]}],` +
			`"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16}}`))
	}))
	defer srv.Close()

	run := newLLMRun("t-audit2", "", "qa", "qa.free_text.v1", "q10")
	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: model.LLMEndpointResponses, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     run.chatMeta(1, nil, 1),
	})
	run.record(res, err)

	if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("incomplete 必须业务失败: %v (code=%q)", err, RefusalCodeOf(err))
	}
	if res == nil || res.Content != "截断的部分输出" || res.FinishReason != "incomplete" || res.Usage.TotalTokens != 16 {
		t.Fatalf("拒收结果应带真实正文/usage/原始 status: %+v", res)
	}
	if run.FinishStateRaw != "incomplete" {
		t.Fatalf("run 应记录原始 status=incomplete: %q", run.FinishStateRaw)
	}
	row := auditRowOf(t, "qa")
	if row.Status != model.LLMCallStatusError || row.ResponseBody != "截断的部分输出" || row.TotalTokens != 16 {
		t.Fatalf("审计应保留真实正文与 usage: %+v", row)
	}
	if row.FinishStateRaw != "incomplete" {
		t.Fatalf("审计 finish_state_raw 应为原始 incomplete: %q", row.FinishStateRaw)
	}
	// 规范化终态：detail 携带真实 max_output_tokens 原因时归 max_tokens。
	if row.FinishState != "max_tokens" {
		t.Fatalf("带截断原因的 incomplete 应归一 max_tokens: %q", row.FinishState)
	}
}

// TestChatStreamLengthRejectKeepsAudit Chat 流式 SSE 半途 finish_reason=length：
// 拒收 + 审计保留已聚合的半截内容与上游 usage。
func TestChatStreamLengthRejectKeepsAudit(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"流式半截\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	run := newLLMRun("t-audit3", "", "compare", "compare.free_text.v1", "c1")
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m", AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
		Meta:     run.chatMeta(1, nil, 1),
	}, nil)
	run.record(res, err)

	if err == nil || RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("流式 length 终态必须业务失败: %v (code=%q)", err, RefusalCodeOf(err))
	}
	if res == nil || res.Content != "流式半截" || res.FinishReason != "length" || res.Usage.TotalTokens != 7 {
		t.Fatalf("拒收结果应带聚合半截/usage/终态: %+v", res)
	}
	if run.FinishStateRaw != "length" || run.FinishState != "length" {
		t.Fatalf("run 终态不符: raw=%q state=%q", run.FinishStateRaw, run.FinishState)
	}
	row := auditRowOf(t, "compare")
	if row.ResponseBody != "流式半截" || row.TotalTokens != 7 || row.FinishStateRaw != "length" {
		t.Fatalf("流式拒收审计应保留半截内容与 usage: %+v", row)
	}
}

// TestRejectedContentNeverParsedAsSuccess 半截正文不得进入业务成功路径：repair 循环层
// 收到「err 非空但 res 带半截」时，err 优先——不调用 parse、不落成功，半截只进审计与
// token 统计。
func TestRejectedContentNeverParsedAsSuccess(t *testing.T) {
	setContractFlag(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"rating\":\"bullish\"}"},"finish_reason":"length"}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	parseCalled := false
	svc := &AnalysisService{}
	run := newLLMRun("t-audit4", "", "analysis", "analysis.v1", "p17")
	_, usage, _, callErr := svc.callWithRepair(
		context.Background(), 1, run,
		&model.LLMConfig{BaseURL: srv.URL, Model: "m"},
		"k", true,
		[]chatMessage{{Role: "user", Content: "x"}},
		func(string) error { parseCalled = true; return nil },
		analysisRepairHint,
	)
	if callErr == nil {
		t.Fatal("拒收必须向业务层报错")
	}
	if parseCalled {
		t.Fatal("半截正文绝不能进入成功解析")
	}
	// token 统计反映实际上游消耗（audit outcome 带出的 usage 被累计）。
	if usage.TotalTokens != 10 {
		t.Fatalf("拒收调用的真实 token 应计入统计: %+v", usage)
	}
}
