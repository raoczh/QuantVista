package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/common"
)

// 本文件覆盖 2026-08-20 配置面板批的修复项（列宽/空白归一化/探测密钥三态）。
// 这些路径此前无测试，而它们都是「新增的拉取模型 + 抽屉表单」直接暴露出来的。

// TestLLMConfigValidateFieldLength 名称/Base URL/模型名超 DB 列宽必须在业务层被拒并给出可读
// 提示。不拦的话 MySQL 严格模式抛 Error 1406 原始报错、SQLite 却静默存下，两端行为不一致。
// 模型名尤其重要：「从上游拉取模型」会把聚合中转的长模型名直接灌进表单。
func TestLLMConfigValidateFieldLength(t *testing.T) {
	svc := &LLMService{}

	// 边界值（正好等于列宽）必须放行，否则白拦合法配置。
	atLimit := validLLMConfigInput()
	atLimit.Name = strings.Repeat("n", llmNameMaxLen)
	atLimit.Model = strings.Repeat("m", llmModelMaxLen)
	atLimit.BaseURL = "https://x.example.com/" + strings.Repeat("p", llmBaseURLMaxLen-22)
	if err := svc.validate(atLimit); err != nil {
		t.Errorf("正好等于列宽的配置不应被拒: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*LLMConfigInput)
		want   string
	}{
		{"名称超长", func(in *LLMConfigInput) { in.Name = strings.Repeat("n", llmNameMaxLen+1) }, "名称过长"},
		{"BaseURL超长", func(in *LLMConfigInput) {
			in.BaseURL = "https://x.example.com/" + strings.Repeat("p", llmBaseURLMaxLen)
		}, "Base URL 过长"},
		{"模型名超长", func(in *LLMConfigInput) {
			// 聚合中转的真实形态：accounts/<org>/models/<很长的模型名>
			in.Model = "accounts/some-long-org-name/models/" + strings.Repeat("x", 40)
		}, "模型名过长"},
	}
	for _, c := range cases {
		in := validLLMConfigInput()
		c.mutate(&in)
		err := svc.validate(in)
		if err == nil {
			t.Errorf("%s 应被拒绝", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 的报错应包含 %q，实际: %v", c.name, c.want, err)
		}
	}

	// 长度按字符数计（对齐 varchar 语义），不能用 Go 的字节长度：64 个汉字合法。
	cjk := validLLMConfigInput()
	cjk.Name = strings.Repeat("配", llmNameMaxLen)
	if err := svc.validate(cjk); err != nil {
		t.Errorf("%d 个汉字的名称应合法（varchar 按字符计）: %v", llmNameMaxLen, err)
	}
	cjk.Name = strings.Repeat("配", llmNameMaxLen+1)
	if err := svc.validate(cjk); err == nil {
		t.Errorf("%d 个汉字的名称应被拒", llmNameMaxLen+1)
	}
}

// TestLLMConfigCRUDTrimsWhitespace 从别处复制来的带空白 Base URL 必须落库前归一化。
// 只 TrimRight("/") 的旧写法会把空格一起存下，之后 url.Parse 解不出 scheme——用户看着
// 地址明明是对的却一直报「Base URL 非法」，极难自查。
func TestLLMConfigCRUDTrimsWhitespace(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM llm_configs").Error; err != nil {
		t.Fatalf("清理 LLM 配置失败: %v", err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-trim-test-key"
	t.Cleanup(func() {
		common.EncryptionKey = oldKey
		common.DB.Exec("DELETE FROM llm_configs")
	})

	svc := &LLMService{}
	in := validLLMConfigInput()
	in.APIKey = "sk-test"
	in.Name = "  我的中转  "
	in.BaseURL = "  https://api.example.com/  "
	in.Model = "  deepseek-chat\n"
	in.ReasoningEffort = "  high  "

	created, err := svc.Create(202, in)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}
	if created.BaseURL != "https://api.example.com" {
		t.Errorf("Base URL 未归一化: %q", created.BaseURL)
	}
	if created.Name != "我的中转" || created.Model != "deepseek-chat" {
		t.Errorf("名称/模型未去空白: name=%q model=%q", created.Name, created.Model)
	}
	if created.ReasoningEffort != "high" {
		t.Errorf("思考档位未去空白: %q", created.ReasoningEffort)
	}

	in.APIKey = ""
	in.BaseURL = "\thttps://api2.example.com//"
	in.Model = " gpt-5 "
	updated, err := svc.Update(202, created.ID, in)
	if err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if updated.BaseURL != "https://api2.example.com" {
		t.Errorf("更新未归一化 Base URL: %q", updated.BaseURL)
	}
	if updated.Model != "gpt-5" {
		t.Errorf("更新未去空白 model: %q", updated.Model)
	}
}

// TestTestByInputReusesStoredKey 编辑已有配置时密钥留空 + 带 config_id，测试连接必须复用
// 已存密钥。否则「只改了思考档位想测一下」得先重填 key，而同一抽屉里的「拉取模型」却不用
// ——那种不一致会被当成测试连接坏了。
func TestTestByInputReusesStoredKey(t *testing.T) {
	setupTestDB(t)
	if err := common.DB.Exec("DELETE FROM llm_configs").Error; err != nil {
		t.Fatalf("清理 LLM 配置失败: %v", err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-probe-key-test"
	t.Cleanup(func() {
		common.EncryptionKey = oldKey
		common.DB.Exec("DELETE FROM llm_configs")
	})

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	svc := &LLMService{}
	in := validLLMConfigInput()
	in.APIKey = "sk-stored-secret"
	in.BaseURL = srv.URL
	created, err := svc.Create(303, in)
	if err != nil {
		t.Fatalf("创建配置失败: %v", err)
	}

	// 草稿测试：密钥留空、带 config_id → 复用已存密钥。
	draft := LLMConfigInput{
		BaseURL: srv.URL, Model: "m", APIKey: "", ConfigID: created.ID,
		ReasoningEffort: "high",
	}
	res, err := svc.TestByInput(303, draft, true)
	if err != nil {
		t.Fatalf("留空密钥的草稿测试不应报错: %v", err)
	}
	if !res.OK {
		t.Fatalf("测试应通过: %+v", res)
	}
	if gotAuth != "Bearer sk-stored-secret" {
		t.Errorf("未复用已存密钥，实际 Authorization=%q", gotAuth)
	}
	// 档位被接受时结果如实标注，用户据此确认档位真的生效了。
	if !strings.Contains(res.Message, "high") {
		t.Errorf("结果应说明思考档位结论: %s", res.Message)
	}

	// 不带 config_id 且无密钥 → 明确报错（不能静默拿别的配置的密钥去打）。
	if _, err := svc.TestByInput(303, LLMConfigInput{BaseURL: srv.URL, Model: "m"}, true); err == nil {
		t.Error("无密钥且无 config_id 应报错")
	}
	// config_id 属于他人 → getOwned 拦住，不得越权取密钥。
	if _, err := svc.TestByInput(999, LLMConfigInput{
		BaseURL: srv.URL, Model: "m", ConfigID: created.ID,
	}, true); err == nil {
		t.Error("拿别人的 config_id 取密钥应被拒")
	}
}

// TestFetchModelsTruncatesAtLimit 超过 llmModelsFetchLimit 时截断并如实上报 truncated
//（前端文案「已截断至前 500 个」依赖这个口径）。排序/密钥三态/空列表/HTML 归因见
// llm_reasoning_effort_test.go 的 TestFetchModels，此处只补它未覆盖的截断边界。
func TestFetchModelsTruncatesAtLimit(t *testing.T) {
	total := llmModelsFetchLimit + 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		type item struct {
			ID string `json:"id"`
		}
		data := make([]item, 0, total)
		for i := total; i > 0; i-- { // 倒序生成：截断必须发生在排序之后
			data = append(data, item{ID: fmt.Sprintf("m-%04d", i)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	models, truncated, err := (&LLMService{}).FetchModels(404,
		LLMConfigInput{BaseURL: srv.URL, APIKey: "sk-x"}, true)
	if err != nil {
		t.Fatalf("拉取模型失败: %v", err)
	}
	if !truncated {
		t.Errorf("超过 %d 个模型应上报 truncated", llmModelsFetchLimit)
	}
	if len(models) != llmModelsFetchLimit {
		t.Fatalf("应截断到 %d 个，实际 %d", llmModelsFetchLimit, len(models))
	}
	// 截断保留的必须是排序后的前 N 个，而不是上游原始顺序的前 N 个。
	if models[0].ID != "m-0001" {
		t.Errorf("截断应发生在排序之后，首项为 %q", models[0].ID)
	}
}
