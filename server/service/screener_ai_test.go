package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// P3c AI 白话建策略测试：prompt 因子字典完整性 / 输出解析与 unmatched 语义 /
// 假 LLM 端到端（含 repair 一次与配额计次）。

// TestParseStrategyPromptFactorDict prompt 因子字典由 factorDefs 程序生成——
// 每个因子的 key 与中文名都必须出现（新增因子自动进 prompt，手抄漂移在此炸）。
func TestParseStrategyPromptFactorDict(t *testing.T) {
	prompt := buildParseStrategySystemPrompt()
	for _, d := range factorDefs {
		if !strings.Contains(prompt, "- "+d.Key+"｜") {
			t.Errorf("prompt 缺因子 key %q", d.Key)
		}
		if !strings.Contains(prompt, d.Name) {
			t.Errorf("prompt 缺因子中文名 %q", d.Name)
		}
	}
	// 纪律要点必须在场：unmatched 兜底 + 禁硬凑 + 输出 schema。
	for _, kw := range []string{"unmatched", "禁止硬凑", "explain", "is_true", "不超过 80 字", "紧凑 JSON"} {
		if !strings.Contains(prompt, kw) {
			t.Errorf("prompt 缺关键内容 %q", kw)
		}
	}
	// few-shot 示例里的因子必须真实存在（示例过时会误导模型）。
	for _, key := range []string{"vol_boost", "high_20d", "turnover_rate", "bias_20", "rsi_14", "chip_profit"} {
		if _, ok := factorByKey(key); !ok {
			t.Errorf("few-shot 示例引用了不存在的因子 %q", key)
		}
	}
	// 阈值提示映射不许出现字典外的 key（改因子名忘改提示会静默失联）。
	for key := range parseFactorHints {
		if _, ok := factorByKey(key); !ok {
			t.Errorf("parseFactorHints 含未知因子 %q", key)
		}
	}
}

// TestParseStrategyLLMOutput 输出解析表驱动：合法树 / null 树 + unmatched /
// 空手交差拒绝 / 未知因子拒绝 / 坏 JSON 拒绝 / 叶子根合法。
func TestParseStrategyLLMOutput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		check   func(t *testing.T, p *parsedStrategy)
	}{
		{
			name: "合法树带 unmatched",
			in:   `{"tree":{"all":[{"factor":"chip_profit","op":"<","value":15}]},"unmatched":["北向重仓"],"explain":"低获利盘"}`,
			check: func(t *testing.T, p *parsedStrategy) {
				if p.Tree == nil || len(p.Unmatched) != 1 {
					t.Fatalf("解析结果不符: %+v", p)
				}
			},
		},
		{
			name: "null 树 + unmatched 合法",
			in:   `{"tree":null,"unmatched":["市盈率低于 20"],"explain":"无可映射条件"}`,
			check: func(t *testing.T, p *parsedStrategy) {
				if p.Tree != nil || len(p.Unmatched) != 1 {
					t.Fatalf("应为 null 树: %+v", p)
				}
			},
		},
		{
			name: "带代码块包裹容忍",
			in:   "```json\n{\"tree\":{\"factor\":\"rsi_14\",\"op\":\"<\",\"value\":30},\"unmatched\":[],\"explain\":\"超卖\"}\n```",
			check: func(t *testing.T, p *parsedStrategy) {
				if p.Tree == nil || p.Tree.Factor != "rsi_14" {
					t.Fatalf("叶子根应合法: %+v", p.Tree)
				}
			},
		},
		{name: "null 树且 unmatched 空拒绝", in: `{"tree":null,"unmatched":[],"explain":"x"}`, wantErr: true},
		{name: "未知因子拒绝", in: `{"tree":{"all":[{"factor":"pe_ttm","op":"<","value":20}]},"unmatched":[],"explain":"x"}`, wantErr: true},
		{name: "布尔因子用数值 op 拒绝", in: `{"tree":{"all":[{"factor":"bull_align","op":">","value":1}]},"unmatched":[],"explain":"x"}`, wantErr: true},
		{name: "坏 JSON 拒绝", in: `完全不是 JSON`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parseStrategyLLMOutput(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望失败，却通过了: %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功: %v", err)
			}
			if c.check != nil {
				c.check(t, p)
			}
		})
	}
}

