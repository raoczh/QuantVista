package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"quantvista/model"
)

func TestChatUsageAliasesTakeMaxWithoutDoubleCounting(t *testing.T) {
	var usage chatUsage
	err := json.Unmarshal([]byte(`{
		"prompt_tokens":11,
		"completion_tokens":7,
		"total_tokens":0,
		"prompt_tokens_details":{"cached_tokens":3},
		"input_tokens_details":{"cached_tokens":5},
		"prompt_cache_hit_tokens":4,
		"cached_tokens":2,
		"completion_tokens_details":{"reasoning_tokens":6},
		"output_tokens_details":{"reasoning_tokens":8}
	}`), &usage)
	if err != nil {
		t.Fatalf("解析 usage 别名失败: %v", err)
	}
	if usage.CachedTokens != 5 || usage.ReasoningTokens != 8 {
		t.Fatalf("同义字段应取最大值而非累计: %+v", usage)
	}
	usage.fillMissingUsageTotal()
	if usage.TotalTokens != 18 {
		t.Fatalf("total 只能由 prompt+completion 补齐，不能重复加 cached/reasoning: %+v", usage)
	}
}

func TestResponsesUsageAliasesTakeMaxWithoutDoubleCounting(t *testing.T) {
	usage := (responsesUsage{
		InputTokens: 13, OutputTokens: 9,
		InputTokensDetails:     inputTokenDetails{CachedTokens: 3},
		PromptTokensDetails:    inputTokenDetails{CachedTokens: 7},
		OutputTokensDetails:    outputTokenDetails{ReasoningTokens: 5},
		CompletionTokenDetails: outputTokenDetails{ReasoningTokens: 8},
		PromptCacheHitTokens:   6, CachedTokensCompat: 4,
	}).toChatUsage()
	if usage.PromptTokens != 13 || usage.CompletionTokens != 9 || usage.TotalTokens != 22 ||
		usage.CachedTokens != 7 || usage.ReasoningTokens != 8 {
		t.Fatalf("Responses usage 归一结果错误: %+v", usage)
	}
}

func TestUsageMissingParentsUsesDetailLowerBounds(t *testing.T) {
	usage := chatUsage{PromptTokens: 11, CachedTokens: 3, ReasoningTokens: 4}
	usage.fillMissingUsageTotal()
	if usage.TotalTokens != 15 {
		t.Fatalf("completion 缺失时应由 reasoning 补对应侧下界: %+v", usage)
	}

	onlyDetails := chatUsage{CachedTokens: 3, ReasoningTokens: 4}
	onlyDetails.fillMissingUsageTotal()
	if onlyDetails.TotalTokens != 7 {
		t.Fatalf("仅有明细时 total 应为两侧已知下界之和: %+v", onlyDetails)
	}

	var merged chatUsage
	mergeChatUsage(&merged, chatUsage{PromptTokens: 11, CachedTokens: 3})
	mergeChatUsage(&merged, chatUsage{CompletionTokens: 7, ReasoningTokens: 4})
	merged.fillMissingUsageTotal()
	if merged.PromptTokens != 11 || merged.CompletionTokens != 7 || merged.CachedTokens != 3 ||
		merged.ReasoningTokens != 4 || merged.TotalTokens != 18 {
		t.Fatalf("分块 usage 应逐字段取大合并: %+v", merged)
	}

	reported := chatUsage{TotalTokens: 9}
	mergeChatUsage(&reported, chatUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 8})
	if reported.TotalTokens != 9 {
		t.Fatalf("上游非零 total 是真实分量，不得被推导下界覆盖: %+v", reported)
	}
}

