package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// resetLLMCallLogs 清空审计表。内存库 cache=shared 跨测试共享，防止用例间串扰。
func resetLLMCallLogs(t *testing.T) {
	t.Helper()
	if err := common.DB.Where("1 = 1").Delete(&model.LLMCallLog{}).Error; err != nil {
		t.Fatalf("清空审计表失败: %v", err)
	}
}

// TestLLMCallLog_NonStream chatCompletion 出口审计：成功/失败各落一行，字段完整；
// stream 列必须记录实际请求形态——chatCompletion 默认流式优先（2026-07-14 起），
// 上游整包返回（假流式）时请求仍是 stream=true 且 first_chunk_ms≈latency_ms 可识别；
// 上游明确拒绝 stream 时回落非流式，审计记 stream=false。
func TestLLMCallLog_NonStream(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`))
	}))
	defer okSrv.Close()

	if _, err := chatCompletion(context.Background(), chatParams{
		BaseURL: okSrv.URL, APIKey: "k", Model: "m1",
		Messages:     []chatMessage{{Role: "user", Content: "审计埋点测试"}},
		AllowPrivate: true,
		Meta:         chatMeta{CallerUserID: 42, Module: "qa", ConfigID: 9, Provider: "openai"},
	}); err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	var row model.LLMCallLog
	if err := common.DB.Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("成功调用应落审计行: %v", err)
	}
	if row.UserID != 42 || row.Module != "qa" || row.LLMConfigID != 9 || row.Provider != "openai" || row.Model != "m1" {
		t.Fatalf("归属字段不符: %+v", row)
	}
	// 上游忽略 stream 整包返回：实际请求形态仍是流式，first_chunk_ms 记整包到达时刻。
	if row.EndpointType != model.LLMEndpointChat || !row.Stream {
		t.Fatalf("端点/流式标记不符（应记实际请求形态 stream=true）: endpoint=%q stream=%v", row.EndpointType, row.Stream)
	}
	if row.FirstChunkMs <= 0 {
		t.Fatalf("假流式整包返回应记 first_chunk_ms: %+v", row)
	}
	if row.Status != model.LLMCallStatusSuccess || row.ErrorMsg != "" {
		t.Fatalf("成功行状态不符: %+v", row)
	}
	if row.PromptTokens != 7 || row.CompletionTokens != 5 || row.TotalTokens != 12 {
		t.Fatalf("token 不符: %+v", row)
	}
	if !strings.Contains(row.RequestBody, "审计埋点测试") || row.ResponseBody != "ok" {
		t.Fatalf("请求/响应正文不符: req=%q resp=%q", row.RequestBody, row.ResponseBody)
	}

	// 上游明确拒绝 stream（4xx 文案含 stream）：回落非流式成功，审计必须记 stream=false。
	noStreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"stream mode is not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"plain"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer noStreamSrv.Close()

	res, err := chatCompletion(context.Background(), chatParams{
		BaseURL: noStreamSrv.URL, APIKey: "k", Model: "m1",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
		Meta:         chatMeta{CallerUserID: 42, Module: "qa", ConfigID: 9, Provider: "openai"},
	})
	if err != nil || res.Content != "plain" {
		t.Fatalf("拒绝 stream 的上游应回落非流式成功: res=%+v err=%v", res, err)
	}
	var plainRow model.LLMCallLog
	if err := common.DB.Order("id desc").First(&plainRow).Error; err != nil {
		t.Fatalf("回落调用应落审计行: %v", err)
	}
	if plainRow.Stream || plainRow.FirstChunkMs != 0 {
		t.Fatalf("回落非流式应记 stream=false 且无 first_chunk: %+v", plainRow)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer errSrv.Close()

	if _, err := chatCompletion(context.Background(), chatParams{
		BaseURL: errSrv.URL, APIKey: "k", Model: "m1",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
		Meta:         chatMeta{CallerUserID: 42, Module: "analysis", ConfigID: 9, Provider: "openai"},
	}); err == nil {
		t.Fatal("401 应报错")
	}
	var errRow model.LLMCallLog
	if err := common.DB.Order("id desc").First(&errRow).Error; err != nil {
		t.Fatalf("失败调用也应落审计行: %v", err)
	}
	if errRow.Status != model.LLMCallStatusError || errRow.Module != "analysis" {
		t.Fatalf("失败行状态不符: %+v", errRow)
	}
	if !strings.Contains(errRow.ErrorMsg, "bad key") || !strings.Contains(errRow.ResponseBody, "bad key") {
		t.Fatalf("错误信息应带上游 message: msg=%q resp=%q", errRow.ErrorMsg, errRow.ResponseBody)
	}
}

// TestLLMCallLog_Stream 流式成功落行：stream=true、内容为完整拼接、usage 记录。
func TestLLMCallLog_Stream(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":6,\"total_tokens\":9}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var deltas strings.Builder
	res, err := chatCompletionStream(context.Background(), chatParams{
		BaseURL: srv.URL, APIKey: "k", Model: "m2",
		Messages:     []chatMessage{{Role: "user", Content: "hi"}},
		AllowPrivate: true,
		Meta:         chatMeta{CallerUserID: 7, Module: "qa", ConfigID: 3, Provider: "openai"},
	}, func(d string) { deltas.WriteString(d) })
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if res.Content != "你好" || deltas.String() != "你好" {
		t.Fatalf("流式内容不符: res=%q deltas=%q", res.Content, deltas.String())
	}
	var row model.LLMCallLog
	if err := common.DB.Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("流式调用应落审计行: %v", err)
	}
	if !row.Stream || row.Status != model.LLMCallStatusSuccess || row.Module != "qa" || row.UserID != 7 {
		t.Fatalf("流式行不符: %+v", row)
	}
	if row.ResponseBody != "你好" || row.TotalTokens != 9 {
		t.Fatalf("流式正文/usage 不符: resp=%q total=%d", row.ResponseBody, row.TotalTokens)
	}
}

// TestLLMCallLog_ListFilterAndDetail 列表筛选（user/module/status）、分页 total、
// 轻重字段分离（列表不带正文，详情带全文）、username 回填。
func TestLLMCallLog_ListFilterAndDetail(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)

	if err := common.DB.Create(&model.User{ID: 1001, Username: "alice", DisplayName: "爱丽丝"}).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	rows := []model.LLMCallLog{
		{UserID: 1001, Module: "qa", Status: model.LLMCallStatusSuccess, RequestBody: "req-a", ResponseBody: "resp-a"},
		{UserID: 1001, Module: "news", Status: model.LLMCallStatusError, ErrorMsg: "boom", RequestBody: "req-b"},
		{UserID: 1002, Module: "qa", Status: model.LLMCallStatusSuccess, RequestBody: "req-c"},
	}
	for i := range rows {
		if err := common.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("造审计行失败: %v", err)
		}
	}

	svc := &AdminService{}
	list, err := svc.ListLLMCalls(1001, "", "", "", 1, 10)
	if err != nil || list.Total != 2 {
		t.Fatalf("按用户筛选应 2 条: total=%d err=%v", list.Total, err)
	}
	if list.Items[0].RequestBody != "" || list.Items[0].ResponseBody != "" {
		t.Fatalf("列表不应返回正文重字段: %+v", list.Items[0])
	}
	found := false
	for _, it := range list.Items {
		if it.UserID == 1001 && it.Username == "爱丽丝" {
			found = true
		}
	}
	if !found {
		t.Fatalf("username 应回填 display_name: %+v", list.Items)
	}

	if list, _ = svc.ListLLMCalls(0, "qa", "", "", 1, 10); list.Total != 2 {
		t.Fatalf("按模块筛选应 2 条: %d", list.Total)
	}
	if list, _ = svc.ListLLMCalls(1001, "qa", "", "", 1, 10); list.Total != 1 {
		t.Fatalf("用户+模块组合应 1 条: %d", list.Total)
	}
	if list, _ = svc.ListLLMCalls(0, "", model.LLMCallStatusError, "", 1, 10); list.Total != 1 || list.Items[0].ErrorMsg != "boom" {
		t.Fatalf("按状态筛选应 1 条 error: %+v", list)
	}
	if list, _ = svc.ListLLMCalls(0, "", "", "", 2, 1); list.Total != 3 || len(list.Items) != 1 {
		t.Fatalf("分页 total 应 3、页内 1 条: total=%d items=%d", list.Total, len(list.Items))
	}

	detail, err := svc.GetLLMCall(rows[0].ID)
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	if detail.RequestBody != "req-a" || detail.ResponseBody != "resp-a" || detail.Username != "爱丽丝" {
		t.Fatalf("详情应带全文与用户名: %+v", detail)
	}
	if _, err := svc.GetLLMCall(999999); err == nil {
		t.Fatal("不存在的 id 应报错")
	}
}

// TestLLMCallLog_Cleanup 90 天清理边界：91 天前的删、今天的留。
func TestLLMCallLog_Cleanup(t *testing.T) {
	setupTestDB(t)
	resetLLMCallLogs(t)

	old := model.LLMCallLog{UserID: 1, Module: "qa", Status: model.LLMCallStatusSuccess, CreatedAt: time.Now().Add(-91 * 24 * time.Hour)}
	fresh := model.LLMCallLog{UserID: 1, Module: "qa", Status: model.LLMCallStatusSuccess}
	if err := common.DB.Create(&old).Error; err != nil {
		t.Fatalf("造旧行失败: %v", err)
	}
	if err := common.DB.Create(&fresh).Error; err != nil {
		t.Fatalf("造新行失败: %v", err)
	}

	n, err := cleanupLLMCallLogsBefore(time.Now().Add(-llmCallRetention))
	if err != nil || n != 1 {
		t.Fatalf("应恰好清理 1 条: n=%d err=%v", n, err)
	}
	var cnt int64
	common.DB.Model(&model.LLMCallLog{}).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("清理后应剩 1 条: %d", cnt)
	}
	var left model.LLMCallLog
	common.DB.First(&left)
	if left.ID != fresh.ID {
		t.Fatalf("留下的应是新行: %+v", left)
	}
}

// TestTruncateAuditText UTF-8 安全截断：不产生半个多字节字符，超限带截断标记。
func TestTruncateAuditText(t *testing.T) {
	if got := truncateAuditText("short", 100); got != "short" {
		t.Fatalf("未超限不应截断: %q", got)
	}
	long := strings.Repeat("汉", 100)
	got := truncateAuditText(long, 64)
	if len(got) > 64 || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("应截断到限长并带标记: len=%d %q", len(got), got)
	}
	if !strings.HasPrefix(got, "汉") || strings.ContainsRune(got, '�') {
		t.Fatalf("截断不应产生坏字符: %q", got)
	}
}