// seedParseStrategyEnv 建内存库 + 默认 LLM 配置指向假服务器，返回配置所属 userID。
func seedParseStrategyEnv(t *testing.T, userID int64, baseURL string) {
	t.Helper()
	setupTestDB(t)
	common.DB.Exec("DELETE FROM llm_configs")
	common.DB.Exec("DELETE FROM user_quota")
	t.Cleanup(func() {
		common.DB.Exec("DELETE FROM llm_configs")
		common.DB.Exec("DELETE FROM user_quota")
	})
	common.EncryptionKey = "unit-test-key"
	cipher, err := common.Encrypt("sk-test")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cfg := &model.LLMConfig{UserID: userID, Name: "t", Provider: "openai", BaseURL: baseURL,
		APIKeyCipher: cipher, Model: "m", IsDefault: true}
	if err := common.DB.Create(cfg).Error; err != nil {
		t.Fatalf("建配置失败: %v", err)
	}
}

func parseQuotaRow(t *testing.T, userID int64) model.UserQuota {
	t.Helper()
	var q model.UserQuota
	if err := common.DB.Where("user_id = ?", userID).First(&q).Error; err != nil {
		t.Fatalf("查配额失败: %v", err)
	}
	return q
}

// TestParseStrategyEndToEnd 假 LLM 端到端：一次成功解析——树/人话回显/prompt 版本，
// 配额计 1 次动作 + token 入账。
func TestParseStrategyEndToEnd(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		content := `{\"tree\":{\"all\":[{\"factor\":\"vol_boost\",\"op\":\">=\",\"value\":2},{\"factor\":\"high_20d\",\"op\":\"is_true\"}]},\"unmatched\":[\"北向资金加仓（因子库无北向数据）\"],\"explain\":\"放量突破 20 日新高\"}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":200,"completion_tokens":80,"total_tokens":280}}`))
	}))
	defer srv.Close()
	seedParseStrategyEnv(t, 21, srv.URL)

	svc := NewScreenerAIService(NewLLMService())
	res, err := svc.ParseStrategy(context.Background(), 21, true, ParseStrategyRequest{Text: "放量突破 20 日新高，北向资金加仓"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("应只调 1 次 LLM, got %d", calls)
	}
	if res.Tree == nil || len(res.Tree.All) != 2 {
		t.Fatalf("树不符: %+v", res.Tree)
	}
	if len(res.Unmatched) != 1 || !strings.Contains(res.Unmatched[0], "北向") {
		t.Fatalf("unmatched 不符: %v", res.Unmatched)
	}
	if res.PromptVersion != screenerParsePromptVersion || res.TotalTokens != 280 {
		t.Fatalf("版本/token 不符: %s %d", res.PromptVersion, res.TotalTokens)
	}
	// 人话回显来自 describeCondTree。
	if len(res.Conditions) != 2 || !strings.Contains(res.Conditions[0], "量比") {
		t.Fatalf("人话条件不符: %v", res.Conditions)
	}
	// 配额：1 次动作 + token 入账 + 1 轮请求。
	q := parseQuotaRow(t, 21)
	if q.ActionUsed != 1 || q.TokenUsed != 280 || q.RequestCount != 1 {
		t.Fatalf("配额记账不符: %+v", q)
	}
}

// TestParseStrategyRepairOnce 首次输出未知因子触发 repair，第二次合格；
// token 累计两轮、动作仍只计 1 次；repair 请求带上了错误反馈。
func TestParseStrategyRepairOnce(t *testing.T) {
	calls := 0
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content := `{\"tree\":{\"all\":[{\"factor\":\"pe_ttm\",\"op\":\"<\",\"value\":20}]},\"unmatched\":[],\"explain\":\"x\"}`
		if calls >= 2 {
			b, _ := io.ReadAll(r.Body)
			secondBody = string(b)
			content = `{\"tree\":{\"all\":[{\"factor\":\"rsi_14\",\"op\":\"<\",\"value\":30}]},\"unmatched\":[],\"explain\":\"超卖\"}`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer srv.Close()
	seedParseStrategyEnv(t, 22, srv.URL)

	svc := NewScreenerAIService(NewLLMService())
	res, err := svc.ParseStrategy(context.Background(), 22, true, ParseStrategyRequest{Text: "RSI 超卖"})
	if err != nil {
		t.Fatalf("repair 后应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好 repair 一次（2 次调用）, got %d", calls)
	}
	if res.Tree == nil || res.Tree.All[0].Factor != "rsi_14" {
		t.Fatalf("repair 后树不符: %+v", res.Tree)
	}
	if res.TotalTokens != 300 {
		t.Fatalf("token 应累计两轮 = 300, got %d", res.TotalTokens)
	}
	if !strings.Contains(secondBody, "pe_ttm") {
		t.Fatalf("repair 请求应带上错误反馈: %s", secondBody)
	}
	q := parseQuotaRow(t, 22)
	if q.ActionUsed != 1 || q.TokenUsed != 300 || q.RequestCount != 1 {
		t.Fatalf("配额记账不符（repair 不重复计次，记账一次入账）: %+v", q)
	}
}

// TestParseStrategyRepairStillBad repair 后仍不合法：报错不出脏树，token 照记、动作照计次。
func TestParseStrategyRepairStillBad(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		content := `{\"tree\":{\"all\":[{\"factor\":\"pe_ttm\",\"op\":\"<\",\"value\":20}]},\"unmatched\":[],\"explain\":\"x\"}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"total_tokens":100}}`))
	}))
	defer srv.Close()
	seedParseStrategyEnv(t, 23, srv.URL)

	svc := NewScreenerAIService(NewLLMService())
	_, err := svc.ParseStrategy(context.Background(), 23, true, ParseStrategyRequest{Text: "低市盈率"})
	if err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("应报解析失败: %v", err)
	}
	// P0-9：repair 打满进统一机读码。
	if RefusalCodeOf(err) != RefusalLLMOutputInvalid {
		t.Fatalf("repair 耗尽应带 llm_output_invalid 码: %q", RefusalCodeOf(err))
	}
	if calls != 2 {
		t.Fatalf("应止步于 1 次 repair, got %d", calls)
	}
	q := parseQuotaRow(t, 23)
	if q.ActionUsed != 1 || q.TokenUsed != 200 {
		t.Fatalf("失败也应记账: %+v", q)
	}
}