func TestUsageEstimateOnlyWhenEntirelyMissing(t *testing.T) {
	messages := []chatMessage{{Role: "user", Content: "123456"}}
	reported := usageOrEstimate(messages, "abcdefgh", chatUsage{PromptTokens: 2})
	if reported.PromptTokens != 2 || reported.CompletionTokens != 0 || reported.TotalTokens != 2 {
		t.Fatalf("已有任一真实分量时不得用正文估算覆盖整包 usage: %+v", reported)
	}
	estimated := usageOrEstimate(messages, "abcdefgh", chatUsage{})
	if estimated.PromptTokens != 3 || estimated.CompletionTokens != 4 || estimated.TotalTokens != 7 {
		t.Fatalf("全部 usage 缺失时才应估算: %+v", estimated)
	}
}

func TestThinkStreamFilterAcrossChunks(t *testing.T) {
	filter := thinkStreamFilter{}
	chunks := []string{" \n", "<th", "ink>隐藏推", "理</th", "ink>正文<th", "ink>字面</think>"}
	var visible, reasoning string
	for _, chunk := range chunks {
		v, r := filter.Push(chunk)
		visible += v
		reasoning += r
	}
	v, r := filter.Flush()
	visible += v
	reasoning += r
	if visible != " \n正文<think>字面</think>" || reasoning != "隐藏推理" {
		t.Fatalf("跨 chunk think 剥离错误: visible=%q reasoning=%q", visible, reasoning)
	}
}

func TestSplitThinkContentOnlyStripsLeadingCompleteBlock(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		wantVisible   string
		wantReasoning string
	}{
		{
			name:          "leading_complete_case_insensitive",
			content:       " \n<ThInK>隐藏推理</tHiNk>正文<think>字面</think>",
			wantVisible:   " \n正文<think>字面</think>",
			wantReasoning: "隐藏推理",
		},
		{
			name:        "embedded_literal",
			content:     "正文<think>字面</think>结论",
			wantVisible: "正文<think>字面</think>结论",
		},
		{
			name:        "json_string_literal",
			content:     `{"text":"<think>字面</think>"}`,
			wantVisible: `{"text":"<think>字面</think>"}`,
		},
		{
			name:        "unclosed_leading_block",
			content:     " \t<think>未闭合正文",
			wantVisible: " \t<think>未闭合正文",
		},
		{
			name:        "partial_open_tag",
			content:     "<thi",
			wantVisible: "<thi",
		},
		{
			name:        "different_open_tag",
			content:     "<thinking>正文</thinking>",
			wantVisible: "<thinking>正文</thinking>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			visible, reasoning := splitThinkContent(tc.content)
			if visible != tc.wantVisible || reasoning != tc.wantReasoning {
				t.Fatalf("think 解析错误: visible=%q reasoning=%q", visible, reasoning)
			}
		})
	}
}

func TestThinkStreamFilterUnclosedLeadingBlockFallsBackToVisible(t *testing.T) {
	filter := thinkStreamFilter{}
	original := " \n<think>未闭合的推理与正文"
	for _, chunk := range []string{" \n<th", "ink>未闭合", "的推理与正文"} {
		visible, reasoning := filter.Push(chunk)
		if visible != "" || reasoning != "" {
			t.Fatalf("闭合前不应提前消费候选块: visible=%q reasoning=%q", visible, reasoning)
		}
	}
	visible, reasoning := filter.Flush()
	if visible != original || reasoning != "" {
		t.Fatalf("未闭合 think 应原样回退正文: visible=%q reasoning=%q", visible, reasoning)
	}
}

func TestThinkStreamFilterEmbeddedLiteralPassesThrough(t *testing.T) {
	filter := thinkStreamFilter{}
	original := "正文<think>字面</think>结论"
	var visible, reasoning string
	for _, chunk := range []string{"正文<th", "ink>字面</th", "ink>结论"} {
		v, r := filter.Push(chunk)
		visible += v
		reasoning += r
	}
	v, r := filter.Flush()
	visible += v
	reasoning += r
	if visible != original || reasoning != "" {
		t.Fatalf("正文中的跨 chunk think 字面标签应原样直通: visible=%q reasoning=%q", visible, reasoning)
	}
}

