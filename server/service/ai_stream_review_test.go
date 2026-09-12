package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
	"quantvista/setting"
)

func enableStreamReviewContract(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	old := setting.LLMAccuracyContract()
	if err := setting.SetLLMAccuracyContract(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setting.SetLLMAccuracyContract(old) })
}

func reviewStreamEvents(t *testing.T, endpoint string, events []string) (*chatResult, string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	var emitted strings.Builder
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "review-key", Model: "m", EndpointType: endpoint, AllowPrivate: true,
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}, func(delta string) { emitted.WriteString(delta) })
	return res, emitted.String(), err
}

func TestChatStreamCannotReplaceTerminalState(t *testing.T) {
	enableStreamReviewContract(t)
	for index, events := range [][]string{
		{`{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, "[DONE]"},
		{`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`, `{"choices":[{"delta":{"content":"extra"}}]}`, "[DONE]"},
		{`{"choices":[{"delta":{"content":"partial"},"finish_reason":"   "}]}`},
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			if _, _, err := reviewStreamEvents(t, "", events); RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
				t.Fatalf("冲突终态、终态后正文或空白终态不得算成功：%v", err)
			}
		})
	}
}

func TestChatStreamDoesNotConcatenateAlternativeChoices(t *testing.T) {
	enableStreamReviewContract(t)
	events := []string{
		`{"choices":[{"index":0,"delta":{"content":"first"}},{"index":1,"delta":{"content":"alternative"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}]}`,
		"[DONE]",
	}
	res, emitted, err := reviewStreamEvents(t, "", events)
	if err != nil || res.Content != "first" || emitted != "first" {
		t.Fatalf("应与非流式同样只读取第一条候选：res=%+v emitted=%s err=%v", res, emitted, err)
	}
}

func TestLLMStreamBoundsAccumulatedOutput(t *testing.T) {
	enableStreamReviewContract(t)
	for _, tc := range []struct {
		name, endpoint, field string
	}{
		{"chat_text", "", "content"}, {"chat_reasoning", "", "reasoning_content"}, {"chat_pending_think", "", "think"},
		{"responses_text", model.LLMEndpointResponses, "response.output_text.delta"},
		{"responses_reasoning", model.LLMEndpointResponses, "response.reasoning_text.delta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat("x", 128<<10)
			events := []string{}
			if tc.field == "think" {
				events = append(events, `{"choices":[{"delta":{"content":"<think>"}}]}`)
			}
			for i := 0; i < 9; i++ {
				var event any
				if tc.endpoint == "" {
					field := tc.field
					if field == "think" {
						field = "content"
					}
					event = map[string]any{"choices": []any{map[string]any{"delta": map[string]string{field: text}}}}
				} else {
					event = map[string]any{"type": tc.field, "delta": text}
				}
				data, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, string(data))
			}
			if tc.endpoint == "" {
				events = append(events, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, "[DONE]")
			} else {
				events = append(events, `{"type":"response.output_text.delta","delta":"ok"}`, `{"type":"response.completed","response":{"status":"completed"}}`)
			}
			res, _, err := reviewStreamEvents(t, tc.endpoint, events)
			if RefusalCodeOf(err) != RefusalLLMResponseIncomplete || !strings.Contains(err.Error(), "1MB") {
				t.Fatalf("多条小事件的总输出也必须有界：err=%v", err)
			}
			if res != nil && len(res.Content)+len(res.ReasoningContent) > aiResponseBodyLimit {
				t.Fatal("超限部分不得进入返回结果")
			}
		})
	}
}

func TestResponsesStreamUsesCompleteOutputToFillMissingTail(t *testing.T) {
	enableStreamReviewContract(t)
	for _, tc := range []struct{ name, prefix, full, want string }{
		{"no_delta", "", "whole answer", "whole answer"},
		{"missing_tail", "whole", "whole answer", "whole answer"},
		{"pending_think", "<think>reason", "<think>reason</think>answer", "answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": tc.prefix})
			complete, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
				"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": tc.full}}}},
			}})
			res, emitted, err := reviewStreamEvents(t, model.LLMEndpointResponses, []string{string(prefix), string(complete)})
			if err != nil || res.Content != tc.want || emitted != tc.want {
				t.Fatalf("完整终态应补全丢失的正文尾部并送达回调：res=%+v emitted=%s err=%v", res, emitted, err)
			}
		})
	}
	if _, _, err := reviewStreamEvents(t, model.LLMEndpointResponses, []string{
		`{"type":"response.output_text.delta","delta":"wrong"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"right"}]}]}}`,
	}); RefusalCodeOf(err) != RefusalLLMResponseIncomplete {
		t.Fatalf("增量与完整输出冲突不能静默接受增量：%v", err)
	}
}

func TestLLMStreamFlushesPendingLiteralToCallback(t *testing.T) {
	enableStreamReviewContract(t)
	res, emitted, err := reviewStreamEvents(t, "", []string{
		`{"choices":[{"delta":{"content":"<thin"},"finish_reason":"stop"}]}`, "[DONE]",
	})
	if err != nil || res.Content != "<thin" || emitted != "<thin" {
		t.Fatalf("未闭合的标签前缀仍是正文，回调不能漏发：res=%+v emitted=%s err=%v", res, emitted, err)
	}
}

func TestThinkStreamBufferDoesNotCopyWholePrefixPerChunk(t *testing.T) {
	const size = 16 << 10
	want := strings.Repeat("x", size)
	allocations := testing.AllocsPerRun(1, func() {
		filter := thinkStreamFilter{}
		filter.Push("<think>")
		for i := 0; i < size; i++ {
			filter.Push("x")
		}
		visible, reasoning := filter.Push("</think>answer")
		if visible != "answer" || reasoning != want {
			t.Fatal("思考分片和正文必须完整保留")
		}
	})
	if allocations > 256 {
		t.Fatalf("16KB 的逐字思考流不应反复复制累积前缀：allocations=%.0f", allocations)
	}
}