// TestParseStrategyQuotaExhausted 次数配额用尽：熔断在调用前，零 LLM 成本。
func TestParseStrategyQuotaExhausted(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	seedParseStrategyEnv(t, 24, srv.URL)
	common.DB.Create(&model.UserQuota{UserID: 24, ActionLimit: 3, ActionUsed: 3})

	svc := NewScreenerAIService(NewLLMService())
	_, err := svc.ParseStrategy(context.Background(), 24, true, ParseStrategyRequest{Text: "放量上涨"})
	if err == nil || !strings.Contains(err.Error(), "配额") {
		t.Fatalf("应配额熔断: %v", err)
	}
	if calls != 0 {
		t.Fatalf("熔断不应发起 LLM 调用, got %d", calls)
	}
}

// TestParseStrategyInputValidation 空输入 / 超长输入直接拒绝（不查配置不发调用）。
func TestParseStrategyInputValidation(t *testing.T) {
	svc := NewScreenerAIService(NewLLMService())
	if _, err := svc.ParseStrategy(context.Background(), 1, false, ParseStrategyRequest{Text: "   "}); err == nil {
		t.Fatal("空输入应拒绝")
	}
	long := strings.Repeat("涨", parseStrategyTextMax+1)
	if _, err := svc.ParseStrategy(context.Background(), 1, false, ParseStrategyRequest{Text: long}); err == nil {
		t.Fatal("超长输入应拒绝")
	}
}

// TestParseStrategyAsyncTask 白话选股解析立即返回 processing 任务，
// LLM 调用在后台完成后通过通用任务详情返回原 ParseStrategyResult。
func TestParseStrategyAsyncTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := `{\"tree\":{\"all\":[{\"factor\":\"rsi_14\",\"op\":\"<\",\"value\":30}]},\"unmatched\":[],\"explain\":\"RSI 超卖\"}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"total_tokens":90}}`))
	}))
	defer srv.Close()

	const userID int64 = 12022
	resetAsyncLLMTasks(t)
	seedParseStrategyEnv(t, userID, srv.URL)
	svc := NewScreenerAIService(NewLLMService())
	task, err := svc.ParseStrategyAsync(userID, true, ParseStrategyRequest{Text: "  RSI 超卖  "})
	if err != nil {
		t.Fatalf("建解析任务失败: %v", err)
	}
	if task.Kind != "screener_parse" || task.Status != model.LLMTaskStatusProcessing {
		t.Fatalf("应立即返回 screener_parse processing 任务: %+v", task)
	}
	done := waitAsyncLLMTask(t, userID, task.ID)
	if done.Status != model.LLMTaskStatusSuccess {
		t.Fatalf("解析任务应成功: %+v", done)
	}
	var got ParseStrategyResult
	if err := json.Unmarshal(done.Result, &got); err != nil {
		t.Fatalf("解码解析结果失败: %v; raw=%s", err, done.Result)
	}
	if got.Tree == nil || len(got.Tree.All) != 1 || got.Tree.All[0].Factor != "rsi_14" {
		t.Fatalf("后台解析条件树不正确: %+v", got.Tree)
	}
}