func TestUsageMissingTotalPreservesComponentsAcrossEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		stream     bool
		response   string
		contentTyp string
		wantPrompt int
		wantComp   int
		wantTotal  int
	}{
		{
			name:       "chat_plain",
			response:   `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":4}}}`,
			contentTyp: "application/json",
			wantPrompt: 11, wantComp: 0, wantTotal: 15,
		},
		{
			name: "chat_stream", stream: true, contentTyp: "text/event-stream",
			response:   "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":11,\"total_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":7,\"total_tokens\":0,\"completion_tokens_details\":{\"reasoning_tokens\":4}}}\n\ndata: [DONE]\n\n",
			wantPrompt: 11, wantComp: 7, wantTotal: 18,
		},
		{
			name: "responses_plain", endpoint: model.LLMEndpointResponses,
			response:   `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":4}}}`,
			contentTyp: "application/json",
			wantPrompt: 0, wantComp: 0, wantTotal: 7,
		},
		{
			name: "responses_stream", endpoint: model.LLMEndpointResponses, stream: true,
			response:   "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":0,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n",
			contentTyp: "text/event-stream",
			wantPrompt: 11, wantComp: 7, wantTotal: 18,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentTyp)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()

			params := chatParams{BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "x"}}, AllowPrivate: true}
			var (
				res *chatResult
				err error
			)
			if tc.stream {
				res, err = chatCompletionStreamInner(context.Background(), params, nil)
			} else {
				res, err = chatCompletionPlain(context.Background(), params)
			}
			if err != nil {
				t.Fatalf("调用失败: %v", err)
			}
			if res.Usage.PromptTokens != tc.wantPrompt || res.Usage.CompletionTokens != tc.wantComp ||
				res.Usage.TotalTokens != tc.wantTotal || res.Usage.CachedTokens != 3 || res.Usage.ReasoningTokens != 4 {
				t.Fatalf("真实分量应保留且只补 total: %+v", res.Usage)
			}
		})
	}
}

func TestStreamFailurePreservesReportedUsage(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{
			name: "chat",
			body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":0,\"total_tokens\":0,\"completion_tokens_details\":{\"reasoning_tokens\":4}},\"error\":{\"message\":\"failed\"}}\n\n",
		},
		{
			name: "responses", endpoint: model.LLMEndpointResponses,
			body: "data: {\"type\":\"response.failed\",\"message\":\"failed\",\"response\":{\"status\":\"failed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":0,\"total_tokens\":0,\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, err := chatCompletionStreamInner(context.Background(), chatParams{
				BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "x"}}, AllowPrivate: true,
			}, nil)
			if err == nil || res == nil {
				t.Fatalf("失败终态应返回带审计事实的错误结果: res=%+v err=%v", res, err)
			}
			if res.Usage.PromptTokens != 11 || res.Usage.ReasoningTokens != 4 || res.Usage.TotalTokens != 15 {
				t.Fatalf("失败终态 usage 应保留真实分量并只补 total: %+v", res.Usage)
			}
		})
	}
}

func TestStreamFailureEstimatesWhenUsageEntirelyMissing(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{
			name: "chat",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"半截\"}}]}\n\ndata: {\"error\":{\"message\":\"failed\"}}\n\n",
		},
		{
			name: "responses", endpoint: model.LLMEndpointResponses,
			body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"半截\"}\n\ndata: {\"type\":\"response.failed\",\"message\":\"failed\"}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, err := chatCompletionStreamInner(context.Background(), chatParams{
				BaseURL: srv.URL, APIKey: "k", Model: "m", EndpointType: tc.endpoint,
				Messages: []chatMessage{{Role: "user", Content: "123456"}}, AllowPrivate: true,
			}, nil)
			if err == nil || res == nil {
				t.Fatalf("失败终态应返回可审计结果: res=%+v err=%v", res, err)
			}
			if res.Usage.PromptTokens != 3 || res.Usage.CompletionTokens != 1 || res.Usage.TotalTokens != 4 {
				t.Fatalf("完全无 usage 的失败响应才应按实际半截正文估算: %+v", res.Usage)
			}
		})
	}
}
