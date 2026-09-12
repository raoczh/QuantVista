package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"quantvista/model"
)

func TestReasoningCapabilityDoesNotRejectDifferentEffort(t *testing.T) {
	setCapRoutingFlag(t, true)
	for _, endpoint := range []string{model.LLMEndpointChat, model.LLMEndpointResponses} {
		for _, stream := range []bool{false, true} {
			t.Run(endpoint+map[bool]string{false: "_plain", true: "_stream"}[stream], func(t *testing.T) {
				resetLLMCapabilityStore()
				t.Cleanup(resetLLMCapabilityStore)
				var mu sync.Mutex
				var efforts []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Effort    string `json:"reasoning_effort"`
						Reasoning struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "bad request", 400)
						return
					}
					effort := body.Effort
					if endpoint == model.LLMEndpointResponses {
						effort = body.Reasoning.Effort
					}
					mu.Lock()
					efforts = append(efforts, effort)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if effort == "max" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":{"message":"reasoning_effort must be one of low, medium, high"}}`))
					} else if endpoint == model.LLMEndpointResponses {
						_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
					} else {
						_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
					}
				}))
				defer srv.Close()
				p := chatParams{BaseURL: srv.URL, APIKey: "review-key", Model: "m", EndpointType: endpoint,
					ReasoningEffort: "max", MaxTokens: 256, AllowPrivate: true,
					Messages: []chatMessage{{Role: "user", Content: "hi"}}}
				call := func() error {
					var err error
					if stream {
						_, err = chatCompletionStream(context.Background(), p, nil)
					} else {
						_, err = chatCompletion(context.Background(), p)
					}
					return err
				}
				if err := call(); err != nil {
					t.Fatal(err)
				}
				p.ReasoningEffort = "low"
				if err := call(); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(efforts) != 3 || efforts[0] != "max" || efforts[1] != "" || efforts[2] != "low" {
					t.Fatalf("max 被拒后应仍发送新配置的 low：%v", efforts)
				}
			})
		}
	}
}

func TestLLMDraftProbeUsesOwnedConfigIdentity(t *testing.T) {
	svc, userID := setupLLMConfigTest(t)
	resetLLMCapabilityStore()
	t.Cleanup(resetLLMCapabilityStore)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()
	in := validLLMConfigInput()
	in.Provider, in.BaseURL, in.APIKey = "review-gateway", srv.URL, "review-key"
	cfg, err := svc.Create(userID, in)
	if err != nil {
		t.Fatal(err)
	}
	draft := LLMConfigInput{ConfigID: cfg.ID, BaseURL: srv.URL, Model: cfg.Model}
	if result, err := svc.TestByInput(userID, draft, true); err != nil || !result.OK {
		t.Fatalf("本人草稿应可复用密钥测试：result=%+v err=%v", result, err)
	}
	target := llmCapabilityTarget(cfg.ID, cfg.Provider, cfg.BaseURL, cfg.Model, cfg.EndpointType)
	if obs, ok := lookupLLMCapability(target, capJSONObject); !ok || obs.State != capSupported {
		t.Error("草稿未提交 provider 时应沿用该配置身份，能力观察必须被实际调用读取")
	}
	before := calls.Load()
	draft.APIKey = "another-review-key"
	if _, err := svc.TestByInput(userID+1, draft, true); err == nil {
		t.Error("显式提供密钥也不能将探测归属到其他用户配置")
	}
	if _, _, err := svc.FetchModels(userID+1, draft, true); err == nil {
		t.Error("拉取模型必须验证 config_id 的归属")
	}
	if calls.Load() != before {
		t.Error("越权配置请求应在访问上游之前被拒绝")
	}
}

func TestJSONSmokeRequiresAnActualCompleteObject(t *testing.T) {
	cases := []struct {
		name, endpoint, response string
		want                     llmCapState
	}{
		{"chat_text", "", `{"choices":[{"message":{"content":"hello"}}]}`, capUnknown},
		{"chat_empty", "", `{"choices":[{}]}`, capUnknown},
		{"chat_array", "", `{"choices":[{"message":{"content":"[]"}}]}`, capUnknown},
		{"chat_truncated", "", `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"length"}]}`, capUnknown},
		{"chat_object", "", `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`, capSupported},
		{"responses_reasoning", model.LLMEndpointResponses, `{"status":"completed","output":[{"type":"reasoning","summary":[]}]}`, capUnknown},
		{"responses_incomplete", model.LLMEndpointResponses, `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`, capUnknown},
		{"responses_object", model.LLMEndpointResponses, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`, capSupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLLMCapabilityStore()
			t.Cleanup(resetLLMCapabilityStore)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			target := llmProbeTarget{Provider: "review-gateway", BaseURL: srv.URL, APIKey: "review-key", Model: "m", EndpointType: tc.endpoint, AllowPrivate: true}
			if got := NewLLMService().probeJSONModeCapability(target); got != tc.want {
				t.Fatalf("结构化能力结论应依据正文：got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestLLMCapabilityURLPathRemainsCaseSensitive(t *testing.T) {
	a := llmCapabilityTarget(1, "gateway", "https://example.com/RouteA", "m", "")
	b := llmCapabilityTarget(1, "gateway", "https://EXAMPLE.COM/routea", "m", "")
	if a == b {
		t.Fatal("大小写不同的上游路径不得共享能力结论")
	}
}
